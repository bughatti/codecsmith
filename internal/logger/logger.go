// Package logger builds the process-wide slog logger: human-readable text on
// stdout plus an optional rotating log file that the dashboard tails.
package logger

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/natefinch/lumberjack.v2"
)

// FileName is the rotating log file name inside log_dir.
const FileName = "transcoder.log"

// Setup returns a logger writing to stdout and, when logDir is non-empty, to
// logDir/transcoder.log with rotation (10 MiB x 5). The returned closer
// flushes the file writer.
func Setup(component, level, logDir string) (*slog.Logger, io.Closer) {
	lvl := parseLevel(level)
	writers := []io.Writer{os.Stdout}
	var closer io.Closer = nopCloser{}

	if logDir != "" {
		if err := os.MkdirAll(logDir, 0o755); err == nil {
			lj := &lumberjack.Logger{
				Filename:   filepath.Join(logDir, FileName),
				MaxSize:    10, // MiB
				MaxBackups: 5,
				MaxAge:     30, // days
				Compress:   true,
			}
			writers = append(writers, lj)
			closer = lj
		}
	}

	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if strings.ToLower(os.Getenv("LOG_FORMAT")) == "json" {
		h = slog.NewJSONHandler(io.MultiWriter(writers...), opts)
	} else {
		h = slog.NewTextHandler(io.MultiWriter(writers...), opts)
	}
	return slog.New(h).With("component", component), closer
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// Tail returns the last n lines of the log file, or an empty slice if the
// file does not exist. Reads at most the trailing 512 KiB.
func Tail(logDir string, n int) ([]string, error) {
	if logDir == "" {
		return []string{}, nil
	}
	f, err := os.Open(filepath.Join(logDir, FileName))
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	const maxRead = 512 * 1024
	start := st.Size() - maxRead
	if start < 0 {
		start = 0
	}
	buf := make([]byte, st.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if start > 0 && len(lines) > 0 {
		lines = lines[1:] // first line is probably partial
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}
