// Package db is the storage layer. It speaks both SQLite (zero-setup
// default, pure-Go driver) and Postgres (multi-worker installs, uses
// LISTEN/NOTIFY and SKIP LOCKED) through database/sql with a tiny dialect
// shim. Queries are written once with "?" placeholders and Go time values
// so no date arithmetic happens in SQL.
package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver
	_ "modernc.org/sqlite"             // sqlite driver

	"github.com/bughatti/codecsmith/internal/job"
)

//go:embed migrations/postgres/*.sql migrations/sqlite/*.sql
var migrationFS embed.FS

// Driver identifies the backend.
type Driver string

const (
	Postgres Driver = "postgres"
	SQLite   Driver = "sqlite"
)

// Store is the database handle.
type Store struct {
	db     *sql.DB
	driver Driver
	dsn    string
}

// Open connects and verifies the connection. For sqlite, dsn is a file path.
func Open(ctx context.Context, driver Driver, dsn string) (*Store, error) {
	var (
		d   *sql.DB
		err error
	)
	switch driver {
	case Postgres:
		d, err = sql.Open("pgx", dsn)
		if err == nil {
			d.SetMaxOpenConns(16)
			d.SetMaxIdleConns(4)
			d.SetConnMaxLifetime(30 * time.Minute)
		}
	case SQLite:
		d, err = sql.Open("sqlite", sqliteDSN(dsn))
		if err == nil {
			// One writer at a time; WAL lets readers proceed concurrently.
			d.SetMaxOpenConns(1)
		}
	default:
		return nil, fmt.Errorf("unknown driver %q", driver)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", driver, err)
	}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := d.PingContext(pctx); err != nil {
		d.Close()
		return nil, fmt.Errorf("connect %s: %w", driver, err)
	}
	return &Store{db: d, driver: driver, dsn: dsn}, nil
}

func sqliteDSN(path string) string {
	if strings.HasPrefix(path, "file:") {
		return path
	}
	return "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_txlock=immediate"
}

// Close releases the pool.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks liveness.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Driver reports the backend in use.
func (s *Store) Driver() Driver { return s.driver }

// DB exposes the raw handle for one-off queries.
func (s *Store) DB() *sql.DB { return s.db }

// -----------------------------------------------------------------------------
// dialect helpers
// -----------------------------------------------------------------------------

// q rewrites "?" placeholders to "$n" for Postgres.
func (s *Store) q(query string) string {
	if s.driver != Postgres {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (s *Store) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, s.q(query), args...)
}

func (s *Store) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, s.q(query), args...)
}

func (s *Store) queryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, s.q(query), args...)
}

// now returns a UTC timestamp truncated to microseconds (Postgres precision)
// so round-trips compare equal on both backends.
func now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

func placeholders(n int) string {
	if n == 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func statusArgs(st []job.Status) []any {
	out := make([]any, len(st))
	for i, s := range st {
		out[i] = string(s)
	}
	return out
}

// -----------------------------------------------------------------------------
// migrations
// -----------------------------------------------------------------------------

// Migrate applies any embedded migrations not yet recorded in
// schema_migrations. Returns the versions applied.
func (s *Store) Migrate(ctx context.Context) ([]int, error) {
	if _, err := s.exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, applied_at TIMESTAMP NOT NULL)`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}
	applied := map[int]bool{}
	rows, err := s.query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err == nil {
			applied[v] = true
		}
	}
	rows.Close()

	dir := "migrations/" + string(s.driver)
	entries, err := fs.ReadDir(migrationFS, dir)
	if err != nil {
		return nil, err
	}
	type mig struct {
		version int
		name    string
	}
	var migs []mig
	for _, e := range entries {
		v, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0])
		if err != nil {
			continue
		}
		migs = append(migs, mig{v, e.Name()})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })

	var done []int
	for _, m := range migs {
		if applied[m.version] {
			continue
		}
		body, err := migrationFS.ReadFile(path.Join(dir, m.name))
		if err != nil {
			return done, err
		}
		if err := s.applyMigration(ctx, m.version, string(body)); err != nil {
			return done, fmt.Errorf("migration %s: %w", m.name, err)
		}
		done = append(done, m.version)
	}
	return done, nil
}

func (s *Store) applyMigration(ctx context.Context, version int, body string) error {
	// Postgres files may carry their own BEGIN/COMMIT; strip them and run in
	// our transaction so the bookkeeping row commits atomically with the DDL.
	body = stripTxWrappers(body)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if s.driver == SQLite {
		// modernc executes one statement per Exec; split on ';' at line end.
		for _, stmt := range splitStatements(body) {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("%w\nstatement: %s", err, stmt)
			}
		}
	} else {
		if _, err := tx.ExecContext(ctx, body); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`), version, now()); err != nil {
		return err
	}
	return tx.Commit()
}

func stripTxWrappers(body string) string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		t := strings.ToUpper(strings.TrimSpace(line))
		if t == "BEGIN;" || t == "COMMIT;" {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func splitStatements(body string) []string {
	var stmts []string
	var cur strings.Builder
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue
		}
		cur.WriteString(line)
		cur.WriteString("\n")
		if strings.HasSuffix(trimmed, ";") {
			st := strings.TrimSpace(cur.String())
			if st != ";" {
				stmts = append(stmts, st)
			}
			cur.Reset()
		}
	}
	if st := strings.TrimSpace(cur.String()); st != "" {
		stmts = append(stmts, st)
	}
	return stmts
}

// SchemaVersion returns the highest applied migration (0 if none).
func (s *Store) SchemaVersion(ctx context.Context) int {
	var v sql.NullInt64
	_ = s.queryRow(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	return int(v.Int64)
}

// -----------------------------------------------------------------------------
// jobs
// -----------------------------------------------------------------------------

const jobColumns = `id, file_path, library, COALESCE(profile,''), status, priority,
	COALESCE(original_size,0), COALESCE(new_size,0),
	COALESCE(original_codec,''), COALESCE(target_codec,''),
	created_at, started_at, completed_at, updated_at,
	COALESCE(error_message,''), progress, COALESCE(speed,0), COALESCE(fps,0), eta_seconds,
	COALESCE(worker_id,''), cancel_requested`

// NewJob is the insert shape.
type NewJob struct {
	FilePath      string
	Library       string
	Profile       string
	OriginalSize  int64
	OriginalCodec string
	Priority      int
}

// AddJob inserts a job unless the path already exists. Returns 0 when the
// path was already known.
func (s *Store) AddJob(ctx context.Context, n NewJob) (int64, error) {
	var id int64
	err := s.queryRow(ctx, `
		INSERT INTO jobs (file_path, library, profile, status, priority, original_size, original_codec, created_at)
		VALUES (?, ?, ?, 'queued', ?, ?, ?, ?)
		ON CONFLICT (file_path) DO NOTHING
		RETURNING id`,
		n.FilePath, n.Library, n.Profile, n.Priority, n.OriginalSize, n.OriginalCodec, now()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// ClaimNextJob atomically takes the highest-priority pending job from any of
// the given libraries and marks it processing for workerID. Returns nil when
// nothing is claimable.
func (s *Store) ClaimNextJob(ctx context.Context, workerID string, libraries []string) (*job.Job, error) {
	if len(libraries) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	lock := ""
	if s.driver == Postgres {
		lock = "FOR UPDATE SKIP LOCKED"
	}
	args := []any{}
	for _, l := range libraries {
		args = append(args, l)
	}
	q := fmt.Sprintf(`
		SELECT %s FROM jobs
		WHERE status IN ('queued','interrupted') AND library IN (%s)
		ORDER BY CASE WHEN status='interrupted' THEN 0 ELSE 1 END, priority DESC, created_at ASC
		LIMIT 1 %s`, jobColumns, placeholders(len(libraries)), lock)

	j, err := scanJob(tx.QueryRowContext(ctx, s.q(q), args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t := now()
	res, err := tx.ExecContext(ctx, s.q(`
		UPDATE jobs SET status='processing', worker_id=?, started_at=?, updated_at=?, progress=0,
		       cancel_requested=?, error_message=NULL
		WHERE id=? AND status IN ('queued','interrupted')`), workerID, t, t, false, j.ID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil // lost the race (sqlite without row locks)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	j.Status = job.StatusProcessing
	j.WorkerID = workerID
	j.StartedAt = &t
	return j, nil
}

// Update is a set of optional field changes applied with UpdateJob.
type Update struct {
	Status       *job.Status
	Progress     *float64
	Speed        *float64
	FPS          *float64
	ETASeconds   *int
	NewSize      *int64
	TargetCodec  *string
	ErrorMessage *string
	Completed    bool // sets completed_at=now
}

// UpdateJob applies the non-nil fields and bumps updated_at.
func (s *Store) UpdateJob(ctx context.Context, id int64, u Update) error {
	set := []string{"updated_at=?"}
	args := []any{now()}
	add := func(col string, v any) {
		set = append(set, col+"=?")
		args = append(args, v)
	}
	if u.Status != nil {
		add("status", string(*u.Status))
	}
	if u.Progress != nil {
		add("progress", *u.Progress)
	}
	if u.Speed != nil {
		add("speed", *u.Speed)
	}
	if u.FPS != nil {
		add("fps", *u.FPS)
	}
	if u.ETASeconds != nil {
		add("eta_seconds", *u.ETASeconds)
	}
	if u.NewSize != nil {
		add("new_size", *u.NewSize)
	}
	if u.TargetCodec != nil {
		add("target_codec", *u.TargetCodec)
	}
	if u.ErrorMessage != nil {
		add("error_message", *u.ErrorMessage)
	}
	if u.Completed {
		add("completed_at", now())
	}
	args = append(args, id)
	_, err := s.exec(ctx, "UPDATE jobs SET "+strings.Join(set, ", ")+" WHERE id=?", args...)
	return err
}

// GetJob fetches one job.
func (s *Store) GetJob(ctx context.Context, id int64) (*job.Job, error) {
	j, err := scanJob(s.queryRow(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// ListJobs returns jobs in the given statuses, newest activity first.
func (s *Store) ListJobs(ctx context.Context, statuses []job.Status, limit int) ([]job.Job, error) {
	if limit <= 0 {
		limit = 100
	}
	order := "ORDER BY COALESCE(completed_at, started_at, created_at) DESC"
	if len(statuses) > 0 && (statuses[0] == job.StatusQueued || statuses[0] == job.StatusInterrupted) {
		order = "ORDER BY CASE WHEN status='interrupted' THEN 0 ELSE 1 END, priority DESC, created_at ASC"
	}
	args := statusArgs(statuses)
	args = append(args, limit)
	rows, err := s.query(ctx, fmt.Sprintf(`SELECT %s FROM jobs WHERE status IN (%s) %s LIMIT ?`,
		jobColumns, placeholders(len(statuses)), order), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectJobs(rows)
}

// CountByStatus returns counts of every status.
func (s *Store) CountByStatus(ctx context.Context) (map[job.Status]int, error) {
	rows, err := s.query(ctx, `SELECT status, COUNT(*) FROM jobs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[job.Status]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err == nil {
			out[job.Status(st)] = n
		}
	}
	return out, rows.Err()
}

// RunningByLibrary counts active jobs per library.
func (s *Store) RunningByLibrary(ctx context.Context) (map[string]int, int, error) {
	rows, err := s.query(ctx, `SELECT library, COUNT(*) FROM jobs WHERE status IN ('processing','running') GROUP BY library`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := map[string]int{}
	total := 0
	for rows.Next() {
		var lib string
		var n int
		if err := rows.Scan(&lib, &n); err == nil {
			out[lib] = n
			total += n
		}
	}
	return out, total, rows.Err()
}

// Stats aggregates jobs created since the given time (zero = all time).
func (s *Store) Stats(ctx context.Context, since time.Time) (job.Stats, error) {
	var st job.Stats
	where := "1=1"
	args := []any{}
	if !since.IsZero() {
		where = "created_at >= ?"
		args = append(args, since)
	}
	err := s.queryRow(ctx, `
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE status='completed'),
		       COUNT(*) FILTER (WHERE status='failed'),
		       COUNT(*) FILTER (WHERE status='skipped'),
		       COUNT(*) FILTER (WHERE status='cancelled'),
		       COUNT(*) FILTER (WHERE status IN ('queued','interrupted')),
		       COUNT(*) FILTER (WHERE status IN ('processing','running')),
		       COALESCE(SUM(CASE WHEN status='completed' THEN original_size - COALESCE(new_size, original_size) ELSE 0 END), 0)
		FROM jobs WHERE `+where, args...).Scan(
		&st.Total, &st.Completed, &st.Failed, &st.Skipped, &st.Cancelled, &st.Queued, &st.Running, &st.TotalSaved)
	return st, err
}

// LibraryStat is a per-library summary for the dashboard.
type LibraryStat struct {
	Library   string
	Total     int64
	Completed int64
	Queued    int64
	Running   int64
	Failed    int64
	Saved     int64
}

// StatsByLibrary aggregates all-time numbers per library.
func (s *Store) StatsByLibrary(ctx context.Context) ([]LibraryStat, error) {
	rows, err := s.query(ctx, `
		SELECT library, COUNT(*),
		       COUNT(*) FILTER (WHERE status='completed'),
		       COUNT(*) FILTER (WHERE status IN ('queued','interrupted')),
		       COUNT(*) FILTER (WHERE status IN ('processing','running')),
		       COUNT(*) FILTER (WHERE status='failed'),
		       COALESCE(SUM(CASE WHEN status='completed' THEN original_size - COALESCE(new_size, original_size) ELSE 0 END), 0)
		FROM jobs GROUP BY library ORDER BY library`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LibraryStat
	for rows.Next() {
		var l LibraryStat
		if err := rows.Scan(&l.Library, &l.Total, &l.Completed, &l.Queued, &l.Running, &l.Failed, &l.Saved); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// DailySaved returns bytes saved per day for the last n days (oldest first).
func (s *Store) DailySaved(ctx context.Context, days int) ([]DayPoint, error) {
	since := now().Add(-time.Duration(days) * 24 * time.Hour)
	rows, err := s.query(ctx, `
		SELECT completed_at, original_size - COALESCE(new_size, original_size)
		FROM jobs WHERE status='completed' AND completed_at >= ?`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	buckets := map[string]*DayPoint{}
	for rows.Next() {
		var t time.Time
		var saved int64
		if err := rows.Scan(&t, &saved); err != nil {
			continue
		}
		day := t.UTC().Format("2006-01-02")
		if buckets[day] == nil {
			buckets[day] = &DayPoint{Day: day}
		}
		buckets[day].Saved += saved
		buckets[day].Jobs++
	}
	out := make([]DayPoint, 0, days)
	for i := days - 1; i >= 0; i-- {
		day := now().Add(-time.Duration(i) * 24 * time.Hour).Format("2006-01-02")
		if b := buckets[day]; b != nil {
			out = append(out, *b)
		} else {
			out = append(out, DayPoint{Day: day})
		}
	}
	return out, nil
}

// DayPoint is one bar in the savings chart.
type DayPoint struct {
	Day   string `json:"day"`
	Saved int64  `json:"saved"`
	Jobs  int    `json:"jobs"`
}

// MarkStaleJobs fails active jobs whose last update is older than the cutoff.
func (s *Store) MarkStaleJobs(ctx context.Context, staleAfter time.Duration) (int64, error) {
	cutoff := now().Add(-staleAfter)
	res, err := s.exec(ctx, `
		UPDATE jobs SET status='failed', error_message='stale: no progress for '||?||' — worker died?',
		       completed_at=?, updated_at=?
		WHERE status IN ('processing','running') AND COALESCE(updated_at, started_at) < ?`,
		staleAfter.String(), now(), now(), cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ReclaimWorkerJobs marks jobs a (restarting) worker left active as
// interrupted so they are picked up first.
func (s *Store) ReclaimWorkerJobs(ctx context.Context, workerID string) (int64, error) {
	res, err := s.exec(ctx, `
		UPDATE jobs SET status='interrupted', updated_at=?, progress=0, speed=NULL, fps=NULL, eta_seconds=NULL
		WHERE status IN ('processing','running') AND worker_id=?`, now(), workerID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RequeueJob puts a terminal or stuck job back in the queue.
func (s *Store) RequeueJob(ctx context.Context, id int64) (bool, error) {
	res, err := s.exec(ctx, `
		UPDATE jobs SET status='queued', started_at=NULL, completed_at=NULL, error_message=NULL,
		       progress=0, speed=NULL, fps=NULL, eta_seconds=NULL, worker_id=NULL, new_size=NULL,
		       cancel_requested=?, updated_at=?
		WHERE id=? AND status IN ('failed','skipped','cancelled','interrupted','processing','running','completed')`,
		false, now(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RequeueFailed retries the most recent n failed jobs.
func (s *Store) RequeueFailed(ctx context.Context, limit int) (int64, error) {
	return s.RequeueByStatus(ctx, []job.Status{job.StatusFailed}, limit)
}

// RequeueByStatus puts the most recent jobs in the given statuses back in
// the queue. Used to re-run everything a dry run skipped.
func (s *Store) RequeueByStatus(ctx context.Context, statuses []job.Status, limit int) (int64, error) {
	if len(statuses) == 0 {
		return 0, nil
	}
	args := statusArgs(statuses)
	args = append(args, limit)
	rows, err := s.query(ctx, fmt.Sprintf(
		`SELECT id FROM jobs WHERE status IN (%s) ORDER BY completed_at DESC NULLS LAST LIMIT ?`,
		placeholders(len(statuses))), args...)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	var n int64
	for _, id := range ids {
		if ok, err := s.RequeueJob(ctx, id); err == nil && ok {
			n++
		}
	}
	return n, nil
}

// RequestCancel flags an active job; the worker kills ffmpeg on its next
// progress tick. Queued jobs are cancelled immediately.
func (s *Store) RequestCancel(ctx context.Context, id int64) (string, error) {
	res, err := s.exec(ctx, `
		UPDATE jobs SET status='cancelled', completed_at=?, updated_at=?, error_message='cancelled before start'
		WHERE id=? AND status IN ('queued','interrupted')`, now(), now(), id)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return "cancelled", nil
	}
	res, err = s.exec(ctx, `UPDATE jobs SET cancel_requested=?, updated_at=? WHERE id=? AND status IN ('processing','running')`, true, now(), id)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return "requested", nil
	}
	return "", nil
}

// CancelRequested reads the flag for an active job.
func (s *Store) CancelRequested(ctx context.Context, id int64) bool {
	var v bool
	_ = s.queryRow(ctx, `SELECT cancel_requested FROM jobs WHERE id=?`, id).Scan(&v)
	return v
}

// DeleteJob removes a job row (any status). Files are never touched.
func (s *Store) DeleteJob(ctx context.Context, id int64) (bool, error) {
	res, err := s.exec(ctx, `DELETE FROM jobs WHERE id=? AND status NOT IN ('processing','running')`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// KnownPaths returns every file_path in the jobs table (for the scanner).
func (s *Store) KnownPaths(ctx context.Context) (map[string]job.Status, error) {
	rows, err := s.query(ctx, `SELECT file_path, status FROM jobs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]job.Status, 4096)
	for rows.Next() {
		var p, st string
		if err := rows.Scan(&p, &st); err != nil {
			return nil, err
		}
		out[p] = job.Status(st)
	}
	return out, rows.Err()
}

// PendingPaths returns file paths of pending/active jobs with their ids.
func (s *Store) PendingPaths(ctx context.Context) (map[int64]string, error) {
	rows, err := s.query(ctx, `SELECT id, file_path FROM jobs WHERE status IN ('queued','interrupted','processing','running')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err == nil {
			out[id] = p
		}
	}
	return out, rows.Err()
}

// FailJob marks a job failed with a message.
func (s *Store) FailJob(ctx context.Context, id int64, msg string) error {
	st := job.StatusFailed
	return s.UpdateJob(ctx, id, Update{Status: &st, ErrorMessage: &msg, Completed: true})
}

// FailureSummary groups failed jobs by message.
func (s *Store) FailureSummary(ctx context.Context, limit int) ([]ErrorCount, int64, error) {
	var total int64
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM jobs WHERE status='failed'`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.query(ctx, `
		SELECT COALESCE(error_message,''), COUNT(*) AS c FROM jobs WHERE status='failed'
		GROUP BY error_message ORDER BY c DESC LIMIT ?`, limit)
	if err != nil {
		return nil, total, err
	}
	defer rows.Close()
	var out []ErrorCount
	for rows.Next() {
		var e ErrorCount
		if err := rows.Scan(&e.Message, &e.Count); err == nil {
			out = append(out, e)
		}
	}
	return out, total, rows.Err()
}

// ErrorCount is one row of FailureSummary.
type ErrorCount struct {
	Message string `json:"message"`
	Count   int64  `json:"count"`
}

// -----------------------------------------------------------------------------
// system state (key/value)
// -----------------------------------------------------------------------------

// GetState reads a key ("" if absent).
func (s *Store) GetState(ctx context.Context, key string) (string, error) {
	var v sql.NullString
	err := s.queryRow(ctx, `SELECT value FROM system_state WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v.String, err
}

// SetState upserts a key.
func (s *Store) SetState(ctx context.Context, key, value string) error {
	_, err := s.exec(ctx, `
		INSERT INTO system_state (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value, now())
	return err
}

// StateUpdatedAt returns when a key was last written (zero if absent).
func (s *Store) StateUpdatedAt(ctx context.Context, key string) time.Time {
	var t time.Time
	_ = s.queryRow(ctx, `SELECT updated_at FROM system_state WHERE key=?`, key).Scan(&t)
	return t
}

// -----------------------------------------------------------------------------
// metrics + storage
// -----------------------------------------------------------------------------

// Metric is one system sample.
type Metric struct {
	Timestamp time.Time `json:"timestamp"`
	CPU       float64   `json:"cpu_percent"`
	Memory    float64   `json:"memory_percent"`
	Disk      float64   `json:"disk_usage_percent"`
	Active    int       `json:"active_jobs"`
	Queued    int       `json:"queue_size"`
	// GPU fields are nil when no GPU counters are available.
	GPU        *float64 `json:"gpu_percent"`
	GPUEncoder *float64 `json:"gpu_encoder_percent"`
	GPUMemory  *float64 `json:"gpu_memory_percent"`
}

// RecordMetric inserts a sample.
func (s *Store) RecordMetric(ctx context.Context, m Metric) error {
	_, err := s.exec(ctx, `
		INSERT INTO system_metrics (timestamp, cpu_percent, memory_percent, disk_usage_percent, active_jobs, queue_size,
		                            gpu_percent, gpu_encoder_percent, gpu_memory_percent)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, now(), m.CPU, m.Memory, m.Disk, m.Active, m.Queued, m.GPU, m.GPUEncoder, m.GPUMemory)
	return err
}

// Metrics returns samples since the cutoff, oldest first.
func (s *Store) Metrics(ctx context.Context, since time.Time) ([]Metric, error) {
	rows, err := s.query(ctx, `
		SELECT timestamp, COALESCE(cpu_percent,0), COALESCE(memory_percent,0), COALESCE(disk_usage_percent,0),
		       COALESCE(active_jobs,0), COALESCE(queue_size,0), gpu_percent, gpu_encoder_percent, gpu_memory_percent
		FROM system_metrics WHERE timestamp >= ? ORDER BY timestamp ASC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Metric{}
	for rows.Next() {
		var m Metric
		if err := rows.Scan(&m.Timestamp, &m.CPU, &m.Memory, &m.Disk, &m.Active, &m.Queued, &m.GPU, &m.GPUEncoder, &m.GPUMemory); err == nil {
			out = append(out, m)
		}
	}
	return out, rows.Err()
}

// PruneMetrics deletes samples older than the cutoff.
func (s *Store) PruneMetrics(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.exec(ctx, `DELETE FROM system_metrics WHERE timestamp < ?`, olderThan)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// StorageStat is a filesystem sample for one library path.
type StorageStat struct {
	Timestamp time.Time `json:"timestamp"`
	Path      string    `json:"path"`
	Total     int64     `json:"total_bytes"`
	Used      int64     `json:"used_bytes"`
	Free      int64     `json:"free_bytes"`
	FileCount int       `json:"file_count"`
}

// RecordStorage inserts a sample.
func (s *Store) RecordStorage(ctx context.Context, st StorageStat) error {
	_, err := s.exec(ctx, `
		INSERT INTO storage_stats (timestamp, path, total_bytes, used_bytes, free_bytes, file_count)
		VALUES (?, ?, ?, ?, ?, ?)`, now(), st.Path, st.Total, st.Used, st.Free, st.FileCount)
	return err
}

// LatestStorage returns the newest sample per path.
func (s *Store) LatestStorage(ctx context.Context) ([]StorageStat, error) {
	rows, err := s.query(ctx, `
		SELECT s.timestamp, s.path, s.total_bytes, s.used_bytes, s.free_bytes, s.file_count
		FROM storage_stats s
		JOIN (SELECT path, MAX(id) AS id FROM storage_stats GROUP BY path) m ON m.id = s.id
		ORDER BY s.path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StorageStat{}
	for rows.Next() {
		var st StorageStat
		if err := rows.Scan(&st.Timestamp, &st.Path, &st.Total, &st.Used, &st.Free, &st.FileCount); err == nil {
			out = append(out, st)
		}
	}
	return out, rows.Err()
}

// PruneStorage keeps the table small.
func (s *Store) PruneStorage(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.exec(ctx, `DELETE FROM storage_stats WHERE timestamp < ?`, olderThan)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// -----------------------------------------------------------------------------
// downloads (SABnzbd mirror)
// -----------------------------------------------------------------------------

// Download is one row of the downloads table.
type Download struct {
	ID             int64     `json:"id"`
	Source         string    `json:"source"`
	ExternalID     string    `json:"external_id"`
	Title          string    `json:"title"`
	Status         string    `json:"status"`
	Progress       float64   `json:"progress"`
	SizeTotal      int64     `json:"size_total"`
	SizeDownloaded int64     `json:"size_downloaded"`
	DownloadRate   float64   `json:"download_rate"`
	ETASeconds     *int      `json:"eta_seconds"`
	Category       string    `json:"category"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// UpsertDownload inserts or refreshes a download row.
func (s *Store) UpsertDownload(ctx context.Context, d Download) error {
	t := now()
	_, err := s.exec(ctx, `
		INSERT INTO downloads (source, external_id, title, status, progress, size_total, size_downloaded,
		                       download_rate, eta_seconds, category, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (source, external_id) DO UPDATE SET
			title=excluded.title, status=excluded.status, progress=excluded.progress,
			size_total=excluded.size_total, size_downloaded=excluded.size_downloaded,
			download_rate=excluded.download_rate, eta_seconds=excluded.eta_seconds,
			category=excluded.category, updated_at=excluded.updated_at`,
		d.Source, d.ExternalID, d.Title, d.Status, d.Progress, d.SizeTotal, d.SizeDownloaded,
		d.DownloadRate, d.ETASeconds, d.Category, t, t)
	return err
}

// Downloads lists active and recent rows.
func (s *Store) Downloads(ctx context.Context, limit int) (active, recent []Download, err error) {
	active, err = s.queryDownloads(ctx, `SELECT `+downloadColumns+` FROM downloads WHERE status IN ('downloading','paused','queued') ORDER BY created_at DESC`)
	if err != nil {
		return nil, nil, err
	}
	recent, err = s.queryDownloads(ctx, `SELECT `+downloadColumns+` FROM downloads ORDER BY updated_at DESC LIMIT ?`, limit)
	return active, recent, err
}

const downloadColumns = `id, source, external_id, title, status, progress, size_total, size_downloaded,
	download_rate, eta_seconds, COALESCE(category,''), created_at, updated_at`

func (s *Store) queryDownloads(ctx context.Context, q string, args ...any) ([]Download, error) {
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Download{}
	for rows.Next() {
		var d Download
		if err := rows.Scan(&d.ID, &d.Source, &d.ExternalID, &d.Title, &d.Status, &d.Progress, &d.SizeTotal,
			&d.SizeDownloaded, &d.DownloadRate, &d.ETASeconds, &d.Category, &d.CreatedAt, &d.UpdatedAt); err == nil {
			out = append(out, d)
		}
	}
	return out, rows.Err()
}

// PruneDownloads removes finished rows older than the cutoff and marks
// stale in-flight rows finished.
func (s *Store) PruneDownloads(ctx context.Context, olderThan time.Time) error {
	if _, err := s.exec(ctx, `DELETE FROM downloads WHERE status IN ('completed','failed') AND updated_at < ?`, olderThan); err != nil {
		return err
	}
	_, err := s.exec(ctx, `UPDATE downloads SET status='completed', progress=100, updated_at=? WHERE status='downloading' AND updated_at < ?`,
		now(), now().Add(-time.Hour))
	return err
}

// -----------------------------------------------------------------------------
// notifications
// -----------------------------------------------------------------------------

// NotifyChannel is the Postgres LISTEN/NOTIFY channel for new jobs.
const NotifyChannel = "transcoder_jobs"

// SupportsListen reports whether Listen is usable on this backend.
func (s *Store) SupportsListen() bool { return s.driver == Postgres }

// Listen blocks delivering NOTIFY payloads until ctx ends (Postgres only).
func (s *Store) Listen(ctx context.Context, onNotify func(payload string)) error {
	if s.driver != Postgres {
		<-ctx.Done()
		return ctx.Err()
	}
	conn, err := pgx.Connect(ctx, s.dsn)
	if err != nil {
		return fmt.Errorf("listen connect: %w", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{NotifyChannel}.Sanitize()); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("wait notify: %w", err)
		}
		if onNotify != nil && n != nil {
			onNotify(n.Payload)
		}
	}
}

// -----------------------------------------------------------------------------
// row scanning
// -----------------------------------------------------------------------------

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(r rowScanner) (*job.Job, error) {
	var (
		j               job.Job
		status          string
		started, done   sql.NullTime
		updated         sql.NullTime
		eta             sql.NullInt64
		cancelRequested bool
	)
	err := r.Scan(
		&j.ID, &j.FilePath, &j.Library, &j.Profile, &status, &j.Priority,
		&j.OriginalSize, &j.NewSize, &j.OriginalCodec, &j.TargetCodec,
		&j.CreatedAt, &started, &done, &updated,
		&j.ErrorMessage, &j.Progress, &j.Speed, &j.FPS, &eta,
		&j.WorkerID, &cancelRequested,
	)
	if err != nil {
		return nil, err
	}
	j.Status = job.Status(status)
	if started.Valid {
		t := started.Time
		j.StartedAt = &t
	}
	if done.Valid {
		t := done.Time
		j.CompletedAt = &t
	}
	if updated.Valid {
		t := updated.Time
		j.UpdatedAt = &t
	}
	if eta.Valid {
		v := int(eta.Int64)
		j.ETASeconds = &v
	}
	j.CancelRequested = cancelRequested
	return &j, nil
}

func collectJobs(rows *sql.Rows) ([]job.Job, error) {
	out := []job.Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}
