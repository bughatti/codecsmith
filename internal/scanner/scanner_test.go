package scanner

import (
	"io/fs"
	"testing"
	"time"

	"github.com/bughatti/codecsmith/internal/config"
)

type fakeInfo struct {
	name  string
	size  int64
	mtime time.Time
	mode  fs.FileMode
}

func (f fakeInfo) Name() string       { return f.name }
func (f fakeInfo) Size() int64        { return f.size }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return f.mtime }
func (f fakeInfo) IsDir() bool        { return false }
func (f fakeInfo) Sys() any           { return nil }

func TestEligible(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	minAge := 5 * time.Minute
	big := int64(2 << 20)
	cases := []struct {
		name string
		path string
		info fakeInfo
		want bool
	}{
		{"mkv old enough", "/m/a.mkv", fakeInfo{"a.mkv", big, old, 0o644}, true},
		{"uppercase ext", "/m/a.MP4", fakeInfo{"a.MP4", big, old, 0o644}, true},
		{"not video", "/m/a.srt", fakeInfo{"a.srt", big, old, 0o644}, false},
		{"too fresh", "/m/a.mkv", fakeInfo{"a.mkv", big, now.Add(-time.Minute), 0o644}, false},
		{"future mtime treated as old", "/m/a.mkv", fakeInfo{"a.mkv", big, now.Add(72 * 365 * 24 * time.Hour), 0o644}, true},
		{"backup artefact", "/m/a.mkv.backup", fakeInfo{"a.mkv.backup", big, old, 0o644}, false},
		{"partial download", "/m/a.part.mkv", fakeInfo{"a.part.mkv", big, old, 0o644}, false},
		{"temp output", "/tmp/transcode_9.mkv", fakeInfo{"transcode_9.mkv", big, old, 0o644}, false},
		{"hidden", "/m/._a.mkv", fakeInfo{"._a.mkv", big, old, 0o644}, false},
		{"tiny sample", "/m/sample.mkv", fakeInfo{"sample.mkv", 100, old, 0o644}, false},
		{"symlink", "/m/a.mkv", fakeInfo{"a.mkv", big, old, fs.ModeSymlink}, false},
	}
	for _, c := range cases {
		if got := Eligible(c.path, c.info, now, minAge); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestProbeCache(t *testing.T) {
	s := New(nil, nil, nil)
	s.probeCache = map[probeKey]string{}
	k := probeKey{"/m/a.mkv", 10, 20}
	if _, ok := s.cachedCodec(k); ok {
		t.Fatal("empty cache hit")
	}
	s.rememberCodec(k, "hevc")
	if c, ok := s.cachedCodec(k); !ok || c != "hevc" {
		t.Fatal("cache miss after remember")
	}
	if _, ok := s.cachedCodec(probeKey{"/m/a.mkv", 11, 20}); ok {
		t.Fatal("size change must miss")
	}
}

func TestNeedsWork(t *testing.T) {
	gb := int64(1 << 30)
	p := config.Profile{SizeLimitGB: 5}
	if !NeedsWork("h264", gb, "hevc", p) {
		t.Error("h264 → hevc should need work")
	}
	if NeedsWork("hevc", gb, "hevc", p) {
		t.Error("small hevc should not need work")
	}
	if !NeedsWork("hevc", 6*gb, "hevc", p) {
		t.Error("oversized hevc should need work")
	}
	if NeedsWork("hevc", 60*gb, "hevc", config.Profile{SizeLimitGB: 0}) {
		t.Error("size_limit 0 disables same-codec re-encode")
	}
	if !NeedsWork("hevc", gb, "av1", p) {
		t.Error("hevc → av1 should need work")
	}
	p.SkipCodecs = []string{"av1"}
	if NeedsWork("av1", gb, "hevc", p) {
		t.Error("av1 → hevc must be skipped by default")
	}
	if !NeedsWork("vc1", 30*gb, "hevc", p) {
		t.Error("vc1 → hevc should need work")
	}
	if !NeedsWork("av1", 6*gb, "av1", p) {
		t.Error("oversized av1 with av1 target uses the size rule, not skip_codecs")
	}
}
