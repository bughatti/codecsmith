// Package web serves the dashboard and the JSON API.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/bughatti/transcoder/internal/config"
	"github.com/bughatti/transcoder/internal/db"
	"github.com/bughatti/transcoder/internal/job"
	"github.com/bughatti/transcoder/internal/logger"
	"github.com/bughatti/transcoder/internal/scanner"
	"github.com/bughatti/transcoder/internal/transcoder"
	"github.com/bughatti/transcoder/internal/version"
	"github.com/bughatti/transcoder/internal/worker"
)

//go:embed templates/index.html
var embedded embed.FS

// Nudger is the in-process worker hook used in --mode=all so the queue
// reacts instantly without a database round trip.
type Nudger interface {
	Wake()
	RequestScan()
}

// Server is the HTTP front end.
type Server struct {
	cfg   *config.Config
	store *db.Store
	log   *slog.Logger
	nudge Nudger
	start time.Time
}

// New builds a server; nudge may be nil (separate worker process).
func New(cfg *config.Config, store *db.Store, log *slog.Logger, nudge Nudger) *Server {
	return &Server{cfg: cfg, store: store, log: log, nudge: nudge, start: time.Now()}
}

// Handler builds the router (exported for tests).
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)
	r.Use(s.logRequest)
	r.Use(middleware.Compress(5, "application/json", "text/html"))

	r.Get("/", s.index)
	r.Get("/healthz", s.healthz)

	r.Route("/api", func(r chi.Router) {
		// read
		r.Get("/status", s.status)
		r.Get("/config", s.configView)
		r.Get("/jobs", s.listJobs)
		r.Get("/jobs/{id}", s.getJob)
		r.Get("/stats", s.stats)
		r.Get("/metrics", s.metrics)
		r.Get("/storage", s.storage)
		r.Get("/downloads", s.downloads)
		r.Get("/logs", s.logs)
		r.Get("/failures", s.failures)

		// write (API key when configured)
		r.Group(func(r chi.Router) {
			r.Use(s.requireKey(s.cfg.Web.APIKey))
			r.Post("/jobs", s.addJob)
			r.Post("/jobs/{id}/cancel", s.cancelJob)
			r.Post("/jobs/{id}/retry", s.retryJob)
			r.Delete("/jobs/{id}", s.deleteJob)
			r.Post("/control/pause", s.pause)
			r.Post("/control/resume", s.resume)
			r.Post("/control/scan", s.scanNow)
			r.Post("/control/retry-failed", s.retryFailed)
		})

		// webhooks (Sonarr/Radarr/anything): queue the file they just imported
		r.Group(func(r chi.Router) {
			secret := s.cfg.Integrations.WebhookSecret
			if secret == "" {
				secret = s.cfg.Web.APIKey
			}
			r.Use(s.requireKey(secret))
			r.Post("/webhook/sonarr", s.webhookArr)
			r.Post("/webhook/radarr", s.webhookArr)
			r.Post("/webhook", s.webhookArr)
		})
	})
	return r
}

// Run serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Web.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	s.log.Info("web listening", "addr", s.cfg.Web.Listen, "auth", s.cfg.Web.APIKey != "")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// -----------------------------------------------------------------------------
// middleware
// -----------------------------------------------------------------------------

func (s *Server) logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		if r.Method != http.MethodGet || ww.Status() >= 400 {
			s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", ww.Status(), "ms", time.Since(start).Milliseconds())
		}
	})
}

// requireKey enforces X-API-Key / ?api_key= when key is non-empty.
func (s *Server) requireKey(key string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if key == "" {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := r.Header.Get("X-API-Key")
			if got == "" {
				got = r.URL.Query().Get("api_key")
			}
			if got == "" {
				got = r.URL.Query().Get("key")
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
				writeErr(w, http.StatusUnauthorized, errors.New("api key required"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// -----------------------------------------------------------------------------
// pages
// -----------------------------------------------------------------------------

func (s *Server) index(w http.ResponseWriter, _ *http.Request) {
	html, err := fs.ReadFile(embedded, "templates/index.html")
	if err != nil {
		http.Error(w, "dashboard missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(html)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		http.Error(w, "db unreachable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok"))
}

// -----------------------------------------------------------------------------
// read API
// -----------------------------------------------------------------------------

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	paused, _ := s.store.GetState(ctx, worker.StatePaused)
	hb, _ := s.store.GetState(ctx, worker.StateHeartbeat)
	enc, _ := s.store.GetState(ctx, worker.StateEncoder)
	lastScan, _ := s.store.GetState(ctx, worker.StateLastScan)
	gpu, _ := s.store.GetState(ctx, worker.StateGPU)
	workerOnline := false
	var workerSeen *time.Time
	if ts, err := strconv.ParseInt(hb, 10, 64); err == nil {
		t := time.Unix(ts, 0).UTC()
		workerSeen = &t
		workerOnline = time.Since(t) < 30*time.Second
	}
	counts, _ := s.store.CountByStatus(ctx)
	libs := make([]map[string]any, 0, len(s.cfg.Libraries))
	for _, l := range s.cfg.Libraries {
		libs = append(libs, map[string]any{
			"name": l.Name, "path": l.Path, "profile": l.Profile, "priority": l.Priority,
			"enabled": l.IsEnabled(), "codec": s.cfg.TargetCodec(s.cfg.ProfileFor(l)),
		})
	}
	writeJSON(w, map[string]any{
		"title":          s.cfg.Web.Title,
		"version":        version.String(),
		"paused":         paused == "true",
		"worker_online":  workerOnline,
		"worker_seen":    workerSeen,
		"encoder":        enc,
		"gpu":            gpu,
		"last_scan":      lastScan,
		"auth_required":  s.cfg.Web.APIKey != "",
		"database":       string(s.store.Driver()),
		"uptime_seconds": int(time.Since(s.start).Seconds()),
		"counts":         counts,
		"libraries":      libs,
	})
}

func (s *Server) configView(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.cfg.Redacted())
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var statuses []job.Status
	switch q.Get("status") {
	case "", "active":
		statuses = []job.Status{job.StatusRunning, job.StatusProcessing}
	case "queued", "pending":
		statuses = []job.Status{job.StatusQueued, job.StatusInterrupted}
	case "done":
		statuses = job.Terminal
	default:
		for _, st := range strings.Split(q.Get("status"), ",") {
			statuses = append(statuses, job.Status(strings.TrimSpace(st)))
		}
	}
	jobs, err := s.store.ListJobs(r.Context(), statuses, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, jobsJSON(jobs))
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	j, err := s.store.GetJob(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if j == nil {
		writeErr(w, http.StatusNotFound, errors.New("job not found"))
		return
	}
	writeJSON(w, jobJSON(*j))
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	all, _ := s.store.Stats(ctx, time.Time{})
	month, _ := s.store.Stats(ctx, time.Now().Add(-30*24*time.Hour))
	day, _ := s.store.Stats(ctx, time.Now().Add(-24*time.Hour))
	byLib, _ := s.store.StatsByLibrary(ctx)
	daily, _ := s.store.DailySaved(ctx, 30)
	writeJSON(w, map[string]any{
		"all_time":  statsJSON(all),
		"last_30d":  statsJSON(month),
		"last_24h":  statsJSON(day),
		"libraries": byLib,
		"daily":     daily,
	})
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	mins, _ := strconv.Atoi(r.URL.Query().Get("minutes"))
	if mins <= 0 || mins > 24*60 {
		mins = 60
	}
	m, err := s.store.Metrics(r.Context(), time.Now().Add(-time.Duration(mins)*time.Minute))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, m)
}

func (s *Server) storage(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.LatestStorage(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	type row struct {
		db.StorageStat
		Library string `json:"library"`
	}
	out := make([]row, 0, len(st))
	for _, x := range st {
		name := ""
		if l, ok := s.cfg.LibraryFor(x.Path); ok {
			name = l.Name
		}
		out = append(out, row{x, name})
	}
	writeJSON(w, out)
}

func (s *Server) downloads(w http.ResponseWriter, r *http.Request) {
	active, recent, err := s.store.Downloads(r.Context(), 20)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]any{"enabled": s.cfg.Integrations.SABnzbd.Enabled, "active": active, "recent": recent})
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	if n <= 0 || n > 2000 {
		n = 200
	}
	lines, err := logger.Tail(s.cfg.LogDir, n)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]any{"lines": lines})
}

func (s *Server) failures(w http.ResponseWriter, r *http.Request) {
	common, total, err := s.store.FailureSummary(r.Context(), 8)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]any{"total": total, "common": common})
}

// -----------------------------------------------------------------------------
// write API
// -----------------------------------------------------------------------------

type addJobRequest struct {
	Path     string `json:"path"`
	Library  string `json:"library"`
	Priority *int   `json:"priority"`
}

// addJob queues a single file. The path must live inside a configured
// library so the worker knows which profile applies.
func (s *Server) addJob(w http.ResponseWriter, r *http.Request) {
	var req addJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	id, err := s.enqueue(r.Context(), req.Path, req.Library, req.Priority)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": id, "queued": id > 0})
}

func (s *Server) enqueue(ctx context.Context, path, libName string, priority *int) (int64, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return 0, errors.New("path must be absolute")
	}
	var lib config.Library
	var ok bool
	if libName != "" {
		if lib, ok = s.cfg.LibraryByName(libName); !ok {
			return 0, fmt.Errorf("unknown library %q", libName)
		}
		if _, inside := s.cfg.LibraryFor(path); !inside {
			return 0, errors.New("path is outside every configured library")
		}
	} else if lib, ok = s.cfg.LibraryFor(path); !ok {
		return 0, errors.New("path is outside every configured library")
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("stat: %w", err)
	}
	if !scanner.VideoExtensions[strings.ToLower(filepath.Ext(path))] {
		return 0, errors.New("not a video file extension")
	}
	codec, err := transcoder.ProbeCodec(ctx, s.cfg.Encoder.FFprobe, path)
	if err != nil {
		return 0, fmt.Errorf("ffprobe: %w", err)
	}
	prio := lib.Priority
	if priority != nil {
		prio = *priority
	}
	// A manual add always queues, even if the scanner would have skipped it,
	// but a path already in the table is reset to queued instead of duplicated.
	id, err := s.store.AddJob(ctx, db.NewJob{FilePath: path, Library: lib.Name, Profile: lib.Profile,
		OriginalSize: info.Size(), OriginalCodec: codec, Priority: prio})
	if err != nil {
		return 0, err
	}
	if id == 0 {
		// existing row → requeue
		rows, _ := s.store.ListJobs(ctx, append(append([]job.Status{}, job.Terminal...), job.Pending...), 100000)
		for _, j := range rows {
			if j.FilePath == path {
				_, _ = s.store.RequeueJob(ctx, j.ID)
				id = j.ID
				break
			}
		}
	}
	if s.nudge != nil {
		s.nudge.Wake()
	}
	return id, nil
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	res, err := s.store.RequestCancel(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if res == "" {
		writeErr(w, http.StatusConflict, errors.New("job is not queued or running"))
		return
	}
	writeJSON(w, map[string]any{"ok": true, "result": res})
}

func (s *Server) retryJob(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	done, err := s.store.RequeueJob(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if s.nudge != nil {
		s.nudge.Wake()
	}
	writeJSON(w, map[string]any{"ok": done})
}

func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	done, err := s.store.DeleteJob(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !done {
		writeErr(w, http.StatusConflict, errors.New("running jobs must be cancelled first"))
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) pause(w http.ResponseWriter, r *http.Request) {
	if err := s.store.SetState(r.Context(), worker.StatePaused, "true"); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "paused": true})
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	if err := s.store.SetState(r.Context(), worker.StatePaused, "false"); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if s.nudge != nil {
		s.nudge.Wake()
	}
	writeJSON(w, map[string]any{"ok": true, "paused": false})
}

func (s *Server) scanNow(w http.ResponseWriter, r *http.Request) {
	if err := s.store.SetState(r.Context(), worker.StateScanRequested, strconv.FormatInt(time.Now().UnixNano(), 10)); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if s.nudge != nil {
		s.nudge.RequestScan()
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) retryFailed(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 25
	}
	n, err := s.store.RequeueFailed(r.Context(), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if s.nudge != nil {
		s.nudge.Wake()
	}
	writeJSON(w, map[string]any{"ok": true, "requeued": n})
}

// -----------------------------------------------------------------------------
// webhooks: Sonarr/Radarr "On Import" payloads carry the imported file
// path; queue it right away instead of waiting for the next scan.
// -----------------------------------------------------------------------------

type arrPayload struct {
	EventType   string `json:"eventType"`
	EpisodeFile struct {
		Path string `json:"path"`
	} `json:"episodeFile"`
	MovieFile struct {
		Path string `json:"path"`
	} `json:"movieFile"`
	// Generic: {"path": "..."}
	Path string `json:"path"`
}

func (s *Server) webhookArr(w http.ResponseWriter, r *http.Request) {
	var p arrPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	path := p.Path
	if path == "" {
		path = p.EpisodeFile.Path
	}
	if path == "" {
		path = p.MovieFile.Path
	}
	if p.EventType == "Test" {
		writeJSON(w, map[string]any{"ok": true, "test": true})
		return
	}
	if path == "" || (p.EventType != "" && p.EventType != "Download" && p.EventType != "Upgrade" && p.EventType != "Import") {
		writeJSON(w, map[string]any{"ok": true, "ignored": p.EventType})
		return
	}
	id, err := s.enqueue(r.Context(), path, "", nil)
	if err != nil {
		s.log.Warn("webhook enqueue", "path", path, "err", err)
		writeJSON(w, map[string]any{"ok": true, "queued": false, "reason": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "queued": true, "id": id})
}

// -----------------------------------------------------------------------------
// JSON helpers
// -----------------------------------------------------------------------------

func idParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("bad id"))
		return 0, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func jobJSON(j job.Job) map[string]any {
	out := map[string]any{
		"id":               j.ID,
		"file_path":        j.FilePath,
		"file_name":        filepath.Base(j.FilePath),
		"library":          j.Library,
		"profile":          j.Profile,
		"status":           string(j.Status),
		"priority":         j.Priority,
		"original_size":    j.OriginalSize,
		"new_size":         j.NewSize,
		"original_codec":   j.OriginalCodec,
		"target_codec":     j.TargetCodec,
		"created_at":       j.CreatedAt.UTC().Format(time.RFC3339),
		"started_at":       tsOrNil(j.StartedAt),
		"completed_at":     tsOrNil(j.CompletedAt),
		"updated_at":       tsOrNil(j.UpdatedAt),
		"message":          j.ErrorMessage,
		"progress":         j.Progress,
		"speed":            j.Speed,
		"fps":              j.FPS,
		"eta_seconds":      j.ETASeconds,
		"worker_id":        j.WorkerID,
		"cancel_requested": j.CancelRequested,
	}
	if j.Status == job.StatusCompleted && j.OriginalSize > 0 {
		out["saved"] = j.OriginalSize - j.NewSize
	}
	return out
}

func tsOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func jobsJSON(in []job.Job) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, j := range in {
		out = append(out, jobJSON(j))
	}
	return out
}

func statsJSON(st job.Stats) map[string]any {
	return map[string]any{
		"total": st.Total, "completed": st.Completed, "failed": st.Failed, "skipped": st.Skipped,
		"cancelled": st.Cancelled, "queued": st.Queued, "running": st.Running, "saved": st.TotalSaved,
	}
}
