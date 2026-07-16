package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"hustack/internal/config"
	"hustack/internal/database"
	"hustack/internal/identity"
	"hustack/internal/queue"
	"hustack/internal/ratelimit"
	"hustack/internal/storage"
	"hustack/internal/web"
)

func main() {
	migrateOnly := flag.Bool("migrate", false, "run migrations and exit")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load failed", "error", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
	slog.SetDefault(logger)

	db, err := database.Connect(cfg.DatabaseURL)
	if err != nil {
		logger.Error("database connect", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	ctx := context.Background()
	if err := db.RunMigrations(ctx); err != nil {
		logger.Error("migrations", "error", err)
		os.Exit(1)
	}

	if *migrateOnly {
		logger.Info("migrations completed")
		return
	}

	redisClient := redis.NewClient(&redis.Options{
		Addr: cfg.RedisAddr,
		DB:   0,
	})
	if err := redisClient.Ping(ctx).Err(); err != nil {
		logger.Error("redis ping", "error", err)
		os.Exit(1)
	}
	defer redisClient.Close()

	idProvider := identity.NewCookieProvider(cfg)
	sourceStore := storage.NewLocalStore(cfg.SourceStorageRoot)
	q := queue.New(redisClient, cfg.QueueRedisKey)
	rl := ratelimit.New(redisClient, cfg)

	srv := web.NewServer(cfg, db, sourceStore, q, rl, idProvider, logger)

	httpServer := &http.Server{
		Addr:         cfg.HTTPAddr,
		Handler:      srv.Handler(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		logger.Info("api server starting", "addr", cfg.HTTPAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("shutting down server")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", "error", err)
	}
	logger.Info("server stopped")
}
