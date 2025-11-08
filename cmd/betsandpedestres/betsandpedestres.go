package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"betsandpedestres/internal/auth"
	"betsandpedestres/internal/config"
	"betsandpedestres/internal/db"
	"betsandpedestres/internal/dbinit"
	apphttp "betsandpedestres/internal/http"
	"betsandpedestres/internal/logging"
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

	mux, err := apphttp.NewMux(pool)
	if err != nil {
		slog.Error("Coulnd't parse templates", "err", err)
		os.Exit(1)
	}
	srv := &http.Server{
		Addr:         cfg.HTTP.Address, // e.g. ":8080"
		Handler:      apphttp.WithStandardMiddleware(mux),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start
	go func() {
		slog.Info("http.starting", "addr", cfg.HTTP.Address)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http.listen", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	slog.Info("http.shutting_down")
	_ = srv.Shutdown(ctx)
	slog.Info("http.stopped")
}
