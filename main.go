// Roger is a self-hosted web interface for yt-dlp: paste a link, pick a format,
// and the file lands in your browser's downloads.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const version = "1.0.0"

func main() {
	if err := run(); err != nil {
		slog.Error("roger stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cacheDir := filepath.Join(cfg.DataDir, "cache")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return err
	}
	yt := &YtDlp{
		Bin:         cfg.YtDlpPath,
		Ffmpeg:      cfg.FfmpegPath,
		CacheDir:    cacheDir,
		MaxFilesize: cfg.MaxFilesize,
		Extra:       cfg.YtDlpArgs,
	}
	ytVersion, err := yt.Version(ctx)
	if err != nil {
		return fmt.Errorf("yt-dlp not usable at %q (set ROGER_YTDLP_PATH): %w", cfg.YtDlpPath, err)
	}
	if cfg.FfmpegPath == "" {
		if _, err := exec.LookPath("ffmpeg"); err != nil {
			slog.Warn("ffmpeg not found: high-resolution merges and mp3 extraction will fail")
		}
	}

	manager, err := NewManager(ctx, cfg, yt)
	if err != nil {
		return err
	}
	managerDone := make(chan struct{})
	go func() {
		defer close(managerDone)
		manager.Run()
	}()

	server := &Server{cfg: cfg, manager: manager, resolve: systemResolver}
	server.ytDlpVersion.Store(ytVersion)
	if cfg.YtDlpUpdateEvery > 0 {
		go server.keepYtDlpFresh(ctx, yt, cfg.YtDlpUpdateEvery)
	}

	httpServer := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// no WriteTimeout: file transfers and event streams are long-lived
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.ListenAndServe() }()

	slog.Info("roger is listening", "port", cfg.Port, "yt-dlp", ytVersion, "password", cfg.Password != "")
	if cfg.Password == "" {
		slog.Warn("no ROGER_PASSWORD set: anyone who can reach this port can use it")
	}

	select {
	case err := <-serveErr:
		stop()
		<-managerDone
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		slog.Warn("shutdown", "error", err)
	}
	httpServer.Close()
	<-managerDone
	return nil
}
