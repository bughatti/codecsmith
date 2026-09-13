// Package scanner walks the configured libraries and queues files that are
// not yet in the profile's target codec (or are oversized).
package scanner

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bughatti/codecsmith/internal/config"
	"github.com/bughatti/codecsmith/internal/db"
	"github.com/bughatti/codecsmith/internal/job"
	"github.com/bughatti/codecsmith/internal/transcoder"
)

// VideoExtensions are the file types considered for transcoding.
var VideoExtensions = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".mov": true, ".wmv": true,
	".flv": true, ".webm": true, ".m4v": true, ".ts": true, ".m2ts": true, ".mpg": true, ".mpeg": true,
}

// Scanner enqueues work.
type Scanner struct {
	cfg   *config.Config
	store *db.Store
	log   *slog.Logger
	now   func() time.Time

	// probeCache remembers the codec of files that were examined and not
	// queued, keyed by path+size+mtime, so hourly rescans of a large library
	// don't re-run ffprobe on thousands of unchanged files.
	mu         sync.Mutex
	probeCache map[probeKey]string
}

type probeKey struct {
	path  string
	size  int64
	mtime int64
}

// New builds a scanner.
func New(cfg *config.Config, store *db.Store, log *slog.Logger) *Scanner {
	return &Scanner{cfg: cfg, store: store, log: log, now: time.Now, probeCache: map[probeKey]string{}}
}

func (s *Scanner) cachedCodec(k probeKey) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.probeCache[k]
	return c, ok
}

func (s *Scanner) rememberCodec(k probeKey, codec string) {
	s.mu.Lock()
	if len(s.probeCache) > 200000 {
		s.probeCache = map[probeKey]string{}
	}
	s.probeCache[k] = codec
	s.mu.Unlock()
}

// Summary is the result of a scan.
type Summary struct {
	Libraries int
	Seen      int
	Added     int
	Elapsed   time.Duration
}

// ScanAll walks every enabled library. Errors on individual libraries are
// logged and skipped.
func (s *Scanner) ScanAll(ctx context.Context) (Summary, error) {
	start := s.now()
	known, err := s.store.KnownPaths(ctx)
	if err != nil {
		return Summary{}, err
	}
	var sum Summary
	for _, lib := range s.cfg.Libraries {
		if !lib.IsEnabled() {
			continue
		}
		if _, err := os.Stat(lib.Path); err != nil {
			s.log.Warn("library path unavailable", "library", lib.Name, "path", lib.Path, "err", err)
			continue
		}
		sum.Libraries++
		seen, added := s.scanLibrary(ctx, lib, known)
		sum.Seen += seen
		sum.Added += added
		if ctx.Err() != nil {
			break
		}
	}
	sum.Elapsed = s.now().Sub(start)
	return sum, nil
}

// ScanLibrary walks one library by name.
func (s *Scanner) ScanLibrary(ctx context.Context, name string) (Summary, error) {
	lib, ok := s.cfg.LibraryByName(name)
	if !ok {
		return Summary{}, os.ErrNotExist
	}
	known, err := s.store.KnownPaths(ctx)
	if err != nil {
		return Summary{}, err
	}
	start := s.now()
	seen, added := s.scanLibrary(ctx, lib, known)
	return Summary{Libraries: 1, Seen: seen, Added: added, Elapsed: s.now().Sub(start)}, nil
}

func (s *Scanner) scanLibrary(ctx context.Context, lib config.Library, known map[string]job.Status) (seen, added int) {
	prof := s.cfg.ProfileFor(lib)
	target := s.cfg.TargetCodec(prof)
	log := s.log.With("library", lib.Name)

	err := filepath.WalkDir(lib.Path, func(path string, d fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if walkErr != nil {
			log.Debug("walk error", "path", path, "err", walkErr)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if !Eligible(path, info, s.now(), s.cfg.Worker.MinFileAge) {
			return nil
		}
		if _, exists := known[path]; exists {
			return nil
		}
		if !s.cfg.Worker.AllowHardlinked && transcoder.HardLinks(info) > 1 {
			log.Debug("skipping hardlinked file (likely seeding)", "path", path)
			return nil
		}
		seen++

		key := probeKey{path, info.Size(), info.ModTime().UnixNano()}
		codec, cached := s.cachedCodec(key)
		if !cached {
			var err error
			codec, err = transcoder.ProbeCodec(ctx, s.cfg.Encoder.FFprobe, path)
			if err != nil {
				log.Debug("ffprobe failed", "path", path, "err", err)
				return nil
			}
			s.rememberCodec(key, codec)
		}
		if !transcoder.IsVideoCodec(codec) {
			return nil
		}
		if !NeedsWork(codec, info.Size(), target, prof) {
			return nil
		}
		id, err := s.store.AddJob(ctx, db.NewJob{
			FilePath: path, Library: lib.Name, Profile: lib.Profile,
			OriginalSize: info.Size(), OriginalCodec: codec, Priority: lib.Priority,
		})
		if err != nil {
			log.Warn("add job failed", "path", path, "err", err)
			return nil
		}
		if id > 0 {
			known[path] = job.StatusQueued
			added++
			log.Info("queued", "id", id, "path", path, "codec", codec, "size", info.Size())
		}
		return nil
	})
	if err != nil && ctx.Err() == nil {
		log.Warn("scan aborted", "err", err)
	}
	log.Info("scan complete", "candidates", seen, "queued", added)
	return seen, added
}

// Eligible applies the cheap filesystem-level filters: extension, not a
// temp/backup artefact, not still being written (mtime within minFileAge).
// A modification time in the future (broken release tooling sets these) is
// treated as old rather than "just written".
func Eligible(path string, info fs.FileInfo, now time.Time, minFileAge time.Duration) bool {
	if !info.Mode().IsRegular() {
		return false
	}
	ext := strings.ToLower(filepath.Ext(path))
	if !VideoExtensions[ext] {
		return false
	}
	base := filepath.Base(path)
	if strings.HasPrefix(base, ".") || strings.HasSuffix(base, ".backup") || strings.Contains(base, ".part") ||
		strings.HasPrefix(base, "transcode_") {
		return false
	}
	if info.Size() < 1<<20 { // < 1 MiB: samples, junk
		return false
	}
	mtime := info.ModTime()
	if mtime.After(now) {
		return true
	}
	return now.Sub(mtime) >= minFileAge
}

// NeedsWork decides whether a file with the given codec/size should be
// queued for a profile targeting target. Files already in the target codec
// are only redone above size_limit_gb; codecs listed in skip_codecs (AV1 by
// default) are never touched because the output would not be smaller.
func NeedsWork(codec string, size int64, target string, prof config.Profile) bool {
	if codec == target {
		limit := int64(prof.SizeLimitGB * (1 << 30))
		return limit > 0 && size > limit
	}
	return !prof.SkipsCodec(codec)
}
