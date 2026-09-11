package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bughatti/transcoder/internal/config"
	"github.com/bughatti/transcoder/internal/db"
)

type nudge struct{ woke, scanned int }

func (n *nudge) Wake()        { n.woke++ }
func (n *nudge) RequestScan() { n.scanned++ }

func newTestServer(t *testing.T, apiKey string) (*httptest.Server, *db.Store, *nudge) {
	t.Helper()
	ctx := context.Background()
	store, err := db.Open(ctx, db.SQLite, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Mode = "all"
	cfg.Web.APIKey = apiKey
	cfg.LogDir = ""
	cfg.Libraries = []config.Library{{Name: "Movies", Path: "/media/movies", Profile: "default", Priority: 5}}
	n := &nudge{}
	srv := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)), n)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close(); store.Close() })
	return ts, store, n
}

func get(t *testing.T, url string, v any) int {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if v != nil {
		_ = json.NewDecoder(r.Body).Decode(v)
	}
	return r.StatusCode
}

func post(t *testing.T, url, key, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(r.Body).Decode(&out)
	return r.StatusCode, out
}

func TestDashboardAndStatus(t *testing.T) {
	ts, _, _ := newTestServer(t, "")
	r, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || !strings.Contains(string(body), "<title>Transcoder</title>") {
		t.Fatalf("dashboard: %d", r.StatusCode)
	}
	if strings.Contains(string(body), "cdn.") || strings.Contains(string(body), "http://") {
		t.Error("dashboard must be self-contained (no external assets)")
	}
	var st map[string]any
	if code := get(t, ts.URL+"/api/status", &st); code != 200 {
		t.Fatalf("status %d", code)
	}
	if st["database"] != "sqlite" || st["auth_required"] != false || st["worker_online"] != false {
		t.Errorf("status: %v", st)
	}
	if code := get(t, ts.URL+"/healthz", nil); code != 200 {
		t.Error("healthz")
	}
	var stats map[string]any
	if code := get(t, ts.URL+"/api/stats", &stats); code != 200 || stats["daily"] == nil {
		t.Errorf("stats %d %v", code, stats)
	}
	var lines map[string]any
	if code := get(t, ts.URL+"/api/logs", &lines); code != 200 {
		t.Error("logs")
	}
}

func TestAuthAndControls(t *testing.T) {
	ts, store, n := newTestServer(t, "sekrit")
	if code, _ := post(t, ts.URL+"/api/control/pause", "", ""); code != 401 {
		t.Errorf("pause without key = %d", code)
	}
	if code, _ := post(t, ts.URL+"/api/control/pause", "wrong", ""); code != 401 {
		t.Errorf("pause with wrong key = %d", code)
	}
	if code, _ := post(t, ts.URL+"/api/control/pause", "sekrit", ""); code != 200 {
		t.Errorf("pause with key = %d", code)
	}
	var st map[string]any
	get(t, ts.URL+"/api/status", &st)
	if st["paused"] != true || st["auth_required"] != true {
		t.Errorf("status after pause: %v", st)
	}
	if code, _ := post(t, ts.URL+"/api/control/resume?api_key=sekrit", "", ""); code != 200 {
		t.Error("query-string key should work")
	}
	if code, _ := post(t, ts.URL+"/api/control/scan", "sekrit", ""); code != 200 || n.scanned != 1 {
		t.Errorf("scan: %d %d", code, n.scanned)
	}
	// webhook falls back to the API key
	code, out := post(t, ts.URL+"/api/webhook/sonarr", "sekrit", `{"eventType":"Test"}`)
	if code != 200 || out["test"] != true {
		t.Errorf("webhook test: %d %v", code, out)
	}
	code, out = post(t, ts.URL+"/api/webhook/radarr?key=sekrit", "", `{"eventType":"Download","movieFile":{"path":"/media/other/x.mkv"}}`)
	if code != 200 || out["queued"] != false {
		t.Errorf("webhook outside library should be ignored: %d %v", code, out)
	}
	// add job for a path outside the libraries is rejected
	if code, out := post(t, ts.URL+"/api/jobs", "sekrit", `{"path":"/nope/x.mkv"}`); code != 400 || out["error"] == nil {
		t.Errorf("add outside library: %d %v", code, out)
	}
	// cancel/retry/delete round trip on a seeded job
	id, _ := store.AddJob(context.Background(), db.NewJob{FilePath: "/media/movies/a.mkv", Library: "Movies", OriginalSize: 10})
	code, out = post(t, ts.URL+"/api/jobs/1/cancel", "sekrit", "")
	if code != 200 || out["result"] != "cancelled" || id != 1 {
		t.Errorf("cancel: %d %v", code, out)
	}
	if code, _ := post(t, ts.URL+"/api/jobs/1/retry", "sekrit", ""); code != 200 || n.woke == 0 {
		t.Errorf("retry: %d woke=%d", code, n.woke)
	}
	var jobs []map[string]any
	get(t, ts.URL+"/api/jobs?status=queued", &jobs)
	if len(jobs) != 1 || jobs[0]["file_name"] != "a.mkv" {
		t.Errorf("queued jobs: %v", jobs)
	}
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/jobs/1", nil)
	req.Header.Set("X-API-Key", "sekrit")
	r, _ := http.DefaultClient.Do(req)
	if r.StatusCode != 200 {
		t.Errorf("delete: %d", r.StatusCode)
	}
	r.Body.Close()
	if code := get(t, ts.URL+"/api/jobs/1", nil); code != 404 {
		t.Errorf("deleted job should 404, got %d", code)
	}
}
