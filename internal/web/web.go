package web

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"

	"hustack/internal/config"
	"hustack/internal/database"
	"hustack/internal/identity"
	"hustack/internal/queue"
	"hustack/internal/ratelimit"
	"hustack/internal/storage"
	"hustack/internal/submissions"
)

type Server struct {
	cfg         *config.Config
	db          *database.DB
	store       *storage.LocalStore
	queue       *queue.Queue
	rateLimiter *ratelimit.RateLimiter
	idProvider  *identity.CookieProvider
	subSvc      *submissions.Service
	templates   *template.Template
	logger      *slog.Logger
}

func NewServer(
	cfg *config.Config,
	db *database.DB,
	store *storage.LocalStore,
	q *queue.Queue,
	rl *ratelimit.RateLimiter,
	idp *identity.CookieProvider,
	logger *slog.Logger,
) *Server {
	subSvc := submissions.NewService(cfg, db, store, q, rl, idp, logger)
	tmpl := template.Must(template.ParseGlob("web/templates/*.html"))

	return &Server{
		cfg:         cfg,
		db:          db,
		store:       store,
		queue:       q,
		rateLimiter: rl,
		idProvider:  idp,
		subSvc:      subSvc,
		templates:   tmpl,
		logger:      logger,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))))
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /submissions/{id}", s.handleSubmissionPage)
	mux.HandleFunc("POST /api/submissions", s.handleCreateSubmission)
	mux.HandleFunc("GET /api/submissions/{id}", s.handleGetSubmission)

	var h http.Handler = mux
	h = s.reqLogMiddleware(h)
	h = s.securityHeaders(h)
	h = s.identityMiddleware(h)
	h = s.recoverMiddleware(h)

	return h
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := s.db.PingContext(ctx); err != nil {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("db: unreachable"))
		return
	}

	if err := s.rateLimiter.Ping(ctx); err != nil {
		s.logger.Warn("readyz redis ping", "error", err)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("redis: unreachable"))
		return
	}

	f, err := os.CreateTemp(s.cfg.SourceStorageRoot, ".readyz-*")
	if err != nil {
		s.logger.Warn("readyz storage", "error", err)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("storage: not writable"))
		return
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		os.Remove(name)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	os.Remove(name)

	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	id := getIdentity(r)
	if id == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	csrfToken := s.idProvider.CSRFToken(id.ParticipantID)

	data := map[string]interface{}{
		"Title":          "HUSTack \u2014 Trusted Runtime",
		"MaxSourceBytes": s.cfg.MaxSourceBytes,
		"MaxSourceMiB":   s.cfg.MaxSourceBytes / (1024 * 1024),
		"CSRFToken":      csrfToken,
	}
	s.renderTemplate(w, "index.html", data)
}

func (s *Server) handleSubmissionPage(w http.ResponseWriter, r *http.Request) {
	submissionID := r.PathValue("id")
	if submissionID == "" {
		http.NotFound(w, r)
		return
	}

	id := getIdentity(r)
	if id == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	_, err := s.subSvc.GetSubmission(r.Context(), submissionID, id.ParticipantID)
	if err != nil {
		if errors.Is(err, submissions.ErrOwnership) {
			http.NotFound(w, r)
			return
		}
		s.logger.Error("get submission page", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := map[string]interface{}{
		"Title":        "Submission Result - HUSTack",
		"SubmissionID": submissionID,
		"PollEndpoint": "/api/submissions/" + submissionID,
	}
	s.renderTemplate(w, "result.html", data)
}

func (s *Server) handleCreateSubmission(w http.ResponseWriter, r *http.Request) {
	id := getIdentity(r)
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.RequestBodyLimit)

	clientIP := identity.ExtractClientIP(r, s.cfg)

	result, err := s.subSvc.CreateSubmission(r, id.ParticipantID, clientIP)
	if err != nil {
		var rateLimitErr *submissions.RateLimitError
		if errors.As(err, &rateLimitErr) {
			w.Header().Set("Retry-After", itoa(rateLimitErr.RetryAfter))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
			return
		}

		switch {
		case errors.Is(err, submissions.ErrOriginNotAllowed):
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "origin not allowed"})
		case errors.Is(err, submissions.ErrCSRFInvalid):
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid CSRF token"})
		case errors.Is(err, submissions.ErrBodyTooLarge):
			w.Header().Set("Retry-After", "60")
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
		case errors.Is(err, submissions.ErrBothSources),
			errors.Is(err, submissions.ErrNoSource),
			errors.Is(err, submissions.ErrEmptySource),
			errors.Is(err, storage.ErrEmptySource),
			errors.Is(err, submissions.ErrInvalidExtension),
			errors.Is(err, submissions.ErrMultipleFiles),
			errors.Is(err, submissions.ErrMultipleText),
			errors.Is(err, submissions.ErrUnknownPart):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		case errors.Is(err, submissions.ErrSizeExceeded),
			errors.Is(err, storage.ErrSizeExceeded):
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "source too large"})
		case errors.Is(err, submissions.ErrContainsNUL),
			errors.Is(err, storage.ErrContainsNUL):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source contains NUL byte"})
		case errors.Is(err, database.ErrGlobalCapacity):
			w.Header().Set("Retry-After", "15")
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "system is busy"})
		case errors.Is(err, database.ErrParticipantQueuedQuota):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "queued submission quota exceeded"})
		default:
			s.logger.Error("create submission", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		}
		return
	}

	writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) handleGetSubmission(w http.ResponseWriter, r *http.Request) {
	submissionID := r.PathValue("id")
	if submissionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing submission id"})
		return
	}

	id := getIdentity(r)
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	allowed, retryAfter, err := s.rateLimiter.AllowPoll(id.ParticipantID)
	if err != nil {
		s.logger.Warn("poll rate limit error, allowing", "participant_id", id.ParticipantID[:8], "error", err)
	} else if !allowed {
		w.Header().Set("Retry-After", itoa(retryAfter))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
		return
	}

	result, err := s.subSvc.GetSubmission(r.Context(), submissionID, id.ParticipantID)
	if err != nil {
		if errors.Is(err, submissions.ErrOwnership) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		s.logger.Error("get submission", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) renderTemplate(w http.ResponseWriter, name string, data interface{}) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, name, data); err != nil {
		s.logger.Error("template render", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

func uuidString() string {
	return uuid.New().String()
}
