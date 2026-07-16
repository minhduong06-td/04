package identity

import (
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hustack/internal/config"
)

func testProvider(t *testing.T) *CookieProvider {
	t.Helper()
	cfg := &config.Config{
		CookieHMACSecret:    "test-secret-that-is-at-least-32-bytes-long!!",
		PublicBaseOrigin:    "http://localhost:8080",
		CookieMaxAgeSeconds: 604800,
		CSRFTokenDuration:   2 * time.Hour,
	}
	return NewCookieProvider(cfg)
}

func TestGenerateParticipantID(t *testing.T) {
	id1, err := GenerateParticipantID()
	if err != nil {
		t.Fatalf("GenerateParticipantID failed: %v", err)
	}
	id2, err := GenerateParticipantID()
	if err != nil {
		t.Fatalf("GenerateParticipantID failed: %v", err)
	}
	if id1 == id2 {
		t.Fatal("expected unique IDs")
	}
	if !isValidUUID(id1) {
		t.Fatal("expected valid UUID format")
	}
}

func TestSetAndResolveCookie(t *testing.T) {
	p := testProvider(t)
	pid, _ := GenerateParticipantID()

	w := httptest.NewRecorder()
	if err := p.SetIdentity(w, pid); err != nil {
		t.Fatalf("SetIdentity failed: %v", err)
	}

	resp := w.Result()
	cookies := resp.Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected cookie")
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Cookie", cookies[0].String())

	identity, err := p.Resolve(req)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if identity.ParticipantID != pid {
		t.Fatalf("expected %s, got %s", pid, identity.ParticipantID)
	}
	if identity.ExpiresAt.IsZero() {
		t.Fatal("expected non-zero expiry")
	}
	if identity.IssuedAt.IsZero() {
		t.Fatal("expected non-zero issued_at")
	}
}

func TestTamperedCookie(t *testing.T) {
	p := testProvider(t)
	pid, _ := GenerateParticipantID()

	w := httptest.NewRecorder()
	p.SetIdentity(w, pid)

	cookie := w.Result().Cookies()[0]
	val := cookie.Value

	// Tamper the payload by changing one byte in the signature part
	parts := strings.SplitN(val, ".", 2)
	if len(parts) != 2 {
		t.Fatal("unexpected cookie format")
	}
	// Flip a bit in the signature
	sigBytes := []byte(parts[0])
	sigBytes[0] ^= 0x01
	tampered := string(sigBytes) + "." + parts[1]

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Cookie", cookie.Name+"="+tampered)

	_, err := p.Resolve(req)
	if err == nil {
		t.Fatal("expected error for tampered cookie")
	}
}

func TestCSRFTokenHeaderOnly(t *testing.T) {
	p := testProvider(t)
	pid, _ := GenerateParticipantID()

	w := httptest.NewRecorder()
	p.SetIdentity(w, pid)

	token := p.CSRFToken(pid)
	if token == "" {
		t.Fatal("expected non-empty token")
	}

	postReq := httptest.NewRequest("POST", "/api/submissions", nil)
	for _, c := range w.Result().Cookies() {
		postReq.AddCookie(c)
	}
	postReq.Header.Set("X-CSRF-Token", token)

	if !p.VerifyCSRFToken(postReq) {
		t.Fatal("expected valid CSRF token via header")
	}
}

func TestCSRFTokenFormValueRejected(t *testing.T) {
	// Verify that CSRF token via form value is NOT accepted (header only)
	p := testProvider(t)
	pid, _ := GenerateParticipantID()

	w := httptest.NewRecorder()
	p.SetIdentity(w, pid)
	token := p.CSRFToken(pid)

	body := strings.NewReader("csrf_token=" + token)
	postReq := httptest.NewRequest("POST", "/api/submissions", body)
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range w.Result().Cookies() {
		postReq.AddCookie(c)
	}

	if p.VerifyCSRFToken(postReq) {
		t.Fatal("expected CSRF via form value to be rejected")
	}
}

func TestInvalidCSRFToken(t *testing.T) {
	p := testProvider(t)
	pid, _ := GenerateParticipantID()

	w := httptest.NewRecorder()
	p.SetIdentity(w, pid)

	postReq := httptest.NewRequest("POST", "/api/submissions", nil)
	for _, c := range w.Result().Cookies() {
		postReq.AddCookie(c)
	}
	postReq.Header.Set("X-CSRF-Token", "invalid-token")

	if p.VerifyCSRFToken(postReq) {
		t.Fatal("expected invalid CSRF token")
	}
}

func TestCookieSignatureDifferentSecret(t *testing.T) {
	cfg1 := &config.Config{CookieHMACSecret: "secret-one-that-is-long-enough-for-test!!"}
	cfg2 := &config.Config{CookieHMACSecret: "secret-two-that-is-long-enough-for-test!!"}

	p1 := NewCookieProvider(cfg1)
	p2 := NewCookieProvider(cfg2)

	pid, _ := GenerateParticipantID()

	w := httptest.NewRecorder()
	p1.SetIdentity(w, pid)

	req := httptest.NewRequest("GET", "/", nil)
	for _, c := range w.Result().Cookies() {
		req.AddCookie(c)
	}

	_, err := p2.Resolve(req)
	if err == nil {
		t.Fatal("expected error with wrong secret")
	}
}

func TestCookieAttributes(t *testing.T) {
	cfg := &config.Config{
		CookieHMACSecret: "test-secret-that-is-at-least-32-bytes-long!!",
	}
	p := NewCookieProvider(cfg)
	pid, _ := GenerateParticipantID()

	w := httptest.NewRecorder()
	p.SetIdentity(w, pid)

	sc := w.Result().Header.Get("Set-Cookie")
	if !strings.Contains(sc, "HttpOnly") {
		t.Fatal("expected HttpOnly")
	}
	if !strings.Contains(sc, "SameSite=Lax") {
		t.Fatal("expected SameSite=Lax")
	}
	if strings.Contains(sc, "Secure") {
		t.Fatal("unexpected Secure in development")
	}

	prodCfg := &config.Config{
		CookieHMACSecret: "test-secret-that-is-at-least-32-bytes-long!!",
		AppEnv:           "production",
	}
	p2 := NewCookieProvider(prodCfg)
	w2 := httptest.NewRecorder()
	p2.SetIdentity(w2, pid)
	sc2 := w2.Result().Header.Get("Set-Cookie")
	if !strings.Contains(sc2, "Secure") {
		t.Fatal("expected Secure in production")
	}
}

func TestOriginAllowed(t *testing.T) {
	p := testProvider(t)

	req := httptest.NewRequest("POST", "/api/submissions", nil)
	req.Header.Set("Origin", "http://localhost:8080")

	if !p.VerifyOrigin(req) {
		t.Fatal("expected allowed origin")
	}
}

func TestOriginRejected(t *testing.T) {
	p := testProvider(t)

	req := httptest.NewRequest("POST", "/api/submissions", nil)
	req.Header.Set("Origin", "http://evil.com")

	if p.VerifyOrigin(req) {
		t.Fatal("expected rejected origin")
	}
}

func TestOriginMissing(t *testing.T) {
	p := testProvider(t)

	req := httptest.NewRequest("POST", "/api/submissions", nil)

	if !p.VerifyOrigin(req) {
		t.Fatal("expected allowed when origin missing")
	}
}

func TestResolveWithoutCookie(t *testing.T) {
	p := testProvider(t)
	req := httptest.NewRequest("GET", "/", nil)

	_, err := p.Resolve(req)
	if err != ErrInvalidCookie {
		t.Fatalf("expected ErrInvalidCookie, got %v", err)
	}
}

func TestIsValidUUID(t *testing.T) {
	cases := []struct {
		input string
		valid bool
	}{
		{"550e8400-e29b-41d4-a716-446655440000", true},
		{"not-a-uuid", false},
		{"", false},
		{"550e8400e29b41d4a716446655440000", false},
	}
	for _, tc := range cases {
		got := isValidUUID(tc.input)
		if got != tc.valid {
			t.Errorf("isValidUUID(%q) = %v, want %v", tc.input, got, tc.valid)
		}
	}
}

func TestExtractClientIPDirect(t *testing.T) {
	cfg := &config.Config{}
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.42:12345"

	ip := ExtractClientIP(req, cfg)
	if ip != "203.0.113.42" {
		t.Fatalf("expected 203.0.113.42, got %s", ip)
	}
}

func TestExtractClientIPIgnoresSpoofedXFF(t *testing.T) {
	cfg := &config.Config{}
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.42:12345"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Real-IP", "5.6.7.8")

	ip := ExtractClientIP(req, cfg)
	if ip != "203.0.113.42" {
		t.Fatalf("expected remote addr, got %s", ip)
	}
}

func TestExtractClientIPTrustedProxy(t *testing.T) {
	_, trustedNet, _ := net.ParseCIDR("172.16.0.0/12")
	cfg := &config.Config{
		UseTrustedProxyHeaders: true,
		TrustedProxies:         []*net.IPNet{trustedNet},
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "172.16.0.1:12345"
	req.Header.Set("X-Forwarded-For", "203.0.113.42")

	ip := ExtractClientIP(req, cfg)
	if ip != "203.0.113.42" {
		t.Fatalf("expected X-Forwarded-For value, got %s", ip)
	}
}

func TestExtractClientIPUntrustedProxy(t *testing.T) {
	_, trustedNet, _ := net.ParseCIDR("10.0.0.0/8")
	cfg := &config.Config{
		UseTrustedProxyHeaders: true,
		TrustedProxies:         []*net.IPNet{trustedNet},
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.42:12345"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")

	ip := ExtractClientIP(req, cfg)
	if ip != "203.0.113.42" {
		t.Fatalf("expected remote addr for untrusted proxy, got %s", ip)
	}
}

func TestExtractClientIPUsesXRealIP(t *testing.T) {
	_, trustedNet, _ := net.ParseCIDR("172.16.0.0/12")
	cfg := &config.Config{
		UseTrustedProxyHeaders: true,
		TrustedProxies:         []*net.IPNet{trustedNet},
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "172.16.0.1:12345"
	req.Header.Set("X-Real-IP", "203.0.113.99")

	ip := ExtractClientIP(req, cfg)
	if ip != "203.0.113.99" {
		t.Fatalf("expected X-Real-IP value, got %s", ip)
	}
}
