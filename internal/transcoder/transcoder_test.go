package transcoder

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bughatti/codecsmith/internal/config"
)

func TestProgressParser(t *testing.T) {
	p := newProgressParser(100) // 100s file
	feed := func(lines ...string) (Progress, bool) {
		var out Progress
		var ok bool
		for _, l := range lines {
			out, ok = p.Feed(l)
		}
		return out, ok
	}
	if _, ok := feed("frame=10", "fps=25.0"); ok {
		t.Fatal("block should not complete before progress= line")
	}
	pr, ok := feed("out_time_us=50000000", "speed=2.5x", "progress=continue")
	if !ok {
		t.Fatal("expected a completed block")
	}
	if pr.Percent != 50 {
		t.Errorf("percent = %v, want 50", pr.Percent)
	}
	if pr.Speed != 2.5 || pr.FPS != 25 {
		t.Errorf("speed/fps = %v/%v", pr.Speed, pr.FPS)
	}
	if pr.ETASeconds != 20 {
		t.Errorf("eta = %d, want 20", pr.ETASeconds)
	}
	pr, _ = feed("out_time_us=100000000", "speed=2.5x", "progress=end")
	if !pr.Done || pr.Percent != 100 || pr.ETASeconds != 0 {
		t.Errorf("end block = %+v", pr)
	}
	// out_time fallback when out_time_us is missing
	p2 := newProgressParser(200)
	pr, _ = p2.Feed("out_time=00:01:40.000000")
	pr, _ = p2.Feed("progress=continue")
	if pr.Percent != 50 {
		t.Errorf("clock fallback percent = %v", pr.Percent)
	}
}

func TestSplitCRLF(t *testing.T) {
	sc := bufio.NewScanner(strings.NewReader("a\rb\r\nc\nd"))
	sc.Split(splitCRLF)
	var got []string
	for sc.Scan() {
		got = append(got, sc.Text())
	}
	want := []string{"a", "b", "", "c", "d"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestSelectAudio(t *testing.T) {
	eng := Stream{Index: 1, Kind: "audio", Language: "eng"}
	jpn := Stream{Index: 2, Kind: "audio", Language: "jpn"}
	und := Stream{Index: 3, Kind: "audio", Language: "und", Title: "English 5.1"}
	cases := []struct {
		name   string
		tracks []Stream
		want   []string
		maps   []string
		note   bool
	}{
		{"no filter keeps all", []Stream{jpn, eng}, nil, []string{"0:2", "0:1"}, false},
		{"tagged english", []Stream{jpn, eng}, []string{"eng"}, []string{"0:1"}, false},
		{"alias en", []Stream{jpn, eng}, []string{"en"}, []string{"0:1"}, false},
		{"title fallback", []Stream{jpn, und}, []string{"eng"}, []string{"0:3"}, true},
		{"single track kept", []Stream{jpn}, []string{"eng"}, []string{"0:2"}, false},
		{"unknown keeps all", []Stream{jpn, {Index: 4, Language: "ger"}}, []string{"eng"}, []string{"0:2", "0:4"}, true},
		{"multiple wanted", []Stream{jpn, eng}, []string{"eng", "jpn"}, []string{"0:2", "0:1"}, false},
	}
	for _, c := range cases {
		maps, note := SelectAudio(c.tracks, c.want)
		if strings.Join(maps, ",") != strings.Join(c.maps, ",") {
			t.Errorf("%s: maps %v want %v", c.name, maps, c.maps)
		}
		if (note != "") != c.note {
			t.Errorf("%s: note %q", c.name, note)
		}
	}
}

func TestSelectSubtitles(t *testing.T) {
	tracks := []Stream{
		{Index: 5, Kind: "subtitle", Codec: "subrip", Language: "eng"},
		{Index: 6, Kind: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "eng"},
		{Index: 7, Kind: "subtitle", Codec: "mov_text", Language: "und"},
		{Index: 8, Kind: "subtitle", Codec: "subrip", Language: "fre"},
	}
	maps, convert := SelectSubtitles(tracks, []string{"eng", "und"}, "mkv")
	if strings.Join(maps, ",") != "0:5,0:6,0:7" || !convert {
		t.Errorf("mkv: %v convert=%v", maps, convert)
	}
	maps, convert = SelectSubtitles(tracks, []string{"eng", "und"}, "mp4")
	if strings.Join(maps, ",") != "0:5,0:7" || !convert {
		t.Errorf("mp4: %v convert=%v", maps, convert)
	}
	maps, _ = SelectSubtitles(tracks, nil, "mkv")
	if len(maps) != 4 {
		t.Errorf("no filter should keep all, got %v", maps)
	}
}

func TestSkipReason(t *testing.T) {
	prof := config.Profile{SizeLimitGB: 1, SkipBelowKbps: 1500}
	gb := int64(1 << 30)
	if r := skipReason(&MediaInfo{VideoCodec: "hevc", BitrateBps: 5_000_000}, gb/2, "hevc", prof); r == "" {
		t.Error("same codec under limit should skip")
	}
	if r := skipReason(&MediaInfo{VideoCodec: "hevc", BitrateBps: 5_000_000}, 2*gb, "hevc", prof); r != "" {
		t.Errorf("same codec over limit should encode, got %q", r)
	}
	if r := skipReason(&MediaInfo{VideoCodec: "h264", BitrateBps: 1_000_000}, gb, "hevc", prof); r == "" {
		t.Error("low bitrate should skip")
	}
	if r := skipReason(&MediaInfo{VideoCodec: "h264", BitrateBps: 8_000_000}, gb, "hevc", prof); r != "" {
		t.Errorf("normal h264 should encode, got %q", r)
	}
	if r := skipReason(&MediaInfo{VideoCodec: "hevc"}, 5*gb, "hevc", config.Profile{SizeLimitGB: 0}); r == "" {
		t.Error("size_limit 0 must never re-encode same codec")
	}
	if r := skipReason(&MediaInfo{VideoCodec: "av1", BitrateBps: 9_000_000}, gb, "hevc", config.Profile{SkipCodecs: []string{"av1"}}); r == "" {
		t.Error("av1 source must be skipped for an hevc target")
	}
	dv := &MediaInfo{VideoCodec: "hevc", DolbyVision: true, BitrateBps: 60_000_000}
	if r := skipReason(dv, 50*gb, "hevc", config.Profile{SizeLimitGB: 10}); r == "" {
		t.Error("Dolby Vision source must be skipped by default")
	}
	if r := skipReason(dv, 50*gb, "hevc", config.Profile{SizeLimitGB: 10, AllowDolbyVision: true}); r != "" {
		t.Errorf("allow_dolby_vision should let it through, got %q", r)
	}
}

func TestParseProbeDolbyVision(t *testing.T) {
	withDV := `{"streams":[{"index":0,"codec_type":"video","codec_name":"hevc",
	 "side_data_list":[{"side_data_type":"DOVI configuration record"}]}],"format":{"duration":"10"}}`
	mi, err := parseProbe([]byte(withDV))
	if err != nil || !mi.DolbyVision {
		t.Errorf("DV not detected: %+v %v", mi, err)
	}
	plain := `{"streams":[{"index":0,"codec_type":"video","codec_name":"hevc",
	 "side_data_list":[{"side_data_type":"Content light level metadata"}]}],"format":{"duration":"10"}}`
	mi, _ = parseProbe([]byte(plain))
	if mi.DolbyVision {
		t.Error("HDR10 side data must not be read as Dolby Vision")
	}
	mi, _ = parseProbe([]byte(`{"streams":[{"index":0,"codec_type":"video","codec_name":"h264"}],"format":{}}`))
	if mi.DolbyVision {
		t.Error("no side data means no DV")
	}
}

func TestTrashPath(t *testing.T) {
	cases := []struct{ trash, lib, file, want string }{
		{"/config/trash", "/media/movies", "/media/movies/Heat (1995)/Heat.mkv", "/config/trash/Heat (1995)/Heat.mkv"},
		{"/config/trash", "/media/shows", "/media/shows/Show/S01/e01.mkv", "/config/trash/Show/S01/e01.mkv"},
		{"/t", "/media/movies", "/elsewhere/odd.mkv", "/t/odd.mkv"},
	}
	for _, c := range cases {
		if got := TrashPath(c.trash, c.lib, c.file); got != c.want {
			t.Errorf("TrashPath(%q,%q) = %q want %q", c.lib, c.file, got, c.want)
		}
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "a.mkv")
	if got := uniquePath(p); got != p {
		t.Errorf("free path should be returned as-is: %s", got)
	}
	_ = os.WriteFile(p, []byte("x"), 0o644)
	if got := uniquePath(p); got != filepath.Join(dir, "a.1.mkv") {
		t.Errorf("taken path should get a counter, got %s", got)
	}
}

func TestHardLinks(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.mkv")
	if err := os.WriteFile(a, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(a)
	if n := HardLinks(info); n != 1 {
		t.Errorf("fresh file has %d links, want 1", n)
	}
	if err := os.Link(a, filepath.Join(dir, "seeding.mkv")); err != nil {
		t.Skipf("hard links unsupported here: %v", err)
	}
	info, _ = os.Stat(a)
	if n := HardLinks(info); n != 2 {
		t.Errorf("linked file has %d links, want 2", n)
	}
}

func TestParseProbe(t *testing.T) {
	raw := `{"streams":[
	 {"index":0,"codec_type":"video","codec_name":"h264","width":1920,"height":1080},
	 {"index":1,"codec_type":"audio","codec_name":"aac","tags":{"language":"eng","title":"Stereo"},"disposition":{"default":1}},
	 {"index":2,"codec_type":"subtitle","codec_name":"subrip","tags":{}}],
	 "format":{"duration":"1234.5","bit_rate":"4500000"}}`
	mi, err := parseProbe([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if mi.VideoCodec != "h264" || mi.Width != 1920 || mi.Duration != 1234.5 || mi.BitrateBps != 4500000 {
		t.Errorf("bad media info: %+v", mi)
	}
	if len(mi.Audio) != 1 || mi.Audio[0].Language != "eng" || !mi.Audio[0].Default {
		t.Errorf("audio: %+v", mi.Audio)
	}
	if len(mi.Subtitles) != 1 || mi.Subtitles[0].Language != "und" {
		t.Errorf("subs: %+v", mi.Subtitles)
	}
}

func TestMoveFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.bin")
	dst := filepath.Join(dir, "sub", "b.bin")
	_ = os.MkdirAll(filepath.Dir(dst), 0o755)
	if err := os.WriteFile(src, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MoveFile(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source should be gone")
	}
	if b, _ := os.ReadFile(dst); string(b) != "hello" {
		t.Error("dest content mismatch")
	}
	// Cross-device: /dev/shm is a separate tmpfs on most Linux hosts.
	if st, err := os.Stat("/dev/shm"); err == nil && st.IsDir() {
		src2 := filepath.Join("/dev/shm", "transcoder-test-"+filepath.Base(dir))
		if err := os.WriteFile(src2, []byte("xdev"), 0o644); err == nil {
			dst2 := filepath.Join(dir, "c.bin")
			if err := MoveFile(src2, dst2); err != nil {
				t.Fatalf("cross-device move: %v", err)
			}
			if b, _ := os.ReadFile(dst2); string(b) != "xdev" {
				t.Error("cross-device content mismatch")
			}
		}
	}
}

func TestLastUsefulErrorLine(t *testing.T) {
	lines := []string{"Stream mapping:", "  Stream #0:0 -> #0:0", "Error while opening encoder for output stream #0:0", "Conversion failed!"}
	if got := lastUsefulErrorLine(lines); got != "Conversion failed!" {
		t.Errorf("got %q", got)
	}
	if got := lastUsefulErrorLine([]string{"[hevc_nvenc] Cannot load libcuda.so.1", "frame=  100"}); !strings.Contains(got, "libcuda") {
		t.Errorf("got %q", got)
	}
}
