package submissions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"hustack/internal/config"
	"hustack/internal/database"
	"hustack/internal/identity"
	"hustack/internal/queue"
	"hustack/internal/ratelimit"
	"hustack/internal/storage"
)

var (
	ErrBothSources      = errors.New("provide either source_text or source_file, not both")
	ErrNoSource         = errors.New("provide either source_text or source_file")
	ErrEmptySource      = errors.New("source is empty")
	ErrInvalidExtension = errors.New("file must have .c extension")
	ErrContainsNUL      = errors.New("source contains NUL byte")
	ErrSizeExceeded     = errors.New("source exceeds maximum size")
	ErrRateLimited      = errors.New("rate limit exceeded")
	ErrCSRFInvalid      = errors.New("invalid CSRF token")
	ErrOwnership        = errors.New("submission not found")
	ErrMultipleFiles    = errors.New("multiple source files not accepted")
	ErrMultipleText     = errors.New("multiple source_text parts not accepted")
	ErrUnknownPart      = errors.New("unknown form part")
	ErrOriginNotAllowed = errors.New("origin not allowed")
	ErrBodyTooLarge     = errors.New("request body too large")
)

type Service struct {
	cfg         *config.Config
	db          *database.DB
	store       *storage.LocalStore
	queue       *queue.Queue
	rateLimiter *ratelimit.RateLimiter
	idProvider  *identity.CookieProvider
	logger      *slog.Logger
}

func NewService(
	cfg *config.Config,
	db *database.DB,
	store *storage.LocalStore,
	q *queue.Queue,
	rl *ratelimit.RateLimiter,
	idp *identity.CookieProvider,
	logger *slog.Logger,
) *Service {
	return &Service{
		cfg:         cfg,
		db:          db,
		store:       store,
		queue:       q,
		rateLimiter: rl,
		idProvider:  idp,
		logger:      logger,
	}
}

type CreateResult struct {
	SubmissionID string `json:"submission_id"`
	Status       string `json:"status"`
}

type SubmissionResult struct {
	ID               string  `json:"id"`
	Status           string  `json:"status"`
	OriginalFilename *string `json:"original_filename,omitempty"`
	SourceSize       int64   `json:"source_size"`
	SourceSHA256     string  `json:"source_sha256"`
	CompileSuccess   *bool   `json:"compile_success"`
	CompileStderr    *string `json:"compile_stderr,omitempty"`
	ExitCode         *int    `json:"exit_code"`
	Stdout           *string `json:"stdout,omitempty"`
	Stderr           *string `json:"stderr,omitempty"`
	ResultTruncated  bool    `json:"result_truncated"`
	CreatedAt        string  `json:"created_at"`
	UpdatedAt        string  `json:"updated_at"`
	StartedAt        *string `json:"started_at,omitempty"`
	FinishedAt       *string `json:"finished_at,omitempty"`
}

func (s *Service) CreateSubmission(r *http.Request, participantID, clientIP string) (*CreateResult, error) {
	if !s.idProvider.VerifyCSRFToken(r) {
		return nil, ErrCSRFInvalid
	}

	if !s.idProvider.VerifyOrigin(r) {
		return nil, ErrOriginNotAllowed
	}

	allowed, retryAfter, err := s.rateLimiter.AllowParticipantSubmit(participantID)
	if err != nil {
		return nil, fmt.Errorf("rate limit check: %w", err)
	}
	if !allowed {
		return nil, &RateLimitError{RetryAfter: retryAfter}
	}

	allowed, retryAfter, err = s.rateLimiter.AllowIPSubmit(clientIP)
	if err != nil {
		return nil, fmt.Errorf("ip rate limit check: %w", err)
	}
	if !allowed {
		return nil, &RateLimitError{RetryAfter: retryAfter}
	}

	mr, err := r.MultipartReader()
	if err != nil {
		return nil, fmt.Errorf("expecting multipart/form-data: %w", err)
	}

	submissionID := uuid.New().String()
	storageKey := submissionID + ".c"
	var sourceFileName string
	var seenText, seenFile bool
	var tmpPath string
	var sourceSize int64
	var sourceSHA256 string

	cleanupTemp := func() {
		if tmpPath != "" {
			os.Remove(tmpPath)
			tmpPath = ""
		}
	}

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if isBodyTooLarge(err) {
			cleanupTemp()
			return nil, ErrBodyTooLarge
		}
		if err != nil {
			cleanupTemp()
			return nil, fmt.Errorf("read multipart part: %w", err)
		}

		formName := part.FormName()

		if formName != "source_text" && formName != "source_file" {
			part.Close()
			if seenText || seenFile {
				cleanupTemp()
			}
			return nil, ErrUnknownPart
		}

		if formName == "source_text" {
			if seenText {
				part.Close()
				cleanupTemp()
				return nil, ErrMultipleText
			}
			if seenFile {
				part.Close()
				cleanupTemp()
				return nil, ErrBothSources
			}
			seenText = true
		}

		if formName == "source_file" {
			if seenFile {
				part.Close()
				cleanupTemp()
				return nil, ErrMultipleFiles
			}
			if seenText {
				part.Close()
				cleanupTemp()
				return nil, ErrBothSources
			}
			if !storage.ValidateExtension(part.FileName()) {
				part.Close()
				return nil, ErrInvalidExtension
			}
			sourceFileName = sanitizeFilename(part.FileName())
			seenFile = true
		}

		reader := stripBOM(part)
		streamTmpPath, streamSize, streamSHA256, err := s.store.SaveStream(reader, s.cfg.MaxSourceBytes)
		part.Close()
		if err != nil {
			cleanupTemp()
			return nil, err
		}

		if tmpPath != "" {
			os.Remove(streamTmpPath)
			cleanupTemp()
			return nil, ErrBothSources
		}

		tmpPath = streamTmpPath
		sourceSize = streamSize
		sourceSHA256 = streamSHA256
	}

	if !seenText && !seenFile {
		cleanupTemp()
		return nil, ErrNoSource
	}

	if seenText {
		sourceFileName = ""
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if _, err := s.db.UpsertParticipant(ctx, participantID); err != nil {
		cleanupTemp()
		return nil, fmt.Errorf("upsert: %w", err)
	}

	if err := s.store.Commit(storageKey, tmpPath); err != nil {
		cleanupTemp()
		return nil, err
	}
	tmpPath = ""

	s.logger.Info("source saved",
		"submission_id", submissionID,
		"source_size", sourceSize,
		"source_sha256", sourceSHA256,
	)

	row, err := s.db.CreateSubmission(ctx, submissionID, participantID, sourceFileName,
		storageKey, sourceSize, sourceSHA256)
	if err != nil {
		s.store.Delete(storageKey)
		cleanupTemp()
		return nil, fmt.Errorf("create submission: %w", err)
	}

	if err := s.queue.EnqueueCheck(submissionID, participantID,
		s.cfg.QueueMaxDepth,
		s.cfg.MaxQueuedPerParticipant,
	); err != nil {
		s.db.SetInternalError(ctx, submissionID)
		s.store.Delete(storageKey)
		cleanupTemp()
		return nil, err
	}

	s.logger.Info("submission queued",
		"submission_id", submissionID,
		"participant_id", participantID,
		"status", row.Status,
	)

	return &CreateResult{
		SubmissionID: row.ID,
		Status:       row.Status,
	}, nil
}

func (s *Service) GetSubmission(ctx context.Context, submissionID, participantID string) (*SubmissionResult, error) {
	row, err := s.db.GetSubmission(ctx, submissionID, participantID)
	if err != nil {
		return nil, fmt.Errorf("get submission: %w", err)
	}
	if row == nil {
		return nil, ErrOwnership
	}

	r := &SubmissionResult{
		ID:               row.ID,
		Status:           row.Status,
		OriginalFilename: row.OriginalFilename,
		SourceSize:       row.SourceSize,
		SourceSHA256:     row.SourceSHA256,
		CompileSuccess:   row.CompileSuccess,
		CompileStderr:    row.CompileStderr,
		ExitCode:         row.ExitCode,
		Stdout:           row.Stdout,
		Stderr:           row.Stderr,
		ResultTruncated:  row.ResultTruncated,
		CreatedAt:        row.CreatedAt.Format(time.RFC3339),
		UpdatedAt:        row.UpdatedAt.Format(time.RFC3339),
	}
	if row.StartedAt != nil {
		v := row.StartedAt.Format(time.RFC3339)
		r.StartedAt = &v
	}
	if row.FinishedAt != nil {
		v := row.FinishedAt.Format(time.RFC3339)
		r.FinishedAt = &v
	}
	return r, nil
}

func stripBOM(reader io.Reader) io.Reader {
	peek := make([]byte, 3)
	n, err := io.ReadAtLeast(reader, peek, 3)
	if err != nil {
		return io.MultiReader(bytes.NewReader(peek[:n]), reader)
	}
	if peek[0] == 0xEF && peek[1] == 0xBB && peek[2] == 0xBF {
		return reader
	}
	return io.MultiReader(bytes.NewReader(peek), reader)
}

type RateLimitError struct {
	RetryAfter int
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limit exceeded, retry after %d seconds", e.RetryAfter)
}

func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "\x00", "")
	if len(name) > 255 {
		ext := ".c"
		if idx := strings.LastIndex(name, "."); idx > 0 && len(name)-idx <= 10 {
			ext = name[idx:]
		}
		name = name[:255-len(ext)] + ext
	}
	return name
}

func isBodyTooLarge(err error) bool {
	var maxBytesErr *http.MaxBytesError
	return errors.As(err, &maxBytesErr)
}
