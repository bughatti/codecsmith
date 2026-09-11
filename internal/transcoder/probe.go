package transcoder

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Stream is a single audio/subtitle/video stream from ffprobe.
type Stream struct {
	Index    int
	Kind     string // video | audio | subtitle
	Codec    string
	Language string // "und" when untagged
	Title    string
	Default  bool
	Forced   bool
}

// MediaInfo is what the pipeline needs to know about a source file.
type MediaInfo struct {
	VideoCodec string
	Duration   float64 // seconds
	BitrateBps int64   // container-level overall bitrate
	Width      int
	Height     int
	Audio      []Stream
	Subtitles  []Stream
}

// Probe runs ffprobe once and extracts everything the pipeline needs.
func Probe(ctx context.Context, ffprobe, input string) (*MediaInfo, error) {
	pctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(pctx, ffprobe, "-v", "error",
		"-show_entries", "format=duration,bit_rate:stream=index,codec_type,codec_name,width,height:stream_tags=language,title:stream_disposition=default,forced",
		"-of", "json", input)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseProbe(out)
}

func parseProbe(out []byte) (*MediaInfo, error) {
	var parsed struct {
		Format struct {
			Duration string `json:"duration"`
			BitRate  string `json:"bit_rate"`
		} `json:"format"`
		Streams []struct {
			Index     int    `json:"index"`
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			Tags      struct {
				Language string `json:"language"`
				Title    string `json:"title"`
			} `json:"tags"`
			Disposition struct {
				Default int `json:"default"`
				Forced  int `json:"forced"`
			} `json:"disposition"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, err
	}
	mi := &MediaInfo{}
	mi.Duration, _ = strconv.ParseFloat(parsed.Format.Duration, 64)
	mi.BitrateBps, _ = strconv.ParseInt(parsed.Format.BitRate, 10, 64)
	for _, s := range parsed.Streams {
		lang := strings.ToLower(strings.TrimSpace(s.Tags.Language))
		if lang == "" {
			lang = "und"
		}
		st := Stream{
			Index: s.Index, Kind: s.CodecType, Codec: s.CodecName, Language: lang,
			Title: s.Tags.Title, Default: s.Disposition.Default == 1, Forced: s.Disposition.Forced == 1,
		}
		switch s.CodecType {
		case "video":
			if mi.VideoCodec == "" {
				mi.VideoCodec = s.CodecName
				mi.Width, mi.Height = s.Width, s.Height
			}
		case "audio":
			mi.Audio = append(mi.Audio, st)
		case "subtitle":
			mi.Subtitles = append(mi.Subtitles, st)
		}
	}
	if mi.VideoCodec == "" {
		mi.VideoCodec = "unknown"
	}
	return mi, nil
}

// ProbeCodec is the cheap scanner variant: video codec only.
func ProbeCodec(ctx context.Context, ffprobe, input string) (string, error) {
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(pctx, ffprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name", "-of", "csv=p=0", input).Output()
	if err != nil {
		return "", err
	}
	codec := strings.TrimSpace(strings.Split(string(out), "\n")[0])
	codec = strings.TrimSuffix(codec, ",")
	if codec == "" {
		return "unknown", nil
	}
	return codec, nil
}

// ProbeDuration returns the container duration in seconds (0 if unknown).
func ProbeDuration(ctx context.Context, ffprobe, input string) float64 {
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(pctx, ffprobe, "-v", "error", "-show_entries", "format=duration",
		"-of", "default=nokey=1:noprint_wrappers=1", input).Output()
	if err != nil {
		return 0
	}
	d, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return d
}

// Image codecs that sometimes show up as the first "video" stream (cover
// art). Files whose only video stream is one of these are not videos.
var imageCodecs = map[string]bool{"mjpeg": true, "png": true, "bmp": true, "gif": true, "webp": true}

// IsVideoCodec reports whether codec is a real video codec.
func IsVideoCodec(codec string) bool {
	return codec != "" && codec != "unknown" && !imageCodecs[codec]
}
