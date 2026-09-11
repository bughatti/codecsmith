// Package priv drops root privileges at startup. The container image runs
// as root only long enough to fix ownership on its writable directories,
// then switches to $PUID/$PGID (the linuxserver.io convention) so ffmpeg and
// the database never run as uid 0. Done in-process so no shell entrypoint
// is needed.
package priv

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Settings describes the target identity.
type Settings struct {
	UID  int
	GID  int
	Sups []int
}

// FromEnv reads PUID / PGID / PGIDS (comma-separated supplementary groups,
// e.g. render/video for /dev/dri access). Returns ok=false if PUID is unset.
func FromEnv() (Settings, bool) {
	uidStr := os.Getenv("PUID")
	if uidStr == "" {
		return Settings{}, false
	}
	uid, err := strconv.Atoi(uidStr)
	if err != nil {
		return Settings{}, false
	}
	gid := uid
	if g, err := strconv.Atoi(os.Getenv("PGID")); err == nil {
		gid = g
	}
	var sups []int
	for _, s := range strings.Split(os.Getenv("PGIDS"), ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if g, err := strconv.Atoi(s); err == nil {
			sups = append(sups, g)
		}
	}
	return Settings{UID: uid, GID: gid, Sups: sups}, true
}

// Drop chowns dirs to the target identity (only the top level and any
// existing files directly inside, not a recursive walk of media) and then
// switches the process uid/gid. No-op when not running as root.
func Drop(s Settings, dirs ...string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
		if err := os.Chown(d, s.UID, s.GID); err != nil {
			return fmt.Errorf("chown %s: %w", d, err)
		}
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			_ = os.Lchown(d+"/"+e.Name(), s.UID, s.GID)
		}
	}
	groups := append([]int{s.GID}, s.Sups...)
	if err := syscall.Setgroups(groups); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(s.GID); err != nil {
		return fmt.Errorf("setgid: %w", err)
	}
	if err := syscall.Setuid(s.UID); err != nil {
		return fmt.Errorf("setuid: %w", err)
	}
	// HOME for ffmpeg/other tools that look for it.
	if os.Getenv("HOME") == "" || os.Getenv("HOME") == "/root" {
		_ = os.Setenv("HOME", "/tmp")
	}
	return nil
}
