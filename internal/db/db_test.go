package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/bughatti/transcoder/internal/job"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, SQLite, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if v := s.SchemaVersion(ctx); v < 1 {
		t.Fatalf("schema version %d", v)
	}
	// second run must be a no-op
	if applied, err := s.Migrate(ctx); err != nil || len(applied) != 0 {
		t.Fatalf("re-migrate: %v %v", applied, err)
	}
	return s
}

func TestJobLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	id, err := s.AddJob(ctx, NewJob{FilePath: "/m/a.mkv", Library: "Movies", Profile: "default", OriginalSize: 1000, OriginalCodec: "h264", Priority: 5})
	if err != nil || id == 0 {
		t.Fatalf("add: %d %v", id, err)
	}
	if dup, err := s.AddJob(ctx, NewJob{FilePath: "/m/a.mkv", Library: "Movies"}); err != nil || dup != 0 {
		t.Fatalf("duplicate should return 0: %d %v", dup, err)
	}
	id2, _ := s.AddJob(ctx, NewJob{FilePath: "/m/b.mkv", Library: "Movies", OriginalSize: 500, Priority: 9})
	_, _ = s.AddJob(ctx, NewJob{FilePath: "/s/c.mkv", Library: "Shows", OriginalSize: 500, Priority: 1})

	// highest priority first, only from eligible libraries
	j, err := s.ClaimNextJob(ctx, "w1", []string{"Movies"})
	if err != nil || j == nil || j.ID != id2 {
		t.Fatalf("claim: %+v %v", j, err)
	}
	if j.Status != job.StatusProcessing || j.WorkerID != "w1" || j.StartedAt == nil {
		t.Errorf("claimed job state: %+v", j)
	}
	per, total, _ := s.RunningByLibrary(ctx)
	if per["Movies"] != 1 || total != 1 {
		t.Errorf("running: %v %d", per, total)
	}

	// progress + completion
	run := job.StatusRunning
	p := 42.5
	if err := s.UpdateJob(ctx, j.ID, Update{Status: &run, Progress: &p}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetJob(ctx, j.ID)
	if got.Status != job.StatusRunning || got.Progress != 42.5 || got.UpdatedAt == nil {
		t.Errorf("update: %+v", got)
	}
	done := job.StatusCompleted
	ns := int64(300)
	tc := "hevc"
	_ = s.UpdateJob(ctx, j.ID, Update{Status: &done, NewSize: &ns, TargetCodec: &tc, Completed: true})
	st, _ := s.Stats(ctx, time.Time{})
	if st.Completed != 1 || st.TotalSaved != 200 || st.Queued != 2 {
		t.Errorf("stats: %+v", st)
	}

	// cancel queued → cancelled immediately; cancel running → flag
	if res, _ := s.RequestCancel(ctx, id); res != "cancelled" {
		t.Errorf("cancel queued: %s", res)
	}
	j3, _ := s.ClaimNextJob(ctx, "w1", []string{"Movies", "Shows"})
	if j3 == nil || j3.Library != "Shows" {
		t.Fatalf("should claim the Shows job: %+v", j3)
	}
	if res, _ := s.RequestCancel(ctx, j3.ID); res != "requested" {
		t.Errorf("cancel running: %s", res)
	}
	if !s.CancelRequested(ctx, j3.ID) {
		t.Error("cancel flag not set")
	}
	// requeue clears the flag
	if ok, _ := s.RequeueJob(ctx, j3.ID); !ok {
		t.Error("requeue")
	}
	if s.CancelRequested(ctx, j3.ID) {
		t.Error("requeue must clear cancel flag")
	}
	if nothing, _ := s.ClaimNextJob(ctx, "w1", []string{"Nope"}); nothing != nil {
		t.Error("no eligible library → nil")
	}
	byLib, _ := s.StatsByLibrary(ctx)
	if len(byLib) != 2 {
		t.Errorf("stats by library: %+v", byLib)
	}
	known, _ := s.KnownPaths(ctx)
	if len(known) != 3 {
		t.Errorf("known paths: %v", known)
	}
	if ok, _ := s.DeleteJob(ctx, id); !ok {
		t.Error("delete cancelled job")
	}
}

func TestStaleAndReclaim(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	id, _ := s.AddJob(ctx, NewJob{FilePath: "/m/a.mkv", Library: "M", OriginalSize: 1})
	j, _ := s.ClaimNextJob(ctx, "w1", []string{"M"})
	if j == nil || j.ID != id {
		t.Fatal("claim")
	}
	if n, _ := s.MarkStaleJobs(ctx, time.Hour); n != 0 {
		t.Error("fresh job must not be stale")
	}
	if n, _ := s.MarkStaleJobs(ctx, -time.Second); n != 1 {
		t.Error("job older than cutoff should be failed")
	}
	_, _ = s.RequeueJob(ctx, id)
	_, _ = s.ClaimNextJob(ctx, "w1", []string{"M"})
	if n, _ := s.ReclaimWorkerJobs(ctx, "w1"); n != 1 {
		t.Error("reclaim")
	}
	got, _ := s.GetJob(ctx, id)
	if got.Status != job.StatusInterrupted {
		t.Errorf("status %s", got.Status)
	}
	// interrupted jobs are claimed before queued ones
	_, _ = s.AddJob(ctx, NewJob{FilePath: "/m/b.mkv", Library: "M", Priority: 99})
	next, _ := s.ClaimNextJob(ctx, "w2", []string{"M"})
	if next == nil || next.ID != id {
		t.Errorf("interrupted should win over high priority queued: %+v", next)
	}
}

func TestStateMetricsStorage(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if v, _ := s.GetState(ctx, "x"); v != "" {
		t.Error("missing key should be empty")
	}
	_ = s.SetState(ctx, "x", "1")
	_ = s.SetState(ctx, "x", "2")
	if v, _ := s.GetState(ctx, "x"); v != "2" {
		t.Error("upsert")
	}
	_ = s.RecordMetric(ctx, Metric{CPU: 10, Memory: 20, Disk: 30, Active: 1, Queued: 2})
	m, _ := s.Metrics(ctx, time.Now().Add(-time.Minute))
	if len(m) != 1 || m[0].CPU != 10 || m[0].Queued != 2 || m[0].GPU != nil {
		t.Errorf("metrics: %+v", m)
	}
	g := 55.0
	_ = s.RecordMetric(ctx, Metric{CPU: 1, GPU: &g, GPUEncoder: &g})
	m, _ = s.Metrics(ctx, time.Now().Add(-time.Minute))
	if len(m) != 2 || m[1].GPU == nil || *m[1].GPU != 55 || m[1].GPUMemory != nil {
		t.Errorf("gpu metrics: %+v", m)
	}
	if n, _ := s.PruneMetrics(ctx, time.Now().Add(time.Minute)); n != 2 {
		t.Error("prune")
	}
	_ = s.RecordStorage(ctx, StorageStat{Path: "/a", Total: 100, Used: 40, Free: 60, FileCount: 3})
	_ = s.RecordStorage(ctx, StorageStat{Path: "/a", Total: 100, Used: 50, Free: 50, FileCount: 4})
	_ = s.RecordStorage(ctx, StorageStat{Path: "/b", Total: 10, Used: 1, Free: 9, FileCount: 1})
	st, _ := s.LatestStorage(ctx)
	if len(st) != 2 || st[0].Used != 50 {
		t.Errorf("latest storage: %+v", st)
	}
	_ = s.UpsertDownload(ctx, Download{Source: "sabnzbd", ExternalID: "n1", Title: "t", Status: "downloading", Progress: 5})
	_ = s.UpsertDownload(ctx, Download{Source: "sabnzbd", ExternalID: "n1", Title: "t", Status: "downloading", Progress: 50})
	active, recent, _ := s.Downloads(ctx, 5)
	if len(active) != 1 || active[0].Progress != 50 || len(recent) != 1 {
		t.Errorf("downloads: %+v %+v", active, recent)
	}
	daily, _ := s.DailySaved(ctx, 7)
	if len(daily) != 7 {
		t.Errorf("daily buckets: %d", len(daily))
	}
}

func TestPlaceholderRewrite(t *testing.T) {
	s := &Store{driver: Postgres}
	if got := s.q("a=? AND b IN (?,?)"); got != "a=$1 AND b IN ($2,$3)" {
		t.Errorf("rewrite: %s", got)
	}
	s.driver = SQLite
	if got := s.q("a=?"); got != "a=?" {
		t.Error("sqlite must keep ?")
	}
}
