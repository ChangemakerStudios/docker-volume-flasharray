// Command docker-volume-flasharray is a Docker managed volume plugin for Pure
// Storage FlashArray over iSCSI or NVMe/TCP.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/config"
	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/driver"
	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/flasharray"
	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/mounter"
	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/plugin"
	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/transport"
)

// socketName is the plugin socket; Docker addresses the driver as "flasharray".
const socketName = "flasharray"

var version = "dev" // set by -ldflags

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println("docker-volume-flasharray", version)
		return
	}
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(cfg.LogLevel)}))
	slog.SetDefault(log)
	log.Info("starting docker-volume-flasharray", "version", version, "namespace", cfg.Namespace,
		"transport", cfg.Transport, "array", cfg.Array.Endpoint, "fs", cfg.FSType, "default_size", cfg.DefaultSize)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	array, err := flasharray.New(ctx, flasharray.Options{
		Endpoint:           cfg.Array.Endpoint,
		APIToken:           cfg.Array.APIToken,
		APIVersion:         cfg.Array.APIVersion,
		InsecureSkipVerify: cfg.Array.InsecureSkipVerify,
		Logger:             log,
	})
	if err != nil {
		return fmt.Errorf("flasharray client: %w", err)
	}
	tr, err := transport.New(string(cfg.Transport), transport.Options{
		AllowedCIDRs: cfg.AllowedCIDRs,
		Timeout:      cfg.AttachTimeout,
		Logger:       log,
	})
	if err != nil {
		return err
	}
	drv, err := driver.New(ctx, cfg, driver.Deps{
		Array:     array,
		Transport: tr,
		Mounter:   mounter.New(log),
		Logger:    log,
	})
	if err != nil {
		return err
	}

	h := plugin.NewHandler(drv, log)
	log.Info("serving", "socket", "/run/docker/plugins/"+socketName+".sock")
	srvCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return h.ServeUnix(srvCtx, socketName)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
