package web

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"hustack/internal/identity"
)

type ctxKey string

const identityCtxKey ctxKey = "participant_identity"
const reqIDCtxKey ctxKey = "request_id"

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self' 'unsafe-inline'; "+
				"script-src 'self'; img-src 'self'; "+
				"connect-src 'self'; form-action 'self'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy",
			"camera=(), microphone=(), geolocation=(), interest-cohort=()")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) identityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}

		id, err := s.idProvider.Resolve(r)
		if err != nil {
			participantID, err := identity.GenerateParticipantID()
			if err != nil {
				s.logger.Error("generate participant id", "error", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if err := s.idProvider.SetIdentity(w, participantID); err != nil {
				s.logger.Error("set identity cookie", "error", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			id = &identity.Identity{ParticipantID: participantID}
		}

		ctx := context.WithValue(r.Context(), identityCtxKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) reqLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := uuidString()[:8]
		ctx := context.WithValue(r.Context(), reqIDCtxKey, reqID)

		start := time.Now()
		lrw := &logWriter{ResponseWriter: w, statusCode: http.StatusOK, wroteHeader: false}
		next.ServeHTTP(lrw, r.WithContext(ctx))

		pid := "-"
		if id := getIdentity(r); id != nil && len(id.ParticipantID) >= 8 {
			pid = id.ParticipantID[:8]
		}

		attrs := []slog.Attr{
			slog.String("req_id", reqID),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", lrw.statusCode),
			slog.Duration("duration", time.Since(start).Round(time.Millisecond)),
			slog.String("pid", pid),
		}

		s.logger.LogAttrs(ctx, slog.LevelInfo, "request", attrs...)
	})
}

func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("panic", "recover", rec)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func getIdentity(r *http.Request) *identity.Identity {
	if id, ok := r.Context().Value(identityCtxKey).(*identity.Identity); ok {
		return id
	}
	return nil
}

func getRequestID(r *http.Request) string {
	if id, ok := r.Context().Value(reqIDCtxKey).(string); ok {
		return id
	}
	return ""
}

type logWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
}

func (w *logWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.statusCode = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}
