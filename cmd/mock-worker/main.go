package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"hustack/internal/config"
	"hustack/internal/database"
	"hustack/internal/queue"
)

func main() {
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

	redisClient := redis.NewClient(&redis.Options{
		Addr: cfg.RedisAddr,
		DB:   0,
	})
	if err := redisClient.Ping(context.Background()).Err(); err != nil {
		logger.Error("redis ping", "error", err)
		os.Exit(1)
	}
	defer redisClient.Close()

	q := queue.New(redisClient, cfg.QueueRedisKey)

	logger.Info("mock worker starting",
		"work_duration", cfg.MockWorkDuration,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		logger.Info("shutting down worker")
		cancel()
	}()

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				recovered, err := q.RecoverStale(2 * time.Minute)
				if err != nil {
					logger.Warn("stale recovery scan", "error", err)
					continue
				}
				for _, item := range recovered {
					ok, err := db.RecoverToQueued(ctx, item.SubmissionID)
					if err != nil {
						logger.Error("stale recovery db", "submission_id", item.SubmissionID, "error", err)
						continue
					}
					if !ok {
						logger.Warn("stale recovery: DB row not in mock_processing",
							"submission_id", item.SubmissionID,
						)
						q.Complete(item.SubmissionID, item.ParticipantID)
						continue
					}
					if err := q.RecoverOne(item.SubmissionID, item.ParticipantID); err != nil {
						logger.Error("stale recovery redis", "submission_id", item.SubmissionID, "error", err)
					} else {
						logger.Info("stale job recovered",
							"submission_id", item.SubmissionID,
							"participant_id", item.ParticipantID[:8],
						)
					}
				}
			}
		}
	}()

	var running atomic.Int32
	maxRunning := cfg.MaxRunningPerParticipant
	processJobs(ctx, logger, cfg, db, q, &running, maxRunning)
}

func processJobs(ctx context.Context, logger *slog.Logger, cfg *config.Config, db *database.DB, q *queue.Queue, running *atomic.Int32, maxRunning int) {
	for {
		select {
		case <-ctx.Done():
			logger.Info("worker stopped")
			return
		default:
		}

		job, err := q.Claim(3*time.Second, maxRunning)
		if err != nil {
			if err == queue.ErrNoJob {
				continue
			}
			if err == queue.ErrRunningQuota {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			logger.Warn("claim error", "error", err)
			time.Sleep(time.Second)
			continue
		}

		running.Add(1)
		pid := job.ParticipantID
		logger.Info("claimed submission",
			"submission_id", job.SubmissionID,
			"participant_id", pid[:8],
		)

		started, err := db.StartSubmissionConditional(ctx, job.SubmissionID)
		if err != nil {
			logger.Error("start submission error",
				"submission_id", job.SubmissionID,
				"error", err,
			)
			q.Fail(job.SubmissionID, pid)
			_ = db.SetInternalError(ctx, job.SubmissionID)
			running.Add(-1)
			continue
		}
		if !started {
			logger.Warn("duplicate delivery or already started",
				"submission_id", job.SubmissionID,
			)
			q.Complete(job.SubmissionID, pid)
			running.Add(-1)
			continue
		}

		select {
		case <-ctx.Done():
			logger.Info("shutdown during job, leaving for recovery",
				"submission_id", job.SubmissionID,
			)
			running.Add(-1)
			return
		case <-time.After(cfg.MockWorkDuration):
		}

		finished, err := db.FinishSubmissionConditional(ctx, job.SubmissionID,
			true, "", "Phase 1 mock runner: source accepted\n", "", 0, false,
		)
		if err != nil {
			logger.Error("finish submission error",
				"submission_id", job.SubmissionID,
				"error", err,
			)
			q.Fail(job.SubmissionID, pid)
			_ = db.SetInternalError(ctx, job.SubmissionID)
			running.Add(-1)
			continue
		}
		if !finished {
			logger.Warn("finish failed: submission not in mock_processing",
				"submission_id", job.SubmissionID,
			)
			q.Fail(job.SubmissionID, pid)
			running.Add(-1)
			continue
		}

		if err := q.Complete(job.SubmissionID, pid); err != nil {
			logger.Warn("complete error",
				"submission_id", job.SubmissionID,
				"error", err,
			)
		}

		running.Add(-1)
		logger.Info("completed submission",
			"submission_id", job.SubmissionID,
			"participant_id", pid[:8],
		)
	}
}
