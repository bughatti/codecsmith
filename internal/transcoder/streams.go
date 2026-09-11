package transcoder

import (
	"fmt"
	"strings"
)

// SelectAudio picks which audio tracks to keep.
//
// Rules, in order:
//  1. no language filter configured → keep every track;
//  2. tracks whose language tag matches a wanted language;
//  3. else tracks whose TITLE names a wanted language (untagged rips);
//  4. else a single track → keep it;
//  5. else keep everything — a wanted track is never silently dropped
//     because a release was mislabeled. The note explains what happened.
func SelectAudio(tracks []Stream, wanted []string) (maps []string, note string) {
	if len(tracks) == 0 {
		return nil, ""
	}
	if len(wanted) == 0 {
		return allMaps(tracks), ""
	}
	want := normalizeLangs(wanted)
	var out []string
	for _, t := range tracks {
		if want[normalizeLang(t.Language)] {
			out = append(out, mapSpec(t))
		}
	}
	if len(out) > 0 {
		return out, ""
	}
	for _, t := range tracks {
		if titleMatches(t.Title, want) {
			out = append(out, mapSpec(t))
		}
	}
	if len(out) > 0 {
		return out, "audio matched by title (language tag missing)"
	}
	if len(tracks) == 1 {
		return []string{mapSpec(tracks[0])}, ""
	}
	return allMaps(tracks), fmt.Sprintf("no %s audio identified; kept all %d tracks", strings.Join(wanted, "/"), len(tracks))
}

// SelectSubtitles keeps text subtitle tracks in the wanted languages (or
// all when no filter). Returns the -map specs and whether any track needs
// converting to a matroska-friendly text codec.
func SelectSubtitles(tracks []Stream, wanted []string, container string) (maps []string, convert bool) {
	want := normalizeLangs(wanted)
	for _, t := range tracks {
		if len(want) > 0 && !want[normalizeLang(t.Language)] {
			continue
		}
		if !subtitleAllowedIn(t.Codec, container) {
			continue
		}
		maps = append(maps, mapSpec(t))
		if needsTextConvert(t.Codec, container) {
			convert = true
		}
	}
	return maps, convert
}

// subtitleAllowedIn filters codecs the target container cannot carry.
func subtitleAllowedIn(codec, container string) bool {
	switch container {
	case "mp4":
		// mp4 only takes timed text; bitmap subs (pgs/dvd) can't be muxed.
		switch codec {
		case "subrip", "srt", "mov_text", "ass", "ssa", "text", "webvtt":
			return true
		}
		return false
	default: // mkv carries everything
		return true
	}
}

func needsTextConvert(codec, container string) bool {
	switch container {
	case "mp4":
		return codec != "mov_text"
	default:
		// mov_text/tx3g can't be stored in mkv; convert to srt.
		return codec == "mov_text" || codec == "tx3g" || codec == "text"
	}
}

// SubtitleCodecArg returns the -c:s value for the container when converting.
func SubtitleCodecArg(container string) string {
	if container == "mp4" {
		return "mov_text"
	}
	return "srt"
}

func mapSpec(t Stream) string { return fmt.Sprintf("0:%d", t.Index) }

func allMaps(tracks []Stream) []string {
	out := make([]string, 0, len(tracks))
	for _, t := range tracks {
		out = append(out, mapSpec(t))
	}
	return out
}

// language aliases → ISO 639-2/B
var langAliases = map[string]string{
	"en": "eng", "en-us": "eng", "en-gb": "eng", "eng-us": "eng", "english": "eng",
	"ja": "jpn", "jp": "jpn", "japanese": "jpn",
	"de": "ger", "deu": "ger", "german": "ger",
	"fr": "fre", "fra": "fre", "french": "fre",
	"es": "spa", "spanish": "spa",
	"it": "ita", "italian": "ita",
	"pt": "por", "portuguese": "por",
	"ru": "rus", "russian": "rus",
	"zh": "chi", "zho": "chi", "chinese": "chi",
	"ko": "kor", "korean": "kor",
	"nl": "dut", "nld": "dut", "dutch": "dut",
	"sv": "swe", "swedish": "swe",
	"pl": "pol", "polish": "pol",
	"hi": "hin", "hindi": "hin",
	"ar": "ara", "arabic": "ara",
	"": "und", "unknown": "und", "undetermined": "und",
}

func normalizeLang(l string) string {
	l = strings.ToLower(strings.TrimSpace(l))
	if v, ok := langAliases[l]; ok {
		return v
	}
	return l
}

func normalizeLangs(in []string) map[string]bool {
	out := map[string]bool{}
	for _, l := range in {
		out[normalizeLang(l)] = true
	}
	return out
}

// language names by code for title matching
var langNames = map[string][]string{
	"eng": {"english", "(eng", "eng ", "en "},
	"jpn": {"japanese", "(jpn", "jpn ", "jap"},
	"ger": {"german", "deutsch"},
	"fre": {"french", "français", "francais"},
	"spa": {"spanish", "español", "espanol", "latino", "castellano"},
	"ita": {"italian", "italiano"},
	"por": {"portuguese", "português", "brazilian"},
	"rus": {"russian"},
	"chi": {"chinese", "mandarin", "cantonese"},
	"kor": {"korean"},
}

func titleMatches(title string, want map[string]bool) bool {
	t := strings.ToLower(title)
	if t == "" {
		return false
	}
	for code := range want {
		for _, name := range langNames[code] {
			if strings.Contains(t, name) {
				return true
			}
		}
		if t == code {
			return true
		}
	}
	return false
}
