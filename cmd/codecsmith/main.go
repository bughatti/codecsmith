// Command codecsmith is a single binary with four modes:
//
//	codecsmith --mode=all      dashboard + worker in one process (default)
//	codecsmith --mode=web      dashboard/API only
//	codecsmith --mode=worker   scanning + encoding only
//	codecsmith --mode=migrate  apply database migrations and exit
//	codecsmith --version
//	codecsmith --healthcheck   (container HEALTHCHECK; no curl needed)
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/bughatti/codecsmith/internal/config"
	"github.com/bughatti/codecsmith/internal/db"
	"github.com/bughatti/codecsmith/internal/logger"
	"github.com/bughatti/codecsmith/internal/priv"
	"github.com/bughatti/codecsmith/internal/version"
	"github.com/bughatti/codecsmith/internal/web"
	"github.com/bughatti/codecsmith/internal/worker"
)

func main() {
	mode := flag.String("mode", "all", "run mode: all | web | worker | migrate")
	cfgPath := flag.String("config", "", "path to config.yaml (default: ./config.yaml, $CODECSMITH_CONFIG)")
	showVersion := flag.Bool("version", false, "print version and exit")
	healthcheck := flag.Bool("healthcheck", false, "exit 0 if the local web server or worker is healthy (for container HEALTHCHECK)")
	flag.Parse()

	if *healthcheck {
		os.Exit(runHealthcheck(*cfgPath))
	}

	if *showVersion {
		fmt.Println("codecsmith", version.String())
		return
	}

	cfg, err := config.Load(*cfgPath, *mode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	// Running as root inside a container: fix ownership of writable dirs and
	// drop to PUID/PGID before anything else opens files.
	if settings, ok := priv.FromEnv(); ok {
		dirs := []string{cfg.DataDir, cfg.LogDir, cfg.Worker.TempDir}
		if p := cfg.SQLitePath(); p != "" {
			dirs = append(dirs, filepath.Dir(p))
		}
		if err := priv.Drop(settings, dirs...); err != nil {
			fmt.Fprintf(os.Stderr, "drop privileges: %v\n", err)
			os.Exit(1)
		}
	}

	log, closeLog := logger.Setup(cfg.Mode, cfg.LogLevel, cfg.LogDir)
	defer closeLog.Close()
	log.Info("codecsmith starting", "version", version.String(), "mode", cfg.Mode, "config", cfg.Path,
		"db", cfg.Database.Driver, "uid", os.Getuid())

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	dsn := cfg.Database.DSN
	if cfg.Database.Driver == "sqlite" {
		dsn = cfg.SQLitePath()
		_ = os.MkdirAll(filepath.Dir(dsn), 0o755)
	}
	store, err := db.Open(ctx, db.Driver(cfg.Database.Driver), dsn)
	if err != nil {
		log.Error("database", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	applied, err := store.Migrate(ctx)
	if err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}
	if len(applied) > 0 {
		log.Info("migrations applied", "versions", applied)
	}
	log.Info("database ready", "driver", cfg.Database.Driver, "schema", store.SchemaVersion(ctx))

	exit := 0
	switch cfg.Mode {
	case "migrate":
		// done above
	case "web":
		srv := web.New(cfg, store, log.With("component", "web"), nil)
		if err := srv.Run(ctx); err != nil {
			log.Error("web", "err", err)
			exit = 1
		}
	case "worker":
		w, err := worker.New(ctx, cfg, store, log.With("component", "worker"))
		if err != nil {
			log.Error("worker init", "err", err)
			os.Exit(1)
		}
		if err := w.Run(ctx); err != nil {
			log.Error("worker", "err", err)
			exit = 1
		}
	case "all":
		w, err := worker.New(ctx, cfg, store, log.With("component", "worker"))
		if err != nil {
			log.Error("worker init", "err", err)
			os.Exit(1)
		}
		srv := web.New(cfg, store, log.With("component", "web"), w)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := srv.Run(ctx); err != nil {
				log.Error("web", "err", err)
				cancel()
			}
		}()
		go func() {
			defer wg.Done()
			if err := w.Run(ctx); err != nil {
				log.Error("worker", "err", err)
				cancel()
			}
		}()
		wg.Wait()
	default:
		fmt.Fprintf(os.Stderr, "unknown --mode=%s (all | web | worker | migrate)\n", cfg.Mode)
		os.Exit(2)
	}
	log.Info("codecsmith stopped")
	os.Exit(exit)
}

// runHealthcheck reports healthy when the web server answers /healthz or,
// for a worker-only process, when the heartbeat file is fresh.
func runHealthcheck(cfgPath string) int {
	cfg, err := config.Load(cfgPath, "web")
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	_, port, _ := net.SplitHostPort(cfg.Web.Listen)
	if port == "" {
		port = "8090"
	}
	client := &http.Client{Timeout: 4 * time.Second}
	if resp, err := client.Get("http://127.0.0.1:" + port + "/healthz"); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return 0
		}
	}
	if st, err := os.Stat(worker.HeartbeatFile(cfg.DataDir)); err == nil && time.Since(st.ModTime()) < 90*time.Second {
		return 0
	}
	fmt.Fprintln(os.Stderr, "unhealthy: no web server on :"+port+" and no fresh worker heartbeat")
	return 1
}
