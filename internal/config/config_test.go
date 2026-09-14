package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const sample = `
web:
  listen: "127.0.0.1:9000"
libraries:
  - name: Movies
    path: /media/movies
    priority: 6
  - name: Anime
    path: /media/anime
    profile: anime
  - name: Anime Movies
    path: /media/anime/movies
    profile: anime
profiles:
  default:
    quality: 27
  anime:
    quality: 24
    max_bitrate: 4M
    audio_languages: [jpn, eng]
`

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadAndMerge(t *testing.T) {
	c, err := Load(writeCfg(t, sample), "all")
	if err != nil {
		t.Fatal(err)
	}
	if c.Web.Listen != "127.0.0.1:9000" {
		t.Errorf("listen = %s", c.Web.Listen)
	}
	if c.Database.Driver != "sqlite" || c.SQLitePath() != filepath.Join("data", "codecsmith.db") {
		t.Errorf("sqlite defaults: %s %s", c.Database.Driver, c.SQLitePath())
	}
	anime := c.Profiles["anime"]
	if anime.Quality != 24 || anime.MaxBitrate != "4M" {
		t.Errorf("anime profile not applied: %+v", anime)
	}
	if len(anime.SkipCodecs) != 1 || anime.SkipCodecs[0] != "av1" {
		t.Errorf("skip_codecs default not inherited: %v", anime.SkipCodecs)
	}
	if anime.AudioCodec != "aac" || anime.Container != "mkv" || anime.SizeLimitGB != 5 {
		t.Errorf("anime profile should inherit defaults: %+v", anime)
	}
	if len(anime.AudioLanguages) != 2 || anime.AudioLanguages[0] != "jpn" {
		t.Errorf("audio languages: %v", anime.AudioLanguages)
	}
	if c.Profiles["default"].Quality != 27 {
		t.Error("default profile override lost")
	}
	if c.Libraries[0].Profile != "default" {
		t.Error("library without profile should get default")
	}
}

func TestLibraryFor(t *testing.T) {
	c, err := Load(writeCfg(t, sample), "all")
	if err != nil {
		t.Fatal(err)
	}
	if l, ok := c.LibraryFor("/media/anime/movies/Film/Film.mkv"); !ok || l.Name != "Anime Movies" {
		t.Errorf("nested library should win: %+v %v", l, ok)
	}
	if l, ok := c.LibraryFor("/media/anime/Show/ep.mkv"); !ok || l.Name != "Anime" {
		t.Errorf("parent library: %+v %v", l, ok)
	}
	if _, ok := c.LibraryFor("/media/other/x.mkv"); ok {
		t.Error("outside all libraries should not match")
	}
	if _, ok := c.LibraryFor("/media/moviesx/x.mkv"); ok {
		t.Error("prefix without separator must not match")
	}
	// The web API's add-job endpoint relies on this check to keep request
	// paths inside the libraries, so ".." must never climb out of one.
	for _, p := range []string{"/media/movies/../other/x.mkv", "/media/movies/../../etc/passwd", "/media/anime/.."} {
		if l, ok := c.LibraryFor(p); ok && l.Path != "/media" {
			t.Errorf("%s escaped into library %q", p, l.Name)
		}
	}
}

func TestDryRunAndTrash(t *testing.T) {
	c, err := Load(writeCfg(t, sample), "all")
	if err != nil {
		t.Fatal(err)
	}
	if c.Worker.DryRun || c.Worker.TrashDir != "" {
		t.Errorf("both must be off by default: %+v", c.Worker)
	}
	if c.Worker.TrashRetention != 14*24*time.Hour {
		t.Errorf("trash retention default = %v", c.Worker.TrashRetention)
	}
	c2, err := Load(writeCfg(t, sample+"\nworker:\n  dry_run: true\n  trash_dir: /config/trash\n  trash_retention: 48h\n"), "all")
	if err != nil {
		t.Fatal(err)
	}
	if !c2.Worker.DryRun || c2.Worker.TrashDir != "/config/trash" || c2.Worker.TrashRetention != 48*time.Hour {
		t.Errorf("yaml not applied: %+v", c2.Worker)
	}
	t.Setenv("CODECSMITH_DRY_RUN", "false")
	t.Setenv("CODECSMITH_TRASH_DIR", "/other")
	c3, _ := Load(writeCfg(t, sample+"\nworker:\n  dry_run: true\n"), "all")
	if c3.Worker.DryRun || c3.Worker.TrashDir != "/other" {
		t.Errorf("env must win over file: %+v", c3.Worker)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("CODECSMITH_DB_DSN", "postgres://u:p@h/db")
	t.Setenv("CODECSMITH_API_KEY", "secret")
	t.Setenv("CODECSMITH_MAX_CONCURRENT", "7")
	c, err := Load(writeCfg(t, sample), "web")
	if err != nil {
		t.Fatal(err)
	}
	if c.Database.Driver != "postgres" {
		t.Errorf("postgres DSN should imply driver, got %s", c.Database.Driver)
	}
	if c.Web.APIKey != "secret" || c.Worker.MaxConcurrent != 7 {
		t.Errorf("env overrides not applied: %+v", c.Web)
	}
	if r := c.Redacted(); r.Web.APIKey != "***" || r.Database.DSN != "postgres://***@h/db" {
		t.Errorf("redaction: %+v %s", r.Web, r.Database.DSN)
	}
}

func TestValidate(t *testing.T) {
	bad := `
libraries:
  - name: A
    path: /a
    profile: missing
  - name: A
    path: /b
profiles:
  weird:
    speed: warp
    audio_codec: mp3
`
	if _, err := Load(writeCfg(t, bad), "worker"); err == nil {
		t.Fatal("expected validation errors")
	}
	if _, err := Load(writeCfg(t, "web: {listen: ':1'}"), "worker"); err == nil {
		t.Fatal("worker mode needs a library")
	}
	if _, err := Load(writeCfg(t, "web: {listen: ':1'}"), "web"); err != nil {
		t.Fatalf("web mode without libraries should load: %v", err)
	}
}
