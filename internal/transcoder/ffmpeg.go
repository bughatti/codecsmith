// Package transcoder runs ffmpeg over a single job.
//
// Pipeline per job:
//  1. ffprobe the source: codec, duration, bitrate, audio/subtitle streams.
//  2. Apply the profile's skip rules (already efficient, unsupported codec).
//  3. Build the ffmpeg command from the encoder backend + profile.
//  4. Run with -progress on stdout for percent/speed/fps/ETA; keep a
//     stderr tail for error messages. If hardware decode fails, retry once
//     with software decode.
//  5. Never-grow rule: if the output is not smaller than the source the
//     source is kept untouched and the job is marked skipped.
//  6. Truncation guard: an output noticeably shorter than the source is
//     rejected (ffmpeg can exit 0 on a broken source).
//  7. Swap the file in place: source → .backup, output → source, remove
//     backup. Cross-filesystem moves (tmpfs → NAS) are handled.
package transcoder

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bughatti/codecsmith/internal/config"
	"github.com/bughatti/codecsmith/internal/encoder"
	"github.com/bughatti/codecsmith/internal/job"
)

// ProgressFunc receives periodic updates. Returning false aborts the encode.
type ProgressFunc func(p Progress) (keepGoing bool)

// Result is the outcome of one job.
type Result struct {
	Status      job.Status
	NewSize     int64
	TargetCodec string
	// Message explains non-completed outcomes and is stored on the job.
	Message string
}

// ErrCancelled is returned when the progress callback asked to stop.
var ErrCancelled = errors.New("cancelled")

// Transcoder executes jobs.
type Transcoder struct {
	cfg *config.Config
	enc encoder.Encoder
	log *slog.Logger

	mu      sync.Mutex
	running map[int64]*exec.Cmd
}

// New wires a Transcoder to a backend.
func New(cfg *config.Config, enc encoder.Encoder, log *slog.Logger) *Transcoder {
	return &Transcoder{cfg: cfg, enc: enc, log: log, running: map[int64]*exec.Cmd{}}
}

// Encoder returns the active backend.
func (t *Transcoder) Encoder() encoder.Encoder { return t.enc }

// Kill terminates a running job's ffmpeg process group.
func (t *Transcoder) Kill(jobID int64) bool {
	t.mu.Lock()
	cmd := t.running[jobID]
	t.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return false
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	return true
}

func (t *Transcoder) track(jobID int64, cmd *exec.Cmd) {
	t.mu.Lock()
	if cmd == nil {
		delete(t.running, jobID)
	} else {
		t.running[jobID] = cmd
	}
	t.mu.Unlock()
}

// Transcode runs the full pipeline for a job.
func (t *Transcoder) Transcode(ctx context.Context, j *job.Job, progress ProgressFunc) (*Result, error) {
	log := t.log.With("job_id", j.ID, "file", filepath.Base(j.FilePath))

	lib, ok := t.cfg.LibraryByName(j.Library)
	if !ok {
		if lib, ok = t.cfg.LibraryFor(j.FilePath); !ok {
			return &Result{Status: job.StatusFailed}, fmt.Errorf("no library matches %s", j.FilePath)
		}
	}
	prof := t.cfg.ProfileFor(lib)
	if j.Profile != "" {
		if p, ok := t.cfg.Profiles[j.Profile]; ok {
			prof = p
		}
	}
	target := t.cfg.TargetCodec(prof)
	if !t.enc.Supports(target) {
		return &Result{Status: job.StatusFailed}, fmt.Errorf("encoder %s cannot produce %s", t.enc.Name(), target)
	}

	info, err := os.Stat(j.FilePath)
	if err != nil {
		return &Result{Status: job.StatusFailed}, fmt.Errorf("source missing: %w", err)
	}
	origSize := info.Size()

	// A file with more than one hard link is almost always seeded by a
	// torrent client from a second path. Replacing it breaks the link and
	// costs disk space instead of saving it.
	if n := HardLinks(info); n > 1 && !t.cfg.Worker.AllowHardlinked {
		msg := fmt.Sprintf("kept original: file has %d hard links (still seeding?); set allow_hardlinked to process it", n)
		return &Result{Status: job.StatusSkipped, NewSize: origSize, Message: msg}, nil
	}

	mi, err := Probe(ctx, t.cfg.Encoder.FFprobe, j.FilePath)
	if err != nil {
		return &Result{Status: job.StatusFailed}, fmt.Errorf("ffprobe: %w", err)
	}
	if !IsVideoCodec(mi.VideoCodec) {
		return &Result{Status: job.StatusSkipped, Message: "no video stream"}, nil
	}

	// Skip rules (cheap, before touching the GPU).
	if reason := skipReason(mi, origSize, target, prof); reason != "" {
		return &Result{Status: job.StatusSkipped, NewSize: origSize, Message: reason}, nil
	}

	quality := prof.Quality
	if mi.VideoCodec == target {
		quality = prof.QualitySameCodec
		if quality == 0 {
			quality = prof.Quality + 4
		}
	}

	if err := os.MkdirAll(t.cfg.Worker.TempDir, 0o755); err != nil {
		return &Result{Status: job.StatusFailed}, fmt.Errorf("temp dir: %w", err)
	}
	ext := "." + prof.Container
	tempFile := filepath.Join(t.cfg.Worker.TempDir, fmt.Sprintf("transcode_%d%s", j.ID, ext))
	_ = os.Remove(tempFile)
	defer os.Remove(tempFile)

	audioMaps, audioNote := SelectAudio(mi.Audio, prof.AudioLanguages)
	if audioNote != "" {
		log.Warn("audio selection", "note", audioNote)
	}
	subMaps, subConvert := SelectSubtitles(mi.Subtitles, prof.SubtitleLanguages, prof.Container)

	plan := buildPlan{
		ffmpeg:     t.cfg.Encoder.FFmpeg,
		enc:        t.enc,
		input:      j.FilePath,
		output:     tempFile,
		params:     encoder.Params{Codec: target, Quality: quality, Speed: prof.Speed, Tune: prof.Tune, MaxBitrate: prof.MaxBitrate, Threads: t.cfg.Worker.Threads},
		audioMaps:  audioMaps,
		subMaps:    subMaps,
		subConvert: subConvert,
		container:  prof.Container,
		audio:      prof,
		nice:       t.cfg.Worker.Nice,
	}

	args := plan.args(true)
	log.Info("ffmpeg start", "encoder", t.enc.Name(), "codec", target, "quality", quality, "args", strings.Join(args, " "))
	rc, tail, err := t.run(ctx, j.ID, args, mi.Duration, progress)
	if errors.Is(err, ErrCancelled) || ctx.Err() != nil {
		return &Result{Status: job.StatusCancelled, Message: "cancelled"}, ErrCancelled
	}
	if rc != 0 && len(t.enc.InputArgs()) > 0 && !errors.Is(err, ErrCancelled) {
		// Hardware decode failed (unsupported source codec, 10-bit on an
		// 8-bit decoder, etc). Decode in software, encode in hardware.
		log.Warn("hardware decode failed; retrying with software decode", "rc", rc, "err", lastUsefulErrorLine(tail))
		_ = os.Remove(tempFile)
		args = plan.args(false)
		rc, tail, err = t.run(ctx, j.ID, args, mi.Duration, progress)
		if errors.Is(err, ErrCancelled) || ctx.Err() != nil {
			return &Result{Status: job.StatusCancelled, Message: "cancelled"}, ErrCancelled
		}
	}
	if rc != 0 || err != nil {
		msg := fmt.Sprintf("ffmpeg rc=%d", rc)
		if line := lastUsefulErrorLine(tail); line != "" {
			msg += ": " + truncate(line, 300)
		} else if err != nil {
			msg += ": " + err.Error()
		}
		return &Result{Status: job.StatusFailed, Message: msg}, errors.New(msg)
	}

	st, err := os.Stat(tempFile)
	if err != nil {
		return &Result{Status: job.StatusFailed}, fmt.Errorf("output missing: %w", err)
	}
	newSize := st.Size()

	// Never-grow rule: the original is only replaced by something smaller.
	if newSize >= origSize {
		msg := fmt.Sprintf("kept original: output would be %s (source %s)", humanBytes(newSize), humanBytes(origSize))
		return &Result{Status: job.StatusSkipped, NewSize: origSize, TargetCodec: target, Message: msg}, nil
	}

	// Truncation guard.
	outDur := ProbeDuration(ctx, t.cfg.Encoder.FFprobe, tempFile)
	if mi.Duration > 0 && outDur > 0 && mi.Duration-outDur > 5 && outDur < mi.Duration*0.99 {
		msg := fmt.Sprintf("kept original: output truncated (%.0fs vs source %.0fs)", outDur, mi.Duration)
		log.Warn("truncation guard", "src_sec", mi.Duration, "out_sec", outDur)
		return &Result{Status: job.StatusFailed, Message: msg}, errors.New(msg)
	}

	// Dry run: everything above is real work, only the replacement is
	// withheld. The caller gets the measured saving and the source is
	// untouched.
	if t.cfg.Worker.DryRun {
		msg := fmt.Sprintf("dry run: would save %s (%s → %s); source untouched",
			humanBytes(origSize-newSize), humanBytes(origSize), humanBytes(newSize))
		return &Result{Status: job.StatusSkipped, NewSize: newSize, TargetCodec: target, Message: msg}, nil
	}

	// Install: source → .backup, temp → source (copy across filesystems).
	finalPath := j.FilePath
	if filepath.Ext(finalPath) != ext {
		finalPath = strings.TrimSuffix(finalPath, filepath.Ext(finalPath)) + ext
	}
	backup := j.FilePath + ".backup"
	if err := os.Rename(j.FilePath, backup); err != nil {
		return &Result{Status: job.StatusFailed}, fmt.Errorf("backup: %w", err)
	}
	if err := MoveFile(tempFile, finalPath); err != nil {
		_ = os.Rename(backup, j.FilePath)
		return &Result{Status: job.StatusFailed}, fmt.Errorf("install: %w", err)
	}
	_ = os.Chmod(finalPath, info.Mode().Perm())
	if err := t.retireOriginal(backup, j.FilePath, lib.Path); err != nil {
		// The new file is already in place; the old one is still on disk as
		// .backup. Say so loudly rather than deleting data we were asked to keep.
		log.Error("could not move the original to the trash dir; it is still on disk as .backup",
			"backup", backup, "err", err)
	}
	if finalPath != j.FilePath {
		log.Info("container changed", "from", filepath.Base(j.FilePath), "to", filepath.Base(finalPath))
	}

	return &Result{Status: job.StatusCompleted, NewSize: newSize, TargetCodec: target}, nil
}

// skipReason returns "" when the file should be encoded.
func skipReason(mi *MediaInfo, size int64, target string, prof config.Profile) string {
	// Dolby Vision first: the DV layer cannot survive any re-encode, and
	// ffmpeg copies the DV label to the output regardless, which produces a
	// file that claims Dolby Vision with no data behind it.
	if mi.DolbyVision && !prof.AllowDolbyVision {
		return "Dolby Vision source: re-encoding would strip the DV layer and leave a file that falsely claims DV (set allow_dolby_vision: true to override)"
	}
	if mi.VideoCodec == target {
		limit := int64(prof.SizeLimitGB * (1 << 30))
		if limit <= 0 || size <= limit {
			return fmt.Sprintf("already %s and under size limit", target)
		}
	} else if prof.SkipsCodec(mi.VideoCodec) {
		return fmt.Sprintf("source codec %s is in skip_codecs", mi.VideoCodec)
	}
	if prof.SkipBelowKbps > 0 && mi.BitrateBps > 0 && mi.BitrateBps < int64(prof.SkipBelowKbps)*1000 {
		return fmt.Sprintf("source bitrate %d kbps is below skip_below_kbps (%d)", mi.BitrateBps/1000, prof.SkipBelowKbps)
	}
	return ""
}

// buildPlan holds everything needed to render an ffmpeg command line.
type buildPlan struct {
	ffmpeg     string
	enc        encoder.Encoder
	input      string
	output     string
	params     encoder.Params
	audioMaps  []string
	subMaps    []string
	subConvert bool
	container  string
	audio      config.Profile
	nice       int
}

// args renders the command. hwDecode=false uses the backend's fallback
// (software decode) path.
func (b buildPlan) args(hwDecode bool) []string {
	args := []string{}
	if b.nice > 0 {
		if _, err := exec.LookPath("nice"); err == nil {
			args = append(args, "nice", "-n", strconv.Itoa(b.nice))
		}
	}
	args = append(args, b.ffmpeg, "-hide_banner", "-nostdin", "-y", "-loglevel", "warning",
		"-progress", "pipe:1", "-nostats")
	if hwDecode {
		args = append(args, b.enc.InputArgs()...)
	} else {
		args = append(args, b.enc.FallbackInputArgs()...)
	}
	if b.params.Threads > 0 {
		args = append(args, "-threads", strconv.Itoa(b.params.Threads))
	}
	args = append(args, "-i", b.input)
	args = append(args, "-map", "0:v:0")
	for _, m := range b.audioMaps {
		args = append(args, "-map", m)
	}
	for _, m := range b.subMaps {
		args = append(args, "-map", m)
	}
	// Attachments (fonts for ASS subs) travel with mkv.
	if b.container == "mkv" {
		args = append(args, "-map", "0:t?")
	}
	args = append(args, "-map_metadata", "0", "-map_chapters", "0")
	args = append(args, b.enc.FilterArgs(hwDecode)...)
	args = append(args, b.enc.VideoArgs(b.params)...)

	switch b.audio.AudioCodec {
	case "copy":
		args = append(args, "-c:a", "copy")
	default:
		codec := b.audio.AudioCodec
		if codec == "opus" {
			codec = "libopus"
		}
		args = append(args, "-c:a", codec)
		if b.audio.AudioBitrate != "" {
			args = append(args, "-b:a", b.audio.AudioBitrate)
		}
		if b.audio.AudioChannels > 0 {
			args = append(args, "-ac", strconv.Itoa(b.audio.AudioChannels))
		}
	}
	if len(b.subMaps) > 0 {
		if b.subConvert {
			args = append(args, "-c:s", SubtitleCodecArg(b.container))
		} else {
			args = append(args, "-c:s", "copy")
		}
	}
	if b.container == "mp4" {
		args = append(args, "-movflags", "+faststart")
	}
	args = append(args, "-max_muxing_queue_size", "1024", b.output)
	return args
}

// run executes ffmpeg, streaming progress, and returns (rc, stderr tail, err).
func (t *Transcoder) run(ctx context.Context, jobID int64, args []string, duration float64, progress ProgressFunc) (int, []string, error) {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return -1, nil, err
	}
	if err := cmd.Start(); err != nil {
		return -1, nil, err
	}
	t.track(jobID, cmd)
	defer t.track(jobID, nil)

	// stderr tail collector
	tail := make([]string, 0, 64)
	var tailMu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		sc.Split(splitCRLF)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			tailMu.Lock()
			tail = append(tail, line)
			if len(tail) > 100 {
				tail = tail[len(tail)-100:]
			}
			tailMu.Unlock()
		}
	}()

	// stdout progress
	cancelled := false
	parser := newProgressParser(duration)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	lastEmit := time.Time{}
	for sc.Scan() {
		p, ok := parser.Feed(sc.Text())
		if !ok || progress == nil {
			continue
		}
		if !p.Done && time.Since(lastEmit) < 2*time.Second {
			continue
		}
		lastEmit = time.Now()
		if !progress(p) {
			cancelled = true
			_ = cmd.Cancel()
			break
		}
	}
	// Drain stdout so ffmpeg is never blocked on a full pipe.
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()
	wg.Wait()

	tailMu.Lock()
	lines := append([]string(nil), tail...)
	tailMu.Unlock()

	if cancelled {
		return -1, lines, ErrCancelled
	}
	if ctx.Err() != nil {
		return -1, lines, ctx.Err()
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return exitErr.ExitCode(), lines, nil
		}
		return -1, lines, waitErr
	}
	return 0, lines, nil
}

// lastUsefulErrorLine picks the most explanatory stderr line.
func lastUsefulErrorLine(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		for _, needle := range []string{"Conversion failed", "Function not implemented", "Invalid", "No such",
			"Error", "error", "failed", "not supported", "Cannot", "No NVENC", "CUDA"} {
			if strings.Contains(l, needle) {
				return l
			}
		}
	}
	if len(lines) > 0 {
		return strings.TrimSpace(lines[len(lines)-1])
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// retireOriginal disposes of the replaced source: deleted by default, or
// moved into the trash directory when one is configured.
func (t *Transcoder) retireOriginal(backup, origPath, libraryPath string) error {
	if t.cfg.Worker.TrashDir == "" {
		return os.Remove(backup)
	}
	dest := uniquePath(TrashPath(t.cfg.Worker.TrashDir, libraryPath, origPath))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return MoveFile(backup, dest)
}

// TrashPath mirrors the file's path inside the library under trashDir, so
// two files with the same name in different folders never collide.
func TrashPath(trashDir, libraryPath, filePath string) string {
	rel, err := filepath.Rel(libraryPath, filePath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		rel = filepath.Base(filePath)
	}
	return filepath.Join(trashDir, rel)
}

// uniquePath appends a counter until the path is free.
func uniquePath(p string) string {
	if _, err := os.Stat(p); err != nil {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; i < 1000; i++ {
		cand := fmt.Sprintf("%s.%d%s", base, i, ext)
		if _, err := os.Stat(cand); err != nil {
			return cand
		}
	}
	return p
}

// HardLinks reports how many directory entries point at this file's inode.
// Returns 1 when the platform does not report it.
func HardLinks(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 0 {
		return uint64(st.Nlink)
	}
	return 1
}

// MoveFile renames src to dst, falling back to copy+fsync+delete when the
// two paths are on different filesystems (EXDEV).
func MoveFile(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}
	var le *os.LinkError
	if !errors.As(err, &le) || !errors.Is(le.Err, syscall.EXDEV) {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	return os.Remove(src)
}
