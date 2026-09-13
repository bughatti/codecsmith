// Package worker runs the background loops: library scanning, job dispatch,
// metrics, maintenance, and (for hardware backends) an encoder health
// check. Multiple workers can share a Postgres database; each claims jobs
// atomically.
package worker

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/bughatti/codecsmith/internal/config"
	"github.com/bughatti/codecsmith/internal/db"
	"github.com/bughatti/codecsmith/internal/encoder"
	"github.com/bughatti/codecsmith/internal/integrations"
	"github.com/bughatti/codecsmith/internal/job"
	"github.com/bughatti/codecsmith/internal/scanner"
	"github.com/bughatti/codecsmith/internal/transcoder"
)

// State keys in system_state shared with the web process.
const (
	StatePaused        = "worker_paused"
	StateScanRequested = "scan_requested"
	StateHeartbeat     = "worker_heartbeat"
	StateEncoder       = "worker_encoder"
	StateLastScan      = "last_scan"
	StateGPU           = "worker_gpu"
)

// HeartbeatFile is touched every few seconds by a live worker so a container
// health check can verify the worker without a database round trip.
func HeartbeatFile(dataDir string) string { return filepath.Join(dataDir, "worker.heartbeat") }

// Worker owns the loops.
type Worker struct {
	cfg   *config.Config
	store *db.Store
	log   *slog.Logger
	xc    *transcoder.Transcoder
	scan  *scanner.Scanner

	wake    chan struct{}
	scanNow chan struct{}
	pauseMu sync.RWMutex
	paused  bool
	running sync.WaitGroup
}

// New detects the encoder and builds a Worker.
func New(ctx context.Context, cfg *config.Config, store *db.Store, log *slog.Logger) (*Worker, error) {
	enc, err := encoder.New(ctx, cfg.Encoder.Backend, cfg.Encoder.Codec, encoder.Options{
		FFmpeg: cfg.Encoder.FFmpeg, VAAPIDevice: cfg.Encoder.VAAPIDevice,
	})
	if err != nil {
		return nil, err
	}
	log.Info("encoder selected", "backend", enc.Name(), "codec", cfg.Encoder.Codec, "encoder", enc.EncoderName(cfg.Encoder.Codec))
	w := &Worker{
		cfg: cfg, store: store, log: log,
		xc:      transcoder.New(cfg, enc, log.With("subsys", "ffmpeg")),
		scan:    scanner.New(cfg, store, log.With("subsys", "scanner")),
		wake:    make(chan struct{}, 1),
		scanNow: make(chan struct{}, 1),
	}
	return w, nil
}

// Wake nudges the dispatcher (used in-process by the web server when the
// database has no NOTIFY support).
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// RequestScan triggers an immediate library scan.
func (w *Worker) RequestScan() {
	select {
	case w.scanNow <- struct{}{}:
	default:
	}
}

// Encoder reports the active backend.
func (w *Worker) Encoder() encoder.Encoder { return w.xc.Encoder() }

// Run blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	w.log.Info("worker start", "id", w.cfg.Worker.ID, "max_concurrent", w.cfg.Worker.MaxConcurrent, "libraries", len(w.cfg.Libraries))
	_ = os.MkdirAll(w.cfg.Worker.TempDir, 0o755)

	// Jobs this worker was running when it last died go to the front.
	if n, err := w.store.ReclaimWorkerJobs(ctx, w.cfg.Worker.ID); err == nil && n > 0 {
		w.log.Warn("re-queued interrupted jobs from previous run", "count", n)
	}
	if v, _ := w.store.GetState(ctx, StatePaused); v == "true" {
		w.setPaused(true)
		w.log.Info("starting paused (per saved state)")
	}
	_ = w.store.SetState(ctx, StateEncoder, w.Encoder().Name()+"/"+w.Encoder().EncoderName(w.cfg.Encoder.Codec))

	loops := []struct {
		name string
		fn   func(context.Context)
	}{
		{"dispatch", w.dispatchLoop},
		{"scan", w.scanLoop},
		{"control", w.controlLoop},
		{"listen", w.listenLoop},
		{"metrics", w.metricsLoop},
		{"storage", w.storageLoop},
		{"maintenance", w.maintenanceLoop},
		{"downloads", w.downloadsLoop},
		{"encoder-health", w.encoderHealthLoop},
	}
	var wg sync.WaitGroup
	for _, l := range loops {
		wg.Add(1)
		go func(name string, fn func(context.Context)) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					w.log.Error("loop panicked", "loop", name, "panic", r)
				}
			}()
			fn(ctx)
		}(l.name, l.fn)
	}
	<-ctx.Done()
	w.log.Info("shutting down; waiting for loops")
	wg.Wait()
	w.running.Wait()
	return nil
}

// -----------------------------------------------------------------------------
// dispatch
// -----------------------------------------------------------------------------

func (w *Worker) dispatchLoop(ctx context.Context) {
	poll := 30 * time.Second
	if !w.store.SupportsListen() {
		poll = 10 * time.Second
	}
	for {
		if ctx.Err() != nil {
			return
		}
		claimed := false
		if !w.isPaused() {
			claimed = w.dispatchOnce(ctx)
		}
		if claimed {
			continue // keep filling slots
		}
		select {
		case <-w.wake:
		case <-time.After(poll):
		case <-ctx.Done():
			return
		}
	}
}

// dispatchOnce claims and starts at most one job. Returns true if it did.
func (w *Worker) dispatchOnce(ctx context.Context) bool {
	perLib, total, err := w.store.RunningByLibrary(ctx)
	if err != nil {
		w.log.Warn("count running", "err", err)
		return false
	}
	if total >= w.cfg.Worker.MaxConcurrent {
		return false
	}
	var eligible []string
	for _, lib := range w.cfg.Libraries {
		if !lib.IsEnabled() {
			continue
		}
		cap := lib.MaxConcurrent
		if cap <= 0 {
			cap = w.cfg.Worker.MaxConcurrent
		}
		if perLib[lib.Name] < cap {
			eligible = append(eligible, lib.Name)
		}
	}
	if len(eligible) == 0 {
		return false
	}
	j, err := w.store.ClaimNextJob(ctx, w.cfg.Worker.ID, eligible)
	if err != nil {
		w.log.Warn("claim failed", "err", err)
		return false
	}
	if j == nil {
		return false
	}
	w.running.Add(1)
	go func() {
		defer w.running.Done()
		w.processJob(ctx, j)
		w.Wake()
	}()
	return true
}

func (w *Worker) processJob(ctx context.Context, j *job.Job) {
	log := w.log.With("job_id", j.ID, "library", j.Library)
	log.Info("job start", "file", j.FilePath, "size", j.OriginalSize, "codec", j.OriginalCodec)

	running := job.StatusRunning
	_ = w.store.UpdateJob(ctx, j.ID, db.Update{Status: &running})

	lastCancelCheck := time.Now()
	result, err := w.xc.Transcode(ctx, j, func(p transcoder.Progress) bool {
		pct, speed, fps, eta := p.Percent, p.Speed, p.FPS, p.ETASeconds
		_ = w.store.UpdateJob(ctx, j.ID, db.Update{Progress: &pct, Speed: &speed, FPS: &fps, ETASeconds: &eta})
		if time.Since(lastCancelCheck) > 3*time.Second {
			lastCancelCheck = time.Now()
			if w.store.CancelRequested(ctx, j.ID) {
				return false
			}
		}
		return true
	})

	// Process shutdown mid-job: leave it 'interrupted' for the next run.
	if ctx.Err() != nil {
		interrupted := job.StatusInterrupted
		msg := "worker shut down mid-job"
		_ = w.store.UpdateJob(context.Background(), j.ID, db.Update{Status: &interrupted, ErrorMessage: &msg})
		log.Warn("job interrupted by shutdown")
		return
	}

	if result == nil {
		result = &Result{}
	}
	u := db.Update{Status: &result.Status, Completed: true}
	if result.NewSize > 0 {
		u.NewSize = &result.NewSize
	}
	if result.TargetCodec != "" {
		u.TargetCodec = &result.TargetCodec
	}
	msg := result.Message
	if err != nil && msg == "" {
		msg = err.Error()
	}
	if msg != "" {
		u.ErrorMessage = &msg
	}
	zero := 0.0
	u.Speed, u.FPS = &zero, &zero
	if result.Status == job.StatusCompleted {
		hundred := 100.0
		u.Progress = &hundred
	}
	_ = w.store.UpdateJob(ctx, j.ID, u)

	switch result.Status {
	case job.StatusCompleted:
		log.Info("job complete", "saved", j.OriginalSize-result.NewSize, "new_size", result.NewSize, "codec", result.TargetCodec)
	case job.StatusSkipped:
		log.Info("job skipped", "reason", msg)
	case job.StatusCancelled:
		log.Info("job cancelled")
	default:
		log.Warn("job failed", "err", msg)
	}
}

// Result aliases transcoder.Result for the nil-guard above.
type Result = transcoder.Result

// -----------------------------------------------------------------------------
// scan
// -----------------------------------------------------------------------------

func (w *Worker) scanLoop(ctx context.Context) {
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-w.scanNow:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		sum, err := w.scan.ScanAll(ctx)
		if err != nil {
			w.log.Warn("scan failed", "err", err)
		} else {
			w.log.Info("scan finished", "libraries", sum.Libraries, "candidates", sum.Seen, "queued", sum.Added, "took", sum.Elapsed.Round(time.Second))
			_ = w.store.SetState(ctx, StateLastScan, time.Now().UTC().Format(time.RFC3339))
			if sum.Added > 0 {
				w.Wake()
			}
		}
		timer.Reset(w.cfg.Worker.ScanInterval)
	}
}

// -----------------------------------------------------------------------------
// control: pause/scan requests written by the web process + heartbeat
// -----------------------------------------------------------------------------

func (w *Worker) controlLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	lastScanReq := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		paused, _ := w.store.GetState(ctx, StatePaused)
		if (paused == "true") != w.isPaused() {
			w.setPaused(paused == "true")
			w.log.Info("pause state changed", "paused", paused == "true")
			if paused != "true" {
				w.Wake()
			}
		}
		if req, _ := w.store.GetState(ctx, StateScanRequested); req != "" && req != lastScanReq {
			lastScanReq = req
			w.RequestScan()
		}
		_ = w.store.SetState(ctx, StateHeartbeat, strconv.FormatInt(time.Now().Unix(), 10))
		_ = os.WriteFile(HeartbeatFile(w.cfg.DataDir), []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0o644)
		// Cancel requests for running jobs (in case the progress callback is
		// slow, e.g. ffmpeg stalled).
		if pending, err := w.store.ListJobs(ctx, []job.Status{job.StatusRunning, job.StatusProcessing}, 100); err == nil {
			for _, j := range pending {
				if j.CancelRequested && j.WorkerID == w.cfg.Worker.ID {
					w.xc.Kill(j.ID)
				}
			}
		}
	}
}

func (w *Worker) listenLoop(ctx context.Context) {
	if !w.store.SupportsListen() {
		<-ctx.Done()
		return
	}
	backoff := time.Second
	for ctx.Err() == nil {
		err := w.store.Listen(ctx, func(string) { w.Wake() })
		if ctx.Err() != nil {
			return
		}
		w.log.Warn("notify listener dropped; reconnecting", "err", err, "in", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// -----------------------------------------------------------------------------
// metrics / storage / maintenance / downloads
// -----------------------------------------------------------------------------

func (w *Worker) metricsLoop(ctx context.Context) {
	t := time.NewTicker(w.cfg.Worker.MetricsInterval)
	defer t.Stop()
	gpuAnnounced := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cpu, mem, disk := integrations.SystemMetrics(w.cfg.Worker.TempDir)
		counts, _ := w.store.CountByStatus(ctx)
		m := db.Metric{CPU: cpu, Memory: mem, Disk: disk,
			Active: counts[job.StatusRunning] + counts[job.StatusProcessing],
			Queued: counts[job.StatusQueued] + counts[job.StatusInterrupted]}
		if g, ok := integrations.SampleGPU(ctx); ok {
			util, memPct := g.Util, g.MemPercent()
			m.GPU, m.GPUMemory = &util, &memPct
			if g.Encoder >= 0 {
				enc := g.Encoder
				m.GPUEncoder = &enc
			}
			if !gpuAnnounced {
				gpuAnnounced = true
				_ = w.store.SetState(ctx, StateGPU, g.Vendor+": "+g.Name)
				w.log.Info("gpu metrics available", "vendor", g.Vendor, "name", g.Name)
			}
		} else if !gpuAnnounced {
			gpuAnnounced = true
			_ = w.store.SetState(ctx, StateGPU, "")
		}
		if err := w.store.RecordMetric(ctx, m); err != nil {
			w.log.Warn("record metric", "err", err)
		}
		if n, err := w.store.MarkStaleJobs(ctx, w.cfg.Worker.StaleAfter); err == nil && n > 0 {
			w.log.Warn("failed stale jobs", "count", n)
		}
	}
}

func (w *Worker) storageLoop(ctx context.Context) {
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		for _, lib := range w.cfg.Libraries {
			wctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			st, err := integrations.DirStats(wctx, lib.Path)
			cancel()
			if err != nil && st.Total == 0 {
				w.log.Debug("storage stats", "library", lib.Name, "err", err)
				continue
			}
			_ = w.store.RecordStorage(ctx, st)
		}
		timer.Reset(6 * time.Hour)
	}
}

func (w *Worker) maintenanceLoop(ctx context.Context) {
	timer := time.NewTimer(5 * time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		w.runMaintenance(ctx)
		timer.Reset(24 * time.Hour)
	}
}

func (w *Worker) runMaintenance(ctx context.Context) {
	log := w.log.With("subsys", "maintenance")
	// Pending jobs whose file vanished (deleted/renamed by the library manager).
	if paths, err := w.store.PendingPaths(ctx); err == nil {
		gone := 0
		for id, p := range paths {
			if _, err := os.Stat(p); err != nil {
				_ = w.store.FailJob(ctx, id, "file no longer exists")
				gone++
			}
		}
		if gone > 0 {
			log.Info("failed jobs whose files are gone", "count", gone)
		}
	}
	cutoff := time.Now().Add(-w.cfg.Worker.MetricsRetention)
	_, _ = w.store.PruneMetrics(ctx, cutoff)
	_, _ = w.store.PruneStorage(ctx, time.Now().Add(-90*24*time.Hour))
	// Trashed originals past their retention.
	if dir := w.cfg.Worker.TrashDir; dir != "" && w.cfg.Worker.TrashRetention > 0 {
		cutoff := time.Now().Add(-w.cfg.Worker.TrashRetention)
		removed := 0
		_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if info.ModTime().Before(cutoff) && os.Remove(p) == nil {
				removed++
			}
			return nil
		})
		if removed > 0 {
			log.Info("pruned trashed originals", "count", removed, "older_than", w.cfg.Worker.TrashRetention.String())
		}
	}

	// Leftover temp files from crashed runs.
	if entries, err := os.ReadDir(w.cfg.Worker.TempDir); err == nil {
		for _, e := range entries {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
				_ = os.Remove(w.cfg.Worker.TempDir + "/" + e.Name())
			}
		}
	}
	log.Info("maintenance complete")
}

func (w *Worker) downloadsLoop(ctx context.Context) {
	sab := w.cfg.Integrations.SABnzbd
	if !sab.Enabled || sab.URL == "" {
		<-ctx.Done()
		return
	}
	client := integrations.NewSABnzbd(sab.URL, sab.APIKey, w.log.With("subsys", "sabnzbd"))
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := client.Sync(ctx, w.store); err != nil {
				w.log.Debug("sabnzbd sync", "err", err)
			}
		}
	}
}

// -----------------------------------------------------------------------------
// encoder health: hardware backends can silently break (driver upgrade
// under a running container, GPU reset). Encoding a null frame every few
// minutes catches it; after repeated failures the process exits non-zero so
// the supervisor restarts it with fresh driver libraries.
// -----------------------------------------------------------------------------

func (w *Worker) encoderHealthLoop(ctx context.Context) {
	if !w.cfg.Worker.GPUHealthCheck || w.Encoder().Name() == encoder.Software {
		<-ctx.Done()
		return
	}
	log := w.log.With("subsys", "encoder-health")
	timer := time.NewTimer(90 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		ok := false
		for attempt := 1; attempt <= 3; attempt++ {
			if err := w.Encoder().Probe(ctx, w.cfg.Encoder.FFmpeg); err == nil {
				ok = true
				break
			} else {
				log.Warn("encoder probe failed", "attempt", attempt, "err", err)
			}
			select {
			case <-time.After(10 * time.Second):
			case <-ctx.Done():
				return
			}
		}
		if !ok {
			log.Error("encoder unhealthy after 3 probes; exiting so the supervisor restarts the process")
			os.Exit(2)
		}
		timer.Reset(5 * time.Minute)
	}
}

// -----------------------------------------------------------------------------
// pause state
// -----------------------------------------------------------------------------

func (w *Worker) isPaused() bool {
	w.pauseMu.RLock()
	defer w.pauseMu.RUnlock()
	return w.paused
}

func (w *Worker) setPaused(p bool) {
	w.pauseMu.Lock()
	w.paused = p
	w.pauseMu.Unlock()
}
