package transcoder

import (
	"bufio"
	"strconv"
	"strings"
)

// Progress is one update parsed from ffmpeg's -progress output.
type Progress struct {
	Percent    float64
	Speed      float64 // multiple of realtime
	FPS        float64
	OutTimeSec float64
	ETASeconds int
	Done       bool
}

// progressParser accumulates the key=value block ffmpeg emits with
// "-progress pipe:1". A block ends at the "progress=continue|end" line.
type progressParser struct {
	duration float64
	cur      map[string]string
}

func newProgressParser(durationSec float64) *progressParser {
	return &progressParser{duration: durationSec, cur: map[string]string{}}
}

// Feed consumes one line; returns a Progress when a block completes.
func (p *progressParser) Feed(line string) (Progress, bool) {
	line = strings.TrimSpace(line)
	k, v, ok := strings.Cut(line, "=")
	if !ok {
		return Progress{}, false
	}
	k = strings.TrimSpace(k)
	v = strings.TrimSpace(v)
	if k != "progress" {
		p.cur[k] = v
		return Progress{}, false
	}
	pr := Progress{Done: v == "end"}
	if us, err := strconv.ParseFloat(p.cur["out_time_us"], 64); err == nil && us > 0 {
		pr.OutTimeSec = us / 1e6
	} else if ms, err := strconv.ParseFloat(p.cur["out_time_ms"], 64); err == nil && ms > 0 {
		// Despite the name, out_time_ms is microseconds in ffmpeg.
		pr.OutTimeSec = ms / 1e6
	} else if t := p.cur["out_time"]; t != "" {
		pr.OutTimeSec = parseClock(t)
	}
	pr.FPS, _ = strconv.ParseFloat(p.cur["fps"], 64)
	pr.Speed, _ = strconv.ParseFloat(strings.TrimSuffix(p.cur["speed"], "x"), 64)
	if p.duration > 0 {
		pr.Percent = pr.OutTimeSec / p.duration * 100
		if pr.Percent > 100 {
			pr.Percent = 100
		}
		if pr.Speed > 0 {
			pr.ETASeconds = int((p.duration - pr.OutTimeSec) / pr.Speed)
			if pr.ETASeconds < 0 {
				pr.ETASeconds = 0
			}
		}
	}
	if pr.Done {
		pr.Percent = 100
		pr.ETASeconds = 0
	}
	p.cur = map[string]string{}
	return pr, true
}

// parseClock parses HH:MM:SS.micro into seconds.
func parseClock(s string) float64 {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0
	}
	h, _ := strconv.ParseFloat(parts[0], 64)
	m, _ := strconv.ParseFloat(parts[1], 64)
	sec, _ := strconv.ParseFloat(parts[2], 64)
	return h*3600 + m*60 + sec
}

// splitCRLF is a bufio.SplitFunc that ends tokens on \n OR \r so ffmpeg's
// carriage-return-updated status lines are delivered individually.
func splitCRLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i, b := range data {
		if b == '\n' || b == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

var _ bufio.SplitFunc = splitCRLF
