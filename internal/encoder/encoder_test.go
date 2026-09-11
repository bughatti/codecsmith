package encoder

import (
	"strings"
	"testing"
)

const sampleEncoders = `Encoders:
 V..... = Video
 A..... = Audio
 ------
 V....D libx265              libx265 H.265 / HEVC (codec hevc)
 V....D hevc_nvenc           NVIDIA NVENC hevc encoder (codec hevc)
 V..... hevc_qsv             HEVC (Intel Quick Sync Video acceleration) (codec hevc)
 V....D av1_nvenc            NVIDIA NVENC av1 encoder (codec av1)
 V..... libsvtav1            SVT-AV1 (codec av1)
 A....D aac                  AAC (Advanced Audio Coding)
`

func TestParseEncoders(t *testing.T) {
	avail := parseEncoders(sampleEncoders)
	for _, want := range []string{"libx265", "hevc_nvenc", "hevc_qsv", "av1_nvenc", "libsvtav1"} {
		if !avail[want] {
			t.Errorf("missing %s", want)
		}
	}
	if avail["aac"] {
		t.Error("audio encoders must not be listed")
	}
}

func TestBackendsSupport(t *testing.T) {
	avail := parseEncoders(sampleEncoders)
	opt := Options{Available: avail, VAAPIDevice: "/dev/dri/renderD128"}
	cases := map[string]map[string]bool{
		NVENC:        {HEVC: true, AV1: true},
		QSV:          {HEVC: true, AV1: false},
		VAAPI:        {HEVC: false, AV1: false},
		VideoToolbox: {HEVC: false, AV1: false},
		Software:     {HEVC: true, AV1: true},
	}
	for name, codecs := range cases {
		e := construct(name, opt)
		for codec, want := range codecs {
			if got := e.Supports(codec); got != want {
				t.Errorf("%s supports %s = %v want %v", name, codec, got, want)
			}
		}
	}
}

func TestVideoArgs(t *testing.T) {
	avail := parseEncoders(sampleEncoders)
	opt := Options{Available: avail}
	p := Params{Codec: HEVC, Quality: 26, Speed: "slow", MaxBitrate: "4M", Tune: "animation"}

	nv := construct(NVENC, opt).VideoArgs(p)
	joined := strings.Join(nv, " ")
	for _, want := range []string{"-c:v hevc_nvenc", "-preset p7", "-cq 26", "-maxrate 4M", "-bufsize 8000k", "-rc vbr"} {
		if !strings.Contains(joined, want) {
			t.Errorf("nvenc args missing %q: %s", want, joined)
		}
	}

	sw := construct(Software, opt).VideoArgs(p)
	joined = strings.Join(sw, " ")
	for _, want := range []string{"-c:v libx265", "-crf 26", "-preset slow", "-tune animation", "vbv-maxrate=4000"} {
		if !strings.Contains(joined, want) {
			t.Errorf("software args missing %q: %s", want, joined)
		}
	}

	av1 := construct(Software, opt).VideoArgs(Params{Codec: AV1, Quality: 30, Speed: "fast"})
	joined = strings.Join(av1, " ")
	if !strings.Contains(joined, "-c:v libsvtav1") || !strings.Contains(joined, "-preset 10") {
		t.Errorf("svt-av1 args: %s", joined)
	}

	q := construct(QSV, Options{Available: avail, VAAPIDevice: "/dev/dri/renderD128"})
	if got := strings.Join(q.InputArgs(), " "); !strings.Contains(got, "-qsv_device /dev/dri/renderD128") {
		t.Errorf("qsv input args: %s", got)
	}
}

func TestParseRate(t *testing.T) {
	cases := map[string]int64{"4M": 4_000_000, "4000k": 4_000_000, "1.5M": 1_500_000, "800000": 800_000, "": 0, "x": 0}
	for in, want := range cases {
		if got := parseRate(in); got != want {
			t.Errorf("parseRate(%q) = %d want %d", in, got, want)
		}
	}
	if doubleRate("4M") != "8000k" || halfRate("4M") != "2000k" || kbps("4M") != "4000" {
		t.Error("rate helpers")
	}
}
