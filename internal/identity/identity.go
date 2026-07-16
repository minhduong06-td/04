package identity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"hustack/internal/config"
)

var (
	ErrInvalidCookie = errors.New("invalid identity cookie")
	ErrExpiredCookie = errors.New("expired identity cookie")
	ErrInvalidOrigin = errors.New("invalid origin")
)

type Identity struct {
	ParticipantID string
	IssuedAt      time.Time
	ExpiresAt     time.Time
}

type Provider interface {
	Resolve(r *http.Request) (*Identity, error)
	SetIdentity(w http.ResponseWriter, participantID string) error
	ClearIdentity(w http.ResponseWriter)
	CSRFToken(pid string) string
	VerifyCSRFToken(r *http.Request) bool
	VerifyOrigin(r *http.Request) bool
}

type CookieProvider struct {
	cfg        *config.Config
	cookieName string
	csrfName   string
	hmacSecret []byte
	secure     bool
}

func NewCookieProvider(cfg *config.Config) *CookieProvider {
	return &CookieProvider{
		cfg:        cfg,
		cookieName: "hustack_sid",
		csrfName:   "hustack_csrf",
		hmacSecret: []byte(cfg.CookieHMACSecret),
		secure:     cfg.IsProduction(),
	}
}

func (p *CookieProvider) Resolve(r *http.Request) (*Identity, error) {
	cookie, err := r.Cookie(p.cookieName)
	if err != nil {
		return nil, ErrInvalidCookie
	}

	parts := strings.SplitN(cookie.Value, ".", 2)
	if len(parts) != 2 {
		return nil, ErrInvalidCookie
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, ErrInvalidCookie
	}

	payload := parts[1]
	mac := hmac.New(sha256.New, p.hmacSecret)
	mac.Write([]byte(payload))
	expected := mac.Sum(nil)

	if !hmac.Equal(sig, expected) {
		return nil, ErrInvalidCookie
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil, ErrInvalidCookie
	}

	fields := strings.SplitN(string(payloadBytes), ":", 3)
	if len(fields) != 3 {
		return nil, ErrInvalidCookie
	}

	participantID := fields[0]
	issuedAtUnix := fields[1]
	expiresAtUnix := fields[2]

	if !isValidUUID(participantID) {
		return nil, ErrInvalidCookie
	}

	issuedAt, err := parseInt64(issuedAtUnix)
	if err != nil {
		return nil, ErrInvalidCookie
	}

	expiresAt, err := parseInt64(expiresAtUnix)
	if err != nil {
		return nil, ErrInvalidCookie
	}

	now := time.Now().Unix()
	if now > expiresAt {
		return nil, ErrExpiredCookie
	}

	return &Identity{
		ParticipantID: participantID,
		IssuedAt:      time.Unix(issuedAt, 0),
		ExpiresAt:     time.Unix(expiresAt, 0),
	}, nil
}

func (p *CookieProvider) SetIdentity(w http.ResponseWriter, participantID string) error {
	now := time.Now()
	issuedAt := now.Unix()
	expiresAt := now.Add(time.Duration(p.cfg.CookieMaxAgeSeconds) * time.Second).Unix()

	payload := fmt.Sprintf("%s:%d:%d", participantID, issuedAt, expiresAt)
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))

	mac := hmac.New(sha256.New, p.hmacSecret)
	mac.Write([]byte(encoded))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	cookieValue := sig + "." + encoded

	http.SetCookie(w, &http.Cookie{
		Name:     p.cookieName,
		Value:    cookieValue,
		Path:     "/",
		HttpOnly: true,
		Secure:   p.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   p.cfg.CookieMaxAgeSeconds,
	})
	return nil
}

func (p *CookieProvider) ClearIdentity(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     p.cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   p.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func (p *CookieProvider) CSRFToken(pid string) string {
	now := time.Now().Unix()
	data := fmt.Sprintf("%s:%d", pid, now)
	mac := hmac.New(sha256.New, p.hmacSecret)
	mac.Write([]byte(data))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("%s.%s", sig, base64.RawURLEncoding.EncodeToString([]byte(data)))
}

func (p *CookieProvider) VerifyCSRFToken(r *http.Request) bool {
	token := r.Header.Get("X-CSRF-Token")
	if token == "" {
		return false
	}

	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return false
	}

	sigBase := parts[0]
	dataBase := parts[1]

	data, err := base64.RawURLEncoding.DecodeString(dataBase)
	if err != nil {
		return false
	}

	fields := strings.SplitN(string(data), ":", 2)
	if len(fields) != 2 {
		return false
	}

	participantID := fields[0]
	tsStr := fields[1]
	ts, err := parseInt64(tsStr)
	if err != nil {
		return false
	}

	id, err := p.Resolve(r)
	if err != nil {
		return false
	}

	if id.ParticipantID != participantID {
		return false
	}

	if time.Since(time.Unix(ts, 0)) > p.cfg.CSRFTokenDuration {
		return false
	}

	mac := hmac.New(sha256.New, p.hmacSecret)
	mac.Write([]byte(data))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(sigBase), []byte(expected))
}

func (p *CookieProvider) VerifyOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}

	origin = strings.TrimRight(origin, "/")

	allowed := p.cfg.PublicBaseOrigin
	allowed = strings.TrimRight(allowed, "/")

	if origin == allowed {
		return true
	}

	return false
}

func isValidUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}

func parseInt64(s string) (int64, error) {
	var n int64
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

func GenerateParticipantID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate participant id: %w", err)
	}
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func ExtractClientIP(r *http.Request, cfg *config.Config) string {
	if cfg.UseTrustedProxyHeaders {
		remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			remoteHost = r.RemoteAddr
		}
		remoteIP := net.ParseIP(remoteHost)
		if remoteIP != nil && cfg.IsTrustedProxy(remoteIP) {
			if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
				parts := strings.SplitN(forwarded, ",", 2)
				candidate := strings.TrimSpace(parts[0])
				if net.ParseIP(candidate) != nil {
					return candidate
				}
			}
			if realIP := r.Header.Get("X-Real-IP"); realIP != "" {
				candidate := strings.TrimSpace(realIP)
				if net.ParseIP(candidate) != nil {
					return candidate
				}
			}
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
