package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
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
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	db, err := database.Connect(cfg.DatabaseURL)
	if err != nil {
		logger.Error("database connect", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	rc := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
	defer rc.Close()
	q := queue.NewStream(rc, cfg.QueueRedisKey, cfg.StreamGroup, cfg.StreamConsumer, cfg.RedisOperationTimeout)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := q.EnsureConsumerGroup(ctx); err != nil {
		logger.Error("consumer group", "error", err)
		os.Exit(1)
	}
	go outboxLoop(ctx, logger, cfg, db, q)
	go recoveryLoop(ctx, logger, cfg, db)
	consumeLoop(ctx, logger, cfg, db, q)
}

func outboxLoop(ctx context.Context, logger *slog.Logger, cfg *config.Config, db *database.DB, q *queue.Queue) {
	t := time.NewTicker(cfg.OutboxPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for i := 0; i < 100; i++ {
				processed, err := db.ProcessNextOutbox(ctx, cfg.OutboxMaxBackoff, func(e database.OutboxEvent) error {
					_, err := q.Enqueue(ctx, e.SubmissionID, e.ParticipantID)
					return err
				})
				if err != nil {
					logger.Warn("outbox dispatch", "error", err)
					break
				}
				if !processed {
					break
				}
			}
		}
	}
}

func recoveryLoop(ctx context.Context, logger *slog.Logger, cfg *config.Config, db *database.DB) {
	t := time.NewTicker(cfg.WorkerStaleAfter / 2)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ids, err := db.ListStaleProcessing(ctx, time.Now().Add(-cfg.WorkerStaleAfter), 100)
			if err != nil {
				logger.Warn("list stale", "error", err)
				continue
			}
			for _, id := range ids {
				if _, err := db.RecoverStaleSubmission(ctx, id, time.Now().Add(-cfg.WorkerStaleAfter)); err != nil {
					logger.Warn("recover stale", "submission_id", id, "error", err)
				}
			}
		}
	}
}

func consumeLoop(ctx context.Context, logger *slog.Logger, cfg *config.Config, db *database.DB, q *queue.Queue) {
	for ctx.Err() == nil {
		job, err := q.ClaimStale(ctx, cfg.WorkerStaleAfter)
		if errors.Is(err, queue.ErrNoJob) {
			job, err = q.Claim(ctx, time.Second)
		}
		if errors.Is(err, queue.ErrNoJob) {
			continue
		}
		if errors.Is(err, queue.ErrInvalidItem) {
			logger.Warn("invalid stream item", "error", err)
			continue
		}
		if err != nil {
			logger.Warn("stream claim", "error", err)
			time.Sleep(time.Second)
			continue
		}
		processOne(ctx, logger, cfg, db, q, job)
	}
}

func processOne(parent context.Context, logger *slog.Logger, cfg *config.Config, db *database.DB, q *queue.Queue, job *queue.JobItem) {
	result, err := db.TryStartSubmission(parent, job.SubmissionID, cfg.MaxRunningPerParticipant)
	if err != nil {
		logger.Warn("try start", "submission_id", job.SubmissionID, "error", err)
		return
	}
	switch result {
	case database.StartDuplicateOrTerminal, database.StartNotFound:
		if err := q.Ack(parent, job.StreamID); err != nil {
			logger.Warn("ack duplicate", "error", err)
		}
		return
	case database.StartQuotaBusy:
		return
	}
	ctx, cancel := context.WithTimeout(parent, cfg.WorkerHardTimeout)
	defer cancel()
	select {
	case <-ctx.Done():
		_ = db.SetInternalError(context.Background(), job.SubmissionID)
		_ = q.Ack(context.Background(), job.StreamID)
		return
	case <-time.After(cfg.MockWorkDuration):
	}
	finished, err := db.FinishSubmissionConditional(ctx, job.SubmissionID, true, "", "Phase 1 mock runner: source accepted\n", "", 0, false)
	if err != nil {
		logger.Warn("finish", "submission_id", job.SubmissionID, "error", err)
		return
	}
	if finished {
		if err := q.Ack(ctx, job.StreamID); err != nil {
			logger.Warn("ack finished", "error", err)
		}
	}
}
