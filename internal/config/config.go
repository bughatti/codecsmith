// Package config loads the codecsmith configuration from a YAML file with
// environment-variable overrides for the values that are typically secrets
// or differ per deployment (database DSN, API keys, listen address).
//
// Resolution order (later wins):
//  1. built-in defaults
//  2. config.yaml (path from --config, $CODECSMITH_CONFIG, or ./config.yaml)
//  3. environment variables (CODECSMITH_*, SABNZBD_*)
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the fully-resolved runtime configuration.
type Config struct {
	// DataDir holds the SQLite database (when used) and runtime state.
	DataDir string `yaml:"data_dir"`
	// LogDir holds transcoder.log (rotated). Empty disables file logging.
	LogDir string `yaml:"log_dir"`
	// LogLevel is debug|info|warn|error.
	LogLevel string `yaml:"log_level"`

	Database     Database           `yaml:"database"`
	Web          Web                `yaml:"web"`
	Worker       Worker             `yaml:"worker"`
	Encoder      Encoder            `yaml:"encoder"`
	Libraries    []Library          `yaml:"libraries"`
	Profiles     map[string]Profile `yaml:"profiles"`
	Integrations Integrations       `yaml:"integrations"`

	// Mode is set from the --mode flag: web | worker | all | migrate.
	Mode string `yaml:"-"`
	// Path is where the config file was loaded from ("" if none).
	Path string `yaml:"-"`
}

// Database selects the storage backend.
type Database struct {
	// Driver is "sqlite" (default, zero-setup) or "postgres".
	Driver string `yaml:"driver"`
	// DSN is a file path for sqlite (relative to data_dir) or a
	// postgres:// URL. Override with $CODECSMITH_DB_DSN.
	DSN string `yaml:"dsn"`
}

// Web configures the HTTP dashboard/API.
type Web struct {
	Listen string `yaml:"listen"`
	// APIKey, when non-empty, is required (X-API-Key header or ?api_key=)
	// for every state-changing request and for webhooks.
	APIKey string `yaml:"api_key"`
	// Title is shown in the dashboard header.
	Title string `yaml:"title"`
}

// Worker configures the background processing loop.
type Worker struct {
	ID string `yaml:"id"`
	// MaxConcurrent caps simultaneous ffmpeg processes across all libraries.
	MaxConcurrent int `yaml:"max_concurrent"`
	// TempDir is where in-progress outputs are written. A tmpfs is ideal.
	TempDir string `yaml:"temp_dir"`
	// ScanInterval is how often libraries are re-walked for new files.
	ScanInterval time.Duration `yaml:"scan_interval"`
	// MinFileAge: files modified more recently than this are skipped so
	// downloaders/importers have finished writing them.
	MinFileAge time.Duration `yaml:"min_file_age"`
	// StaleAfter: a job with no progress update for this long is failed.
	StaleAfter time.Duration `yaml:"stale_after"`
	// MetricsInterval is how often CPU/memory/disk samples are recorded.
	MetricsInterval time.Duration `yaml:"metrics_interval"`
	// MetricsRetention prunes metric rows older than this.
	MetricsRetention time.Duration `yaml:"metrics_retention"`
	// GPUHealthCheck runs a null-frame encode periodically and exits the
	// process (so a supervisor restarts it) after repeated failures. Only
	// meaningful for hardware backends.
	GPUHealthCheck bool `yaml:"gpu_health_check"`
	// Nice lowers ffmpeg's CPU priority (0-19).
	Nice int `yaml:"nice"`
	// Threads limits ffmpeg's decoder/filter threads (0 = ffmpeg default).
	Threads int `yaml:"threads"`
	// AllowHardlinked processes files that have more than one hard link.
	// Off by default: replacing such a file breaks the link, so a torrent
	// client still seeding the other copy keeps the original on disk and the
	// library uses twice the space.
	AllowHardlinked bool `yaml:"allow_hardlinked"`
}

// Encoder selects the hardware/software backend.
type Encoder struct {
	// Backend: auto | nvenc | qsv | vaapi | videotoolbox | software.
	Backend string `yaml:"backend"`
	// Codec is the default output codec: hevc | av1. Profiles may override.
	Codec string `yaml:"codec"`
	// VAAPIDevice is the DRM render node for vaapi/qsv.
	VAAPIDevice string `yaml:"vaapi_device"`
	// FFmpeg and FFprobe binary names/paths.
	FFmpeg  string `yaml:"ffmpeg"`
	FFprobe string `yaml:"ffprobe"`
}

// Library is a directory tree to watch.
type Library struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
	// Profile names an entry in profiles (default: "default").
	Profile string `yaml:"profile"`
	// Priority orders the queue across libraries (higher first).
	Priority int `yaml:"priority"`
	// MaxConcurrent caps jobs from this library (0 = worker.max_concurrent).
	MaxConcurrent int `yaml:"max_concurrent"`
	// Enabled=false keeps the library configured but skips scanning.
	Enabled *bool `yaml:"enabled"`
}

// IsEnabled treats a nil Enabled as true.
func (l Library) IsEnabled() bool { return l.Enabled == nil || *l.Enabled }

// Profile is a set of encoding parameters.
type Profile struct {
	// Codec overrides encoder.codec for this profile (hevc|av1).
	Codec string `yaml:"codec"`
	// Quality is the constant-quality value (CRF/CQ scale, lower = better,
	// typically 20-32). Used when the source codec differs from the target.
	Quality int `yaml:"quality"`
	// QualitySameCodec is used when the source is already in the target
	// codec (a re-encode to shrink an oversized file). 0 = Quality+4.
	QualitySameCodec int `yaml:"quality_same_codec"`
	// Speed: fast | medium | slow (mapped to each backend's preset scale).
	Speed string `yaml:"speed"`
	// Tune: "" | animation | film | grain — passed where the backend supports it.
	Tune string `yaml:"tune"`
	// MaxBitrate caps the encoder (e.g. "4M"). Empty = unconstrained.
	MaxBitrate string `yaml:"max_bitrate"`
	// SizeLimitGB: files already in the target codec are only re-encoded
	// when larger than this. 0 = never re-encode same-codec files.
	SizeLimitGB float64 `yaml:"size_limit_gb"`
	// SkipBelowKbps skips sources whose overall bitrate is already under
	// this value (they rarely shrink). 0 = disabled.
	SkipBelowKbps int `yaml:"skip_below_kbps"`
	// AllowDolbyVision re-encodes Dolby Vision sources anyway. Off by
	// default: the DV enhancement data lives in the video bitstream and
	// cannot survive a re-encode, yet the DV label is copied to the output,
	// leaving a file that claims Dolby Vision with nothing behind it. Such a
	// file can tone-map wrongly on a DV display. HDR10 is unaffected and is
	// always preserved.
	AllowDolbyVision bool `yaml:"allow_dolby_vision"`
	// SkipCodecs are source codecs left alone even though they differ from
	// the target, because re-encoding them would not save space (an AV1
	// source converted to HEVC almost always grows). Default [av1].
	SkipCodecs []string `yaml:"skip_codecs"`
	// AudioCodec: copy | aac | opus. "copy" keeps the source audio untouched.
	AudioCodec string `yaml:"audio_codec"`
	// AudioBitrate for re-encoded audio ("192k"). AudioChannels 0 = keep.
	AudioBitrate  string `yaml:"audio_bitrate"`
	AudioChannels int    `yaml:"audio_channels"`
	// AudioLanguages to keep (ISO 639-2, e.g. eng, jpn). Empty = keep all.
	// If none of the listed languages is found every track is kept, so a
	// wanted track is never silently dropped by a mislabeled release.
	AudioLanguages []string `yaml:"audio_languages"`
	// SubtitleLanguages to keep (soft subs are copied, never burned in).
	// Empty = keep all. "und" matches untagged tracks.
	SubtitleLanguages []string `yaml:"subtitle_languages"`
	// Container for the output: mkv (default) or mp4.
	Container string `yaml:"container"`
}

// Integrations configures optional external services.
type Integrations struct {
	SABnzbd SABnzbd `yaml:"sabnzbd"`
	// WebhookSecret protects /api/webhook/* when set (?key=... or
	// X-API-Key). Falls back to web.api_key.
	WebhookSecret string `yaml:"webhook_secret"`
}

// SABnzbd polls the download queue for the dashboard.
type SABnzbd struct {
	Enabled bool   `yaml:"enabled"`
	URL     string `yaml:"url"`
	APIKey  string `yaml:"api_key"`
}

// Defaults returns the built-in configuration.
func Defaults() *Config {
	return &Config{
		DataDir:  "data",
		LogDir:   "logs",
		LogLevel: "info",
		Database: Database{Driver: "sqlite", DSN: "codecsmith.db"},
		Web:      Web{Listen: "0.0.0.0:8090", Title: "Codecsmith"},
		Worker: Worker{
			ID:               "worker-1",
			MaxConcurrent:    2,
			TempDir:          filepath.Join(os.TempDir(), "codecsmith"),
			ScanInterval:     time.Hour,
			MinFileAge:       5 * time.Minute,
			StaleAfter:       2 * time.Hour,
			MetricsInterval:  time.Minute,
			MetricsRetention: 7 * 24 * time.Hour,
			GPUHealthCheck:   true,
			Nice:             10,
		},
		Encoder: Encoder{
			Backend:     "auto",
			Codec:       "hevc",
			VAAPIDevice: "/dev/dri/renderD128",
			FFmpeg:      "ffmpeg",
			FFprobe:     "ffprobe",
		},
		Profiles: map[string]Profile{
			"default": DefaultProfile(),
		},
	}
}

// DefaultProfile is a sensible general-purpose profile.
func DefaultProfile() Profile {
	return Profile{
		Quality:           28,
		Speed:             "medium",
		SizeLimitGB:       5,
		AudioCodec:        "aac",
		AudioBitrate:      "192k",
		AudioChannels:     2,
		AudioLanguages:    []string{"eng"},
		SubtitleLanguages: []string{"eng", "und"},
		SkipCodecs:        []string{"av1"},
		Container:         "mkv",
	}
}

// Load reads the config file (if any), applies env overrides and validates.
func Load(path, mode string) (*Config, error) {
	c := Defaults()
	c.Mode = mode

	if path == "" {
		path = os.Getenv("CODECSMITH_CONFIG")
	}
	if path == "" {
		for _, cand := range []string{"config.yaml", "config.yml", "/config/config.yaml", "/etc/codecsmith/config.yaml"} {
			if _, err := os.Stat(cand); err == nil {
				path = cand
				break
			}
		}
	}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
		if err := yaml.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
		c.Path = path
	}

	c.applyEnv()
	c.fillProfiles()

	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// applyEnv overlays environment variables on top of file values.
func (c *Config) applyEnv() {
	str := func(k string, dst *string) {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			*dst = v
		}
	}
	num := func(k string, dst *int) {
		if v, ok := os.LookupEnv(k); ok {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}
	boolean := func(k string, dst *bool) {
		if v, ok := os.LookupEnv(k); ok {
			switch strings.ToLower(v) {
			case "1", "true", "yes", "on":
				*dst = true
			case "0", "false", "no", "off":
				*dst = false
			}
		}
	}

	str("CODECSMITH_DATA_DIR", &c.DataDir)
	str("CODECSMITH_LOG_DIR", &c.LogDir)
	str("CODECSMITH_LOG_LEVEL", &c.LogLevel)
	str("LOG_LEVEL", &c.LogLevel)
	str("CODECSMITH_DB_DRIVER", &c.Database.Driver)
	str("CODECSMITH_DB_DSN", &c.Database.DSN)
	str("CODECSMITH_WEB_LISTEN", &c.Web.Listen)
	str("CODECSMITH_API_KEY", &c.Web.APIKey)
	str("CODECSMITH_WORKER_ID", &c.Worker.ID)
	num("CODECSMITH_MAX_CONCURRENT", &c.Worker.MaxConcurrent)
	str("CODECSMITH_TEMP_DIR", &c.Worker.TempDir)
	str("CODECSMITH_ENCODER", &c.Encoder.Backend)
	str("CODECSMITH_CODEC", &c.Encoder.Codec)
	str("CODECSMITH_VAAPI_DEVICE", &c.Encoder.VAAPIDevice)
	boolean("SABNZBD_ENABLED", &c.Integrations.SABnzbd.Enabled)
	str("SABNZBD_URL", &c.Integrations.SABnzbd.URL)
	str("SABNZBD_API_KEY", &c.Integrations.SABnzbd.APIKey)
	str("CODECSMITH_WEBHOOK_SECRET", &c.Integrations.WebhookSecret)

	// Postgres DSN implies the postgres driver unless explicitly set.
	if strings.HasPrefix(c.Database.DSN, "postgres://") || strings.HasPrefix(c.Database.DSN, "postgresql://") {
		if os.Getenv("CODECSMITH_DB_DRIVER") == "" && c.Database.Driver == "sqlite" {
			c.Database.Driver = "postgres"
		}
	}
}

// fillProfiles ensures every profile has complete values by layering it over
// the default profile, and that a "default" profile exists.
func (c *Config) fillProfiles() {
	if c.Profiles == nil {
		c.Profiles = map[string]Profile{}
	}
	base := DefaultProfile()
	if p, ok := c.Profiles["default"]; ok {
		base = mergeProfile(base, p)
	}
	c.Profiles["default"] = base
	for name, p := range c.Profiles {
		if name == "default" {
			continue
		}
		c.Profiles[name] = mergeProfile(base, p)
	}
}

func mergeProfile(base, over Profile) Profile {
	out := base
	if over.Codec != "" {
		out.Codec = over.Codec
	}
	if over.Quality != 0 {
		out.Quality = over.Quality
	}
	if over.QualitySameCodec != 0 {
		out.QualitySameCodec = over.QualitySameCodec
	}
	if over.Speed != "" {
		out.Speed = over.Speed
	}
	if over.Tune != "" {
		out.Tune = over.Tune
	}
	if over.MaxBitrate != "" {
		out.MaxBitrate = over.MaxBitrate
	}
	if over.SizeLimitGB != 0 {
		out.SizeLimitGB = over.SizeLimitGB
	}
	if over.SkipBelowKbps != 0 {
		out.SkipBelowKbps = over.SkipBelowKbps
	}
	if over.AudioCodec != "" {
		out.AudioCodec = over.AudioCodec
	}
	if over.AudioBitrate != "" {
		out.AudioBitrate = over.AudioBitrate
	}
	if over.AudioChannels != 0 {
		out.AudioChannels = over.AudioChannels
	}
	if over.AudioLanguages != nil {
		out.AudioLanguages = over.AudioLanguages
	}
	if over.SkipCodecs != nil {
		out.SkipCodecs = over.SkipCodecs
	}
	if over.AllowDolbyVision {
		out.AllowDolbyVision = true
	}
	if over.SubtitleLanguages != nil {
		out.SubtitleLanguages = over.SubtitleLanguages
	}
	if over.Container != "" {
		out.Container = over.Container
	}
	return out
}

// Validate checks cross-field constraints.
func (c *Config) Validate() error {
	var errs []error
	switch c.Database.Driver {
	case "sqlite", "postgres":
	default:
		errs = append(errs, fmt.Errorf("database.driver must be sqlite or postgres, got %q", c.Database.Driver))
	}
	if c.Database.DSN == "" {
		errs = append(errs, errors.New("database.dsn is required"))
	}
	switch c.Encoder.Backend {
	case "auto", "nvenc", "qsv", "vaapi", "videotoolbox", "software":
	default:
		errs = append(errs, fmt.Errorf("encoder.backend %q unknown", c.Encoder.Backend))
	}
	switch c.Encoder.Codec {
	case "hevc", "av1":
	default:
		errs = append(errs, fmt.Errorf("encoder.codec must be hevc or av1, got %q", c.Encoder.Codec))
	}
	if c.Worker.MaxConcurrent < 1 {
		errs = append(errs, errors.New("worker.max_concurrent must be >= 1"))
	}
	seen := map[string]bool{}
	for i, l := range c.Libraries {
		if l.Name == "" {
			errs = append(errs, fmt.Errorf("libraries[%d]: name is required", i))
		}
		if l.Path == "" {
			errs = append(errs, fmt.Errorf("libraries[%d] (%s): path is required", i, l.Name))
		}
		if seen[l.Name] {
			errs = append(errs, fmt.Errorf("libraries: duplicate name %q", l.Name))
		}
		seen[l.Name] = true
		prof := l.Profile
		if prof == "" {
			prof = "default"
			c.Libraries[i].Profile = prof
		}
		if _, ok := c.Profiles[prof]; !ok {
			errs = append(errs, fmt.Errorf("libraries[%d] (%s): profile %q not defined", i, l.Name, prof))
		}
		c.Libraries[i].Path = filepath.Clean(l.Path)
	}
	for name, p := range c.Profiles {
		switch p.Codec {
		case "", "hevc", "av1":
		default:
			errs = append(errs, fmt.Errorf("profiles.%s.codec must be hevc or av1", name))
		}
		switch p.Speed {
		case "fast", "medium", "slow":
		default:
			errs = append(errs, fmt.Errorf("profiles.%s.speed must be fast, medium or slow", name))
		}
		switch p.AudioCodec {
		case "copy", "aac", "opus":
		default:
			errs = append(errs, fmt.Errorf("profiles.%s.audio_codec must be copy, aac or opus", name))
		}
		switch p.Container {
		case "mkv", "mp4":
		default:
			errs = append(errs, fmt.Errorf("profiles.%s.container must be mkv or mp4", name))
		}
		if p.Quality < 0 || p.Quality > 63 {
			errs = append(errs, fmt.Errorf("profiles.%s.quality must be 0-63", name))
		}
	}
	if c.Mode == "worker" || c.Mode == "all" {
		if len(c.Libraries) == 0 {
			errs = append(errs, errors.New("at least one library must be configured"))
		}
	}
	return errors.Join(errs...)
}

// LibraryFor returns the library whose path contains file, choosing the
// longest matching prefix so nested libraries work.
func (c *Config) LibraryFor(file string) (Library, bool) {
	file = filepath.Clean(file)
	best := -1
	for i, l := range c.Libraries {
		if l.Path == "" {
			continue
		}
		rel, err := filepath.Rel(l.Path, file)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if best == -1 || len(l.Path) > len(c.Libraries[best].Path) {
			best = i
		}
	}
	if best == -1 {
		return Library{}, false
	}
	return c.Libraries[best], true
}

// LibraryByName looks a library up by its configured name.
func (c *Config) LibraryByName(name string) (Library, bool) {
	for _, l := range c.Libraries {
		if l.Name == name {
			return l, true
		}
	}
	return Library{}, false
}

// SkipsCodec reports whether a profile leaves sources in codec untouched.
func (p Profile) SkipsCodec(codec string) bool {
	for _, c := range p.SkipCodecs {
		if strings.EqualFold(c, codec) {
			return true
		}
	}
	return false
}

// ProfileFor resolves the profile for a library (never fails after Validate).
func (c *Config) ProfileFor(lib Library) Profile {
	if p, ok := c.Profiles[lib.Profile]; ok {
		return p
	}
	return c.Profiles["default"]
}

// TargetCodec returns the codec a profile encodes to.
func (c *Config) TargetCodec(p Profile) string {
	if p.Codec != "" {
		return p.Codec
	}
	return c.Encoder.Codec
}

// SQLitePath resolves the sqlite DSN relative to data_dir.
func (c *Config) SQLitePath() string {
	if c.Database.Driver != "sqlite" {
		return ""
	}
	if filepath.IsAbs(c.Database.DSN) || strings.HasPrefix(c.Database.DSN, "file:") {
		return c.Database.DSN
	}
	return filepath.Join(c.DataDir, c.Database.DSN)
}

// ProfileNames returns profile names sorted for stable output.
func (c *Config) ProfileNames() []string {
	names := make([]string, 0, len(c.Profiles))
	for n := range c.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Redacted returns a copy safe for logging or the API (secrets blanked).
func (c *Config) Redacted() *Config {
	out := *c
	if out.Web.APIKey != "" {
		out.Web.APIKey = "***"
	}
	if out.Integrations.SABnzbd.APIKey != "" {
		out.Integrations.SABnzbd.APIKey = "***"
	}
	if out.Integrations.WebhookSecret != "" {
		out.Integrations.WebhookSecret = "***"
	}
	if out.Database.Driver == "postgres" {
		out.Database.DSN = redactDSN(out.Database.DSN)
	}
	return &out
}

func redactDSN(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	scheme := strings.Index(dsn, "://")
	if at == -1 || scheme == -1 || at < scheme {
		return dsn
	}
	return dsn[:scheme+3] + "***" + dsn[at:]
}
