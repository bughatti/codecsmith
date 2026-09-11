package integrations

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// GPUSample is one reading of GPU load. Percentages are 0-100; a negative
// value means the vendor does not expose that counter.
type GPUSample struct {
	Vendor   string // nvidia | amd | intel
	Name     string
	Util     float64 // overall busy %
	Encoder  float64 // video encoder busy % (NVIDIA only)
	Decoder  float64 // video decoder busy % (NVIDIA only)
	MemUsed  int64   // bytes
	MemTotal int64   // bytes
	Temp     float64 // °C, 0 if unknown
}

// MemPercent is VRAM usage as a percentage (0 if unknown).
func (g GPUSample) MemPercent() float64 {
	if g.MemTotal <= 0 {
		return 0
	}
	return 100 * float64(g.MemUsed) / float64(g.MemTotal)
}

// SampleGPU tries each vendor in turn: nvidia-smi (injected into containers
// by the NVIDIA runtime's "utility" capability), then the amdgpu sysfs
// counters, then a best-effort Intel reading. ok=false when no GPU is
// visible or the vendor exposes no load counters.
func SampleGPU(ctx context.Context) (GPUSample, bool) {
	if s, ok := sampleNvidia(ctx); ok {
		return s, true
	}
	if s, ok := sampleAMDGPU(); ok {
		return s, true
	}
	return GPUSample{}, false
}

func sampleNvidia(ctx context.Context) (GPUSample, bool) {
	bin, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return GPUSample{}, false
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(pctx, bin, "--query-gpu=name,utilization.gpu,utilization.encoder,utilization.decoder,memory.used,memory.total,temperature.gpu",
		"--format=csv,noheader,nounits", "-i", "0").Output()
	if err != nil {
		return GPUSample{}, false
	}
	return parseNvidiaSMI(string(out))
}

func parseNvidiaSMI(line string) (GPUSample, bool) {
	f := strings.Split(strings.TrimSpace(strings.Split(line, "\n")[0]), ",")
	if len(f) < 7 {
		return GPUSample{}, false
	}
	num := func(s string) float64 {
		v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
		return v
	}
	return GPUSample{
		Vendor:   "nvidia",
		Name:     strings.TrimSpace(f[0]),
		Util:     num(f[1]),
		Encoder:  num(f[2]),
		Decoder:  num(f[3]),
		MemUsed:  int64(num(f[4])) << 20,
		MemTotal: int64(num(f[5])) << 20,
		Temp:     num(f[6]),
	}, true
}

// sampleAMDGPU reads the amdgpu driver's sysfs counters. Works inside a
// container as long as /sys is mounted (Docker does this by default).
func sampleAMDGPU() (GPUSample, bool) {
	cards, _ := filepath.Glob("/sys/class/drm/card[0-9]*/device/gpu_busy_percent")
	for _, busyPath := range cards {
		dev := filepath.Dir(busyPath)
		vendor := readTrim(filepath.Join(dev, "vendor"))
		if vendor != "0x1002" { // AMD
			continue
		}
		busy, err := strconv.ParseFloat(readTrim(busyPath), 64)
		if err != nil {
			continue
		}
		used, _ := strconv.ParseInt(readTrim(filepath.Join(dev, "mem_info_vram_used")), 10, 64)
		total, _ := strconv.ParseInt(readTrim(filepath.Join(dev, "mem_info_vram_total")), 10, 64)
		s := GPUSample{Vendor: "amd", Name: amdName(dev), Util: busy, Encoder: -1, Decoder: -1, MemUsed: used, MemTotal: total}
		if hw, _ := filepath.Glob(filepath.Join(dev, "hwmon", "hwmon*", "temp1_input")); len(hw) > 0 {
			if mc, err := strconv.ParseFloat(readTrim(hw[0]), 64); err == nil {
				s.Temp = mc / 1000
			}
		}
		return s, true
	}
	return GPUSample{}, false
}

func amdName(dev string) string {
	if n := readTrim(filepath.Join(dev, "product_name")); n != "" {
		return n
	}
	if id := readTrim(filepath.Join(dev, "device")); id != "" {
		return "AMD GPU " + id
	}
	return "AMD GPU"
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
