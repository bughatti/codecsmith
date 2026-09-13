// Package integrations holds the optional external hooks: the SABnzbd
// download poller and host metrics (CPU, memory, disk) used by the
// dashboard.
package integrations

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bughatti/codecsmith/internal/db"
)

// -----------------------------------------------------------------------------
// SABnzbd
// -----------------------------------------------------------------------------

// SABnzbd mirrors the download queue into the downloads table.
type SABnzbd struct {
	base   string
	apiKey string
	hc     *http.Client
	log    *slog.Logger
}

// NewSABnzbd builds a poller.
func NewSABnzbd(baseURL, apiKey string, log *slog.Logger) *SABnzbd {
	return &SABnzbd{
		base:   strings.TrimRight(baseURL, "/") + "/api",
		apiKey: apiKey,
		hc:     &http.Client{Timeout: 10 * time.Second},
		log:    log,
	}
}

// Sync polls queue + history and upserts rows.
func (s *SABnzbd) Sync(ctx context.Context, store *db.Store) error {
	queue, err := s.fetch(ctx, url.Values{"mode": {"queue"}})
	if err != nil {
		return err
	}
	for _, slot := range queue.Queue.Slots {
		status := "paused"
		switch slot.Status {
		case "Downloading":
			status = "downloading"
		case "Queued":
			status = "queued"
		}
		total := parseSize(slot.Size)
		left := parseSize(slot.SizeLeft)
		_ = store.UpsertDownload(ctx, db.Download{
			Source: "sabnzbd", ExternalID: slot.NzoID, Title: slot.Filename, Status: status,
			Progress: atof(slot.Percentage), SizeTotal: total, SizeDownloaded: total - left,
			DownloadRate: atof(slot.KBPerSec) * 1024, ETASeconds: parseEtaSeconds(slot.TimeLeft), Category: slot.Category,
		})
	}
	hist, err := s.fetch(ctx, url.Values{"mode": {"history"}, "limit": {"20"}})
	if err == nil {
		for _, slot := range hist.History.Slots {
			status, progress := "completed", 100.0
			if slot.Status == "Failed" {
				status, progress = "failed", 0
			}
			_ = store.UpsertDownload(ctx, db.Download{
				Source: "sabnzbd", ExternalID: slot.NzoID, Title: slot.Name, Status: status,
				Progress: progress, SizeTotal: parseSize(slot.Size), Category: slot.Category,
			})
		}
	}
	return store.PruneDownloads(ctx, time.Now().Add(-7*24*time.Hour))
}

type sabResponse struct {
	Queue struct {
		Slots []struct {
			NzoID      string `json:"nzo_id"`
			Filename   string `json:"filename"`
			Status     string `json:"status"`
			Percentage string `json:"percentage"`
			Size       string `json:"size"`
			SizeLeft   string `json:"sizeleft"`
			KBPerSec   string `json:"kbpersec"`
			TimeLeft   string `json:"timeleft"`
			Category   string `json:"cat"`
		} `json:"slots"`
	} `json:"queue"`
	History struct {
		Slots []struct {
			NzoID    string `json:"nzo_id"`
			Name     string `json:"name"`
			Status   string `json:"status"`
			Size     string `json:"size"`
			Category string `json:"category"`
		} `json:"slots"`
	} `json:"history"`
}

func (s *SABnzbd) fetch(ctx context.Context, params url.Values) (*sabResponse, error) {
	params.Set("apikey", s.apiKey)
	params.Set("output", "json")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"?"+params.Encode(), nil)
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("sabnzbd %s: %s", params.Get("mode"), resp.Status)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var out sabResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func parseSize(s string) int64 {
	parts := strings.Fields(s)
	if len(parts) != 2 {
		return 0
	}
	v, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0
	}
	mult := map[string]int64{"B": 1, "KB": 1 << 10, "MB": 1 << 20, "GB": 1 << 30, "TB": 1 << 40}[strings.ToUpper(parts[1])]
	if mult == 0 {
		mult = 1
	}
	return int64(v * float64(mult))
}

func parseEtaSeconds(s string) *int {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return nil
	}
	h, e1 := strconv.Atoi(parts[0])
	m, e2 := strconv.Atoi(parts[1])
	sec, e3 := strconv.Atoi(parts[2])
	if e1 != nil || e2 != nil || e3 != nil {
		return nil
	}
	v := h*3600 + m*60 + sec
	return &v
}

func atof(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// -----------------------------------------------------------------------------
// host metrics
// -----------------------------------------------------------------------------

// SystemMetrics samples CPU, memory and disk usage percentages. Memory is
// cgroup-aware so a container limit is respected.
func SystemMetrics(diskPath string) (cpu, mem, disk float64) {
	return readCPUPercent(), readMemPercent(), readDiskPercent(diskPath)
}

func readCPUPercent() float64 {
	a, ok := readCPUStat()
	if !ok {
		return 0
	}
	time.Sleep(200 * time.Millisecond)
	b, ok := readCPUStat()
	if !ok {
		return 0
	}
	dTotal, dIdle := b.total-a.total, b.idle-a.idle
	if dTotal == 0 {
		return 0
	}
	return 100 * float64(dTotal-dIdle) / float64(dTotal)
}

type cpuStat struct{ total, idle int64 }

func readCPUStat() (cpuStat, bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuStat{}, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		var st cpuStat
		for i, f := range strings.Fields(line)[1:] {
			v, _ := strconv.ParseInt(f, 10, 64)
			st.total += v
			if i == 3 || i == 4 { // idle + iowait
				st.idle += v
			}
		}
		return st, true
	}
	return cpuStat{}, false
}

func readMemPercent() float64 {
	if cur, max, ok := readCgroupMem(); ok {
		return 100 * float64(cur) / float64(max)
	}
	if total, avail, ok := readMemInfo(); ok {
		return 100 * float64(total-avail) / float64(total)
	}
	return 0
}

func readCgroupMem() (cur, max int64, ok bool) {
	c, err := os.ReadFile("/sys/fs/cgroup/memory.current")
	if err != nil {
		return 0, 0, false
	}
	m, err := os.ReadFile("/sys/fs/cgroup/memory.max")
	if err != nil {
		return 0, 0, false
	}
	mStr := strings.TrimSpace(string(m))
	if mStr == "max" {
		return 0, 0, false
	}
	cur, _ = strconv.ParseInt(strings.TrimSpace(string(c)), 10, 64)
	max, _ = strconv.ParseInt(mStr, 10, 64)
	return cur, max, max > 0
}

func readMemInfo() (total, available int64, ok bool) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			available = v * 1024
		}
	}
	return total, available, total > 0
}

func readDiskPercent(path string) float64 {
	if path == "" {
		path = "/"
	}
	t, u, _, err := StatFS(path)
	if err != nil || t == 0 {
		return 0
	}
	return 100 * float64(u) / float64(t)
}

// StatFS returns total/used/free bytes for the filesystem containing path.
func StatFS(path string) (total, used, free int64, err error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0, 0, 0, err
	}
	total = int64(s.Blocks) * int64(s.Bsize)
	free = int64(s.Bavail) * int64(s.Bsize)
	return total, total - free, free, nil
}

// DirStats returns filesystem totals plus the directory's own byte size and
// file count. The walk is bounded by ctx.
func DirStats(ctx context.Context, path string) (db.StorageStat, error) {
	total, _, free, err := StatFS(path)
	if err != nil {
		return db.StorageStat{}, err
	}
	st := db.StorageStat{Path: path, Total: total, Free: free}
	err = filepath.WalkDir(path, func(_ string, d os.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if walkErr != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			st.Used += info.Size()
			st.FileCount++
		}
		return nil
	})
	return st, err
}
