// Package encoder abstracts the video encoder backend so the same pipeline
// runs on NVIDIA (NVENC), Intel (QSV), Linux VAAPI, Apple VideoToolbox, or
// pure software (libx265 / SVT-AV1).
//
// Each backend translates the profile's backend-neutral knobs — quality
// (CRF/CQ scale), speed (fast/medium/slow), max bitrate, tune — into the
// flags that encoder understands.
package encoder

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Backend names.
const (
	NVENC        = "nvenc"
	QSV          = "qsv"
	VAAPI        = "vaapi"
	VideoToolbox = "videotoolbox"
	Software     = "software"
)

// Codec names.
const (
	HEVC = "hevc"
	AV1  = "av1"
)

// Params are the backend-neutral encode settings for one job.
type Params struct {
	Codec      string // hevc | av1
	Quality    int    // CRF/CQ scale 0-51 (lower = better)
	Speed      string // fast | medium | slow
	Tune       string // "" | animation | film | grain
	MaxBitrate string // e.g. "4M" ("" = unconstrained)
	Threads    int    // decoder/software-encoder threads (0 = default)
}

// Encoder builds ffmpeg arguments for a backend.
type Encoder interface {
	// Name is the backend identifier.
	Name() string
	// EncoderName returns the ffmpeg -c:v value for the codec.
	EncoderName(codec string) string
	// Supports reports whether the codec is available on this backend.
	Supports(codec string) bool
	// InputArgs are hardware-decode flags placed before -i. May be empty.
	InputArgs() []string
	// FallbackInputArgs are used when a run with InputArgs fails (e.g. the
	// source codec has no hardware decoder). Software decode, hardware encode.
	FallbackInputArgs() []string
	// FilterArgs returns -vf arguments needed when decoding in software
	// (upload to the device) — empty for most backends.
	FilterArgs(hwDecode bool) []string
	// VideoArgs are the -c:v and rate-control flags.
	VideoArgs(p Params) []string
	// Probe checks the backend really works by encoding a null frame.
	Probe(ctx context.Context, ffmpeg string) error
}

// Options tune backend construction.
type Options struct {
	FFmpeg      string
	VAAPIDevice string
	// Available is the set of encoder names ffmpeg reports (from Detect).
	Available map[string]bool
}

// Detect lists ffmpeg's compiled-in encoders.
func Detect(ctx context.Context, ffmpeg string) (map[string]bool, error) {
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(pctx, ffmpeg, "-hide_banner", "-encoders").Output()
	if err != nil {
		return nil, fmt.Errorf("%s -encoders: %w", ffmpeg, err)
	}
	return parseEncoders(string(out)), nil
}

func parseEncoders(out string) map[string]bool {
	avail := map[string]bool{}
	started := false
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "------") {
			started = true
			continue
		}
		if !started {
			continue
		}
		f := strings.Fields(line)
		if len(f) >= 2 && strings.HasPrefix(f[0], "V") {
			avail[f[1]] = true
		}
	}
	return avail
}

// Order is the auto-detect preference.
var Order = []string{NVENC, QSV, VAAPI, VideoToolbox, Software}

// New constructs a backend by name. "auto" picks the first backend whose
// encoder for the requested codec is compiled in AND passes a probe.
func New(ctx context.Context, backend, codec string, opt Options) (Encoder, error) {
	if opt.FFmpeg == "" {
		opt.FFmpeg = "ffmpeg"
	}
	if opt.Available == nil {
		avail, err := Detect(ctx, opt.FFmpeg)
		if err != nil {
			return nil, err
		}
		opt.Available = avail
	}
	if backend != "auto" && backend != "" {
		e := construct(backend, opt)
		if e == nil {
			return nil, fmt.Errorf("unknown encoder backend %q", backend)
		}
		if !e.Supports(codec) {
			return nil, fmt.Errorf("backend %s: ffmpeg has no %s encoder (%s)", backend, codec, e.EncoderName(codec))
		}
		return e, nil
	}
	var errs []error
	for _, name := range Order {
		e := construct(name, opt)
		if e == nil || !e.Supports(codec) {
			continue
		}
		if err := e.Probe(ctx, opt.FFmpeg); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		return e, nil
	}
	return nil, fmt.Errorf("no working %s encoder found: %w", codec, errors.Join(errs...))
}

func construct(name string, opt Options) Encoder {
	switch name {
	case NVENC:
		return &nvenc{avail: opt.Available}
	case QSV:
		return &qsv{avail: opt.Available, device: opt.VAAPIDevice}
	case VAAPI:
		return &vaapi{avail: opt.Available, device: opt.VAAPIDevice}
	case VideoToolbox:
		return &videotoolbox{avail: opt.Available}
	case Software:
		return &software{avail: opt.Available}
	}
	return nil
}

// probeEncode runs a 0.1s null-source encode through the backend. It is the
// definitive "does the GPU actually work right now" test: library presence
// and device nodes can both look fine while the driver is broken.
func probeEncode(ctx context.Context, ffmpeg string, e Encoder, codec string) error {
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	// Software decode of the test pattern, hardware encode.
	args = append(args, "-f", "lavfi", "-i", "nullsrc=s=256x256:r=25:d=0.2")
	if vf := e.FilterArgs(false); len(vf) > 0 {
		args = append(args, vf...)
	}
	args = append(args, e.VideoArgs(Params{Codec: codec, Quality: 30, Speed: "fast"})...)
	args = append(args, "-frames:v", "3", "-f", "null", "-")
	cmd := exec.CommandContext(pctx, ffmpeg, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		return fmt.Errorf("probe failed: %v: %s", err, msg)
	}
	return nil
}

func firstSupported(avail map[string]bool, names ...string) string {
	for _, n := range names {
		if avail[n] {
			return n
		}
	}
	return ""
}

// ----------------------------------------------------------------------------
// NVENC
// ----------------------------------------------------------------------------

type nvenc struct{ avail map[string]bool }

func (n *nvenc) Name() string { return NVENC }
func (n *nvenc) EncoderName(codec string) string {
	return map[string]string{HEVC: "hevc_nvenc", AV1: "av1_nvenc"}[codec]
}
func (n *nvenc) Supports(codec string) bool { return n.avail[n.EncoderName(codec)] }
func (n *nvenc) InputArgs() []string {
	return []string{"-hwaccel", "cuda", "-hwaccel_output_format", "cuda"}
}
func (n *nvenc) FallbackInputArgs() []string { return nil }
func (n *nvenc) FilterArgs(bool) []string    { return nil }
func (n *nvenc) VideoArgs(p Params) []string {
	preset := map[string]string{"fast": "p3", "medium": "p5", "slow": "p7"}[p.Speed]
	if preset == "" {
		preset = "p5"
	}
	args := []string{
		"-c:v", n.EncoderName(p.Codec),
		"-preset", preset,
		"-tune", "hq",
		"-rc", "vbr",
		"-cq", strconv.Itoa(p.Quality),
		"-b:v", "0",
		"-spatial-aq", "1",
		"-temporal-aq", "1",
	}
	if p.MaxBitrate != "" {
		args = append(args, "-maxrate", p.MaxBitrate, "-bufsize", doubleRate(p.MaxBitrate))
	}
	if p.Codec == HEVC {
		args = append(args, "-tag:v", "hvc1")
	}
	return args
}
func (n *nvenc) Probe(ctx context.Context, ffmpeg string) error {
	codec := HEVC
	if !n.Supports(HEVC) {
		codec = AV1
	}
	return probeEncode(ctx, ffmpeg, n, codec)
}

// ----------------------------------------------------------------------------
// Intel Quick Sync (oneVPL / libmfx)
// ----------------------------------------------------------------------------

type qsv struct {
	avail  map[string]bool
	device string
}

func (q *qsv) Name() string { return QSV }
func (q *qsv) EncoderName(codec string) string {
	return map[string]string{HEVC: "hevc_qsv", AV1: "av1_qsv"}[codec]
}
func (q *qsv) Supports(codec string) bool { return q.avail[q.EncoderName(codec)] }
func (q *qsv) InputArgs() []string {
	args := []string{"-hwaccel", "qsv", "-hwaccel_output_format", "qsv"}
	if q.device != "" {
		args = append(args, "-qsv_device", q.device)
	}
	return args
}
func (q *qsv) FallbackInputArgs() []string {
	if q.device != "" {
		return []string{"-init_hw_device", "qsv=hw:" + q.device, "-filter_hw_device", "hw"}
	}
	return []string{"-init_hw_device", "qsv=hw", "-filter_hw_device", "hw"}
}
func (q *qsv) FilterArgs(hwDecode bool) []string {
	if hwDecode {
		return nil
	}
	return []string{"-vf", "format=nv12,hwupload=extra_hw_frames=64"}
}
func (q *qsv) VideoArgs(p Params) []string {
	preset := map[string]string{"fast": "veryfast", "medium": "medium", "slow": "slower"}[p.Speed]
	if preset == "" {
		preset = "medium"
	}
	args := []string{
		"-c:v", q.EncoderName(p.Codec),
		"-preset", preset,
		"-global_quality", strconv.Itoa(p.Quality),
	}
	if p.MaxBitrate != "" {
		// ICQ can't be capped; use VBR with a cap instead.
		args = append(args, "-b:v", halfRate(p.MaxBitrate), "-maxrate", p.MaxBitrate, "-bufsize", doubleRate(p.MaxBitrate))
	} else if p.Codec == HEVC {
		args = append(args, "-look_ahead", "1")
	}
	return args
}
func (q *qsv) Probe(ctx context.Context, ffmpeg string) error {
	codec := HEVC
	if !q.Supports(HEVC) {
		codec = AV1
	}
	return probeEncode(ctx, ffmpeg, q, codec)
}

// ----------------------------------------------------------------------------
// VAAPI (Intel/AMD on Linux)
// ----------------------------------------------------------------------------

type vaapi struct {
	avail  map[string]bool
	device string
}

func (v *vaapi) Name() string { return VAAPI }
func (v *vaapi) EncoderName(codec string) string {
	return map[string]string{HEVC: "hevc_vaapi", AV1: "av1_vaapi"}[codec]
}
func (v *vaapi) Supports(codec string) bool { return v.avail[v.EncoderName(codec)] }
func (v *vaapi) dev() string {
	if v.device == "" {
		return "/dev/dri/renderD128"
	}
	return v.device
}
func (v *vaapi) InputArgs() []string {
	return []string{"-hwaccel", "vaapi", "-hwaccel_output_format", "vaapi", "-vaapi_device", v.dev()}
}
func (v *vaapi) FallbackInputArgs() []string { return []string{"-vaapi_device", v.dev()} }
func (v *vaapi) FilterArgs(hwDecode bool) []string {
	if hwDecode {
		return nil
	}
	return []string{"-vf", "format=nv12,hwupload"}
}
func (v *vaapi) VideoArgs(p Params) []string {
	args := []string{
		"-c:v", v.EncoderName(p.Codec),
		"-rc_mode", "ICQ",
		"-global_quality", strconv.Itoa(p.Quality),
	}
	if p.MaxBitrate != "" {
		args = append(args, "-rc_mode", "VBR", "-b:v", halfRate(p.MaxBitrate), "-maxrate", p.MaxBitrate)
	}
	if p.Codec == HEVC {
		args = append(args, "-tag:v", "hvc1")
	}
	return args
}
func (v *vaapi) Probe(ctx context.Context, ffmpeg string) error {
	codec := HEVC
	if !v.Supports(HEVC) {
		codec = AV1
	}
	return probeEncode(ctx, ffmpeg, v, codec)
}

// ----------------------------------------------------------------------------
// Apple VideoToolbox
// ----------------------------------------------------------------------------

type videotoolbox struct{ avail map[string]bool }

func (v *videotoolbox) Name() string { return VideoToolbox }
func (v *videotoolbox) EncoderName(codec string) string {
	return map[string]string{HEVC: "hevc_videotoolbox"}[codec]
}
func (v *videotoolbox) Supports(codec string) bool {
	return codec == HEVC && v.avail["hevc_videotoolbox"]
}
func (v *videotoolbox) InputArgs() []string         { return []string{"-hwaccel", "videotoolbox"} }
func (v *videotoolbox) FallbackInputArgs() []string { return nil }
func (v *videotoolbox) FilterArgs(bool) []string    { return nil }
func (v *videotoolbox) VideoArgs(p Params) []string {
	// VideoToolbox quality is 1-100 (higher = better); map CRF-ish 0-51.
	q := 100 - int(float64(p.Quality)*1.6)
	if q < 20 {
		q = 20
	}
	if q > 95 {
		q = 95
	}
	args := []string{"-c:v", "hevc_videotoolbox", "-q:v", strconv.Itoa(q), "-tag:v", "hvc1"}
	if p.MaxBitrate != "" {
		args = append(args, "-maxrate", p.MaxBitrate, "-bufsize", doubleRate(p.MaxBitrate))
	}
	return args
}
func (v *videotoolbox) Probe(ctx context.Context, ffmpeg string) error {
	return probeEncode(ctx, ffmpeg, v, HEVC)
}

// ----------------------------------------------------------------------------
// Software (libx265 / SVT-AV1)
// ----------------------------------------------------------------------------

type software struct{ avail map[string]bool }

func (s *software) Name() string { return Software }
func (s *software) EncoderName(codec string) string {
	switch codec {
	case HEVC:
		return firstSupported(s.avail, "libx265")
	case AV1:
		return firstSupported(s.avail, "libsvtav1", "libaom-av1")
	}
	return ""
}
func (s *software) Supports(codec string) bool  { return s.EncoderName(codec) != "" }
func (s *software) InputArgs() []string         { return nil }
func (s *software) FallbackInputArgs() []string { return nil }
func (s *software) FilterArgs(bool) []string    { return nil }
func (s *software) VideoArgs(p Params) []string {
	enc := s.EncoderName(p.Codec)
	args := []string{"-c:v", enc, "-crf", strconv.Itoa(p.Quality)}
	switch enc {
	case "libx265":
		preset := map[string]string{"fast": "fast", "medium": "medium", "slow": "slow"}[p.Speed]
		if preset == "" {
			preset = "medium"
		}
		args = append(args, "-preset", preset, "-tag:v", "hvc1")
		x265 := []string{"log-level=error"}
		if p.MaxBitrate != "" {
			x265 = append(x265, "vbv-maxrate="+kbps(p.MaxBitrate), "vbv-bufsize="+kbps(doubleRate(p.MaxBitrate)))
		}
		if p.Tune == "animation" || p.Tune == "grain" {
			args = append(args, "-tune", p.Tune)
		}
		args = append(args, "-x265-params", strings.Join(x265, ":"))
	case "libsvtav1":
		preset := map[string]string{"fast": "10", "medium": "8", "slow": "5"}[p.Speed]
		if preset == "" {
			preset = "8"
		}
		args = append(args, "-preset", preset)
		if p.MaxBitrate != "" {
			args = append(args, "-maxrate", p.MaxBitrate, "-bufsize", doubleRate(p.MaxBitrate))
		}
		if p.Tune == "animation" {
			args = append(args, "-svtav1-params", "tune=0")
		}
	case "libaom-av1":
		cpu := map[string]string{"fast": "8", "medium": "6", "slow": "4"}[p.Speed]
		if cpu == "" {
			cpu = "6"
		}
		args = append(args, "-cpu-used", cpu, "-b:v", "0", "-row-mt", "1")
	}
	if p.Threads > 0 {
		args = append(args, "-threads", strconv.Itoa(p.Threads))
	}
	return args
}
func (s *software) Probe(ctx context.Context, ffmpeg string) error {
	codec := HEVC
	if !s.Supports(HEVC) {
		codec = AV1
	}
	return probeEncode(ctx, ffmpeg, s, codec)
}

// ----------------------------------------------------------------------------
// bitrate helpers
// ----------------------------------------------------------------------------

// parseRate turns "4M", "4000k", "4000000" into bits per second.
func parseRate(s string) int64 {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "m"):
		mult = 1_000_000
		s = strings.TrimSuffix(s, "m")
	case strings.HasSuffix(s, "k"):
		mult = 1_000
		s = strings.TrimSuffix(s, "k")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(f * float64(mult))
}

func formatRate(bps int64) string { return strconv.FormatInt(bps/1000, 10) + "k" }
func doubleRate(s string) string  { return formatRate(parseRate(s) * 2) }
func halfRate(s string) string    { return formatRate(parseRate(s) / 2) }
func kbps(s string) string        { return strconv.FormatInt(parseRate(s)/1000, 10) }
