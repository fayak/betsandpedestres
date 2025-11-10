package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"betsandpedestres/internal/auth"
	"betsandpedestres/internal/config"
	"betsandpedestres/internal/db"
	"betsandpedestres/internal/dbinit"
	apphttp "betsandpedestres/internal/http"
	"betsandpedestres/internal/logging"
	"betsandpedestres/internal/telegram"
)

func main() {
	cfg, err := config.Load("config.yaml")
	if err != nil && cfg == nil {
		panic(err)
	}

	l := logging.New(logging.Options{
		Level:  cfg.Logging.Level,
		Format: cfg.Logging.Format,
	})
	slog.SetDefault(l)

	if err != nil {
		slog.Warn("Could not get `config.yaml` file. Will run with default values")
		slog.Warn("The JWT secret will be defined to a default value. This is a security risk in production.")
	}

	pgURL, err := cfg.Database.AppURL()
	if err != nil {
		panic(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := dbinit.EnsureDatabaseAndMigrate(ctx, pgURL, cfg.Database.Name, cfg.Database.User); err != nil {
		log.Fatalf("db init failed: %v", err)
	}

	log.Println("database ensured and migrated")

	auth.SetSecret(cfg.Security.JWTSecret)

	appURL, err := cfg.Database.AppURL()
	if err != nil {
		slog.Error("db.url", "err", err)
		os.Exit(1)
	}
	ctxpool, cancelpool := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelpool()
	pool, err := db.NewPool(ctxpool, appURL)
	if err != nil {
		slog.Error("db.pool", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	apphttp.SetVersion(readVersionFile("VERSION"))

	mux, err := apphttp.NewMux(pool, cfg)
	if err != nil {
		slog.Error("Coulnd't parse templates", "err", err)
		os.Exit(1)
	}
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()

	if cfg.Telegram.BotToken != "" {
		if poller := telegram.NewPoller(pool, cfg.Telegram.BotToken); poller != nil {
			go poller.Run(rootCtx)
		}
	}
	srv := &http.Server{
		Addr:         cfg.HTTP.Address,
		Handler:      apphttp.WithStandardMiddleware(mux),
		BaseContext:  func(l net.Listener) context.Context { return rootCtx },
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("http.listening", "addr", srv.Addr)
		serverErr <- srv.ListenAndServe()
	}()
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case <-sigCtx.Done():
		slog.Info("http.shutting_down")
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http.failed", "err", err)
			// close pool before exiting on fatal serve error
			pool.Close()
			os.Exit(1)
		}
	}

	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shCtx); err != nil {
		slog.Warn("http.shutdown_error", "err", err)
	}
	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("http.serve_returned", "err", err)
		}
	case <-time.After(3 * time.Second):
		slog.Warn("http.serve_wait_timeout")
	}

	slog.Info("http.stopped")
	st := pool.Stat()
	slog.Info("pgxpool.stats",
		"total", st.TotalConns(),
		"acquired", st.AcquiredConns(),
		"idle", st.IdleConns(),
		"constructing", st.ConstructingConns(),
		"acquire_count", st.AcquireCount(),
		"canceled_acquire_count", st.CanceledAcquireCount(),
	)

	pool.Close()
	slog.Info("pool.closed")
}

func readVersionFile(path string) string {
	tryPaths := []string{path}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		tryPaths = append(tryPaths, filepath.Join(dir, path))
	}
	for _, p := range tryPaths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		ver := strings.TrimSpace(string(data))
		if ver != "" {
			return ver
		}
	}
	return "DEVBUILD"
}
