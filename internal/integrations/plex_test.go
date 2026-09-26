package integrations

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePlex serves sections, a substring file filter like the real server,
// and records scans and analyze calls.
type fakePlex struct {
	mu       sync.Mutex
	files    map[string]string // file → ratingKey
	scans    []string
	analyzed []string
	onScan   func(f *fakePlex)
}

func (f *fakePlex) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("X-Plex-Token") != "tok" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.Path == "/library/sections":
		fmt.Fprint(w, `{"MediaContainer":{"Directory":[
			{"key":"1","type":"movie","Location":[{"path":"/media/movies"}]},
			{"key":"2","type":"show","Location":[{"path":"/media/shows"}]},
			{"key":"3","type":"artist","Location":[{"path":"/media/music"}]}]}}`)
	case strings.HasSuffix(r.URL.Path, "/refresh"):
		f.scans = append(f.scans, r.URL.Query().Get("path"))
		if f.onScan != nil {
			f.onScan(f)
		}
	case strings.HasSuffix(r.URL.Path, "/all"):
		var items []string
		for file, key := range f.files {
			if strings.Contains(file, r.URL.Query().Get("file")) {
				items = append(items, fmt.Sprintf(`{"ratingKey":%q,"Media":[{"Part":[{"file":%q}]}]}`, key, file))
			}
		}
		fmt.Fprintf(w, `{"MediaContainer":{"Metadata":[%s]}}`, strings.Join(items, ","))
	case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/analyze"):
		f.analyzed = append(f.analyzed, strings.Split(r.URL.Path, "/")[3])
	default:
		http.NotFound(w, r)
	}
}

func newTestPlex(t *testing.T, f *fakePlex, pathMap map[string]string) *Plex {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	p := NewPlex(srv.URL, "tok", pathMap, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.retries, p.retryDelay = 2, time.Millisecond
	return p
}

func TestPlexRefreshSamePath(t *testing.T) {
	f := &fakePlex{files: map[string]string{
		"/media/movies/2067 (2020)/2067.mkv":        "100",
		"/media/movies/2067 (2020)/2067.mkv.extras": "101", // substring decoy
	}}
	p := newTestPlex(t, f, nil)
	if err := p.Refresh(context.Background(), "/media/movies/2067 (2020)/2067.mkv", "/media/movies/2067 (2020)/2067.mkv"); err != nil {
		t.Fatal(err)
	}
	if len(f.scans) != 1 || f.scans[0] != "/media/movies/2067 (2020)" {
		t.Errorf("scans %v", f.scans)
	}
	if len(f.analyzed) != 1 || f.analyzed[0] != "100" {
		t.Errorf("analyzed %v, want [100]", f.analyzed)
	}
}

func TestPlexRefreshContainerChange(t *testing.T) {
	// Plex lists the new name only after the folder scan.
	f := &fakePlex{files: map[string]string{}}
	f.onScan = func(f *fakePlex) { f.files["/media/shows/S/Season 1/e1.mkv"] = "7" }
	p := newTestPlex(t, f, nil)
	if err := p.Refresh(context.Background(), "/media/shows/S/Season 1/e1.mp4", "/media/shows/S/Season 1/e1.mkv"); err != nil {
		t.Fatal(err)
	}
	if len(f.analyzed) != 1 || f.analyzed[0] != "7" {
		t.Errorf("analyzed %v, want [7]", f.analyzed)
	}
}

func TestPlexRefreshPathMap(t *testing.T) {
	f := &fakePlex{files: map[string]string{"/media/movies/A/a.mkv": "5"}}
	p := newTestPlex(t, f, map[string]string{"/data": "/elsewhere", "/data/films": "/media/movies"})
	if err := p.Refresh(context.Background(), "/data/films/A/a.mkv", "/data/films/A/a.mkv"); err != nil {
		t.Fatal(err)
	}
	if len(f.analyzed) != 1 || f.analyzed[0] != "5" {
		t.Errorf("analyzed %v, want [5]", f.analyzed)
	}
}

func TestPlexRefreshNotInPlex(t *testing.T) {
	p := newTestPlex(t, &fakePlex{}, nil)
	for _, file := range []string{"/media/movies-4k/x.mkv", "/media/music/x.flac", "/other/x.mkv"} {
		if err := p.Refresh(context.Background(), file, file); err != ErrNotInPlex {
			t.Errorf("%s: err %v, want ErrNotInPlex", file, err)
		}
	}
}

func TestPlexRefreshMissingItem(t *testing.T) {
	f := &fakePlex{files: map[string]string{}}
	p := newTestPlex(t, f, nil)
	if err := p.Refresh(context.Background(), "/media/movies/B/b.mkv", "/media/movies/B/b.mkv"); err == nil {
		t.Fatal("expected an error for a file Plex does not list")
	}
	if len(f.analyzed) != 0 {
		t.Errorf("analyzed %v", f.analyzed)
	}
}
