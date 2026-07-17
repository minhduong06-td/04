//go:build phase1acceptance

package tests

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

const hostileOutput = `<script>window.__hustack_xss = true</script>`

type acceptanceHelper struct {
	t       *testing.T
	baseURL string
	client  *http.Client
}

func newAcceptanceHelper(t *testing.T, baseURL string) *acceptanceHelper {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &acceptanceHelper{t: t, baseURL: baseURL, client: &http.Client{Jar: jar, Timeout: 10 * time.Second}}
}

func (h *acceptanceHelper) csrf() string {
	h.t.Helper()
	resp, err := h.client.Get(h.baseURL + "/")
	if err != nil {
		h.t.Fatalf("get CSRF page: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	prefix := `name="csrf_token" value="`
	start := bytes.Index(body, []byte(prefix))
	if start < 0 {
		h.t.Fatal("CSRF token missing")
	}
	start += len(prefix)
	end := bytes.IndexByte(body[start:], '"')
	if end < 0 {
		h.t.Fatal("CSRF token unterminated")
	}
	return string(body[start : start+end])
}

func (h *acceptanceHelper) submit(token, source string) (*http.Response, []byte) {
	h.t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("source_text", source); err != nil {
		h.t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		h.t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, h.baseURL+"/api/submissions", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-CSRF-Token", token)
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("submit: %v", err)
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		h.t.Fatal(err)
	}
	return resp, data
}

func acceptanceDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("PHASE1_POSTGRES_URL")
	if dsn == "" {
		dsn = "postgres://hustack:hustack@127.0.0.1:25432/hustack?sslmode=disable"
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		t.Fatalf("required PostgreSQL unavailable: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func resetAcceptanceState(t *testing.T) *sql.DB {
	t.Helper()
	db := acceptanceDB(t)
	if _, err := db.Exec(`TRUNCATE submission_outbox, submissions, participants RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("reset PostgreSQL: %v", err)
	}
	rc := redis.NewClient(&redis.Options{Addr: "127.0.0.1:26379"})
	if err := rc.FlushDB(context.Background()).Err(); err != nil {
		rc.Close()
		t.Fatalf("required Redis unavailable: %v", err)
	}
	rc.Close()
	return db
}

func acceptanceSourceCount(t *testing.T) int {
	t.Helper()
	container := os.Getenv("PHASE1_API_CONTAINER")
	if container == "" {
		container = "hustack-phase1-accept-api-1"
	}
	out, err := exec.Command("docker", "exec", container, "find", "/var/lib/hustack/sources", "-maxdepth", "1", "-type", "f", "-print").Output()
	if err != nil {
		t.Fatalf("inspect source volume: %v", err)
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}

func TestApplicationParticipantRateLimit(t *testing.T) {
	resetAcceptanceState(t)
	h := newAcceptanceHelper(t, "http://127.0.0.1:18080")
	token := h.csrf()
	for i := 0; i < 2; i++ {
		resp, body := h.submit(token, fmt.Sprintf("int allowed_%d;", i))
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("allowed request %d: status=%d body=%s", i+1, resp.StatusCode, body)
		}
	}
	resp, body := h.submit(token, "int rejected;")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected application 429, got %d: %s", resp.StatusCode, body)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("application 429 content type = %q", resp.Header.Get("Content-Type"))
	}
	retryAfter, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || retryAfter <= 0 || resp.Header.Get("Server") == "nginx" {
		t.Fatalf("application 429 headers do not prove direct Go response: %#v", resp.Header)
	}
}

func TestNginxSubmissionRateLimit(t *testing.T) {
	resetAcceptanceState(t)
	h := newAcceptanceHelper(t, "http://127.0.0.1:8080")
	token := h.csrf()
	const requests = 12
	type observed struct {
		status      int
		contentType string
	}
	results := make(chan observed, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, _ := h.submit(token, fmt.Sprintf("int nginx_%d;", i))
			results <- observed{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type")}
		}(i)
	}
	wg.Wait()
	close(results)
	foundNginx429 := false
	for result := range results {
		if result.status == http.StatusTooManyRequests && !strings.HasPrefix(result.contentType, "application/json") {
			foundNginx429 = true
		}
	}
	if !foundNginx429 {
		t.Fatal("no distinct non-JSON Nginx 429 observed")
	}
}

func TestGlobalCapacityRejectsWithoutPartialState(t *testing.T) {
	db := resetAcceptanceState(t)
	sourcesBefore := acceptanceSourceCount(t)
	h1 := newAcceptanceHelper(t, "http://127.0.0.1:18080")
	h2 := newAcceptanceHelper(t, "http://127.0.0.1:18080")
	resp1, body1 := h1.submit(h1.csrf(), "int first;")
	if resp1.StatusCode != http.StatusAccepted {
		t.Fatalf("first submission: %d %s", resp1.StatusCode, body1)
	}
	resp2, body2 := h2.submit(h2.csrf(), "int second;")
	if resp2.StatusCode != http.StatusServiceUnavailable || resp2.Header.Get("Retry-After") == "" {
		t.Fatalf("capacity response: status=%d retry=%q body=%s", resp2.StatusCode, resp2.Header.Get("Retry-After"), body2)
	}
	var submissions, outbox int
	if err := db.QueryRow(`SELECT COUNT(*) FROM submissions`).Scan(&submissions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM submission_outbox`).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	sourcesAfter := acceptanceSourceCount(t)
	if submissions != 1 || outbox != 1 || sourcesAfter != sourcesBefore+1 {
		t.Fatalf("rejection left partial state: submissions=%d outbox=%d source_files_before=%d source_files_after=%d", submissions, outbox, sourcesBefore, sourcesAfter)
	}
}

func TestActualHostileResultUsesTextOnlySink(t *testing.T) {
	db := resetAcceptanceState(t)
	h := newAcceptanceHelper(t, "http://127.0.0.1:18080")
	resp, body := h.submit(h.csrf(), "int fixture;")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("fixture submit: %d %s", resp.StatusCode, body)
	}
	var created struct {
		SubmissionID string `json:"submission_id"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.SubmissionID == "" {
		t.Fatalf("decode fixture: %v %s", err, body)
	}
	if _, err := db.Exec(`UPDATE submissions SET status='finished',compile_success=TRUE,exit_code=0,stdout=$2,stderr=$2,finished_at=NOW(),updated_at=NOW() WHERE id=$1`, created.SubmissionID, hostileOutput); err != nil {
		t.Fatal(err)
	}
	jsonResp, err := h.client.Get(h.baseURL + "/api/submissions/" + created.SubmissionID)
	if err != nil {
		t.Fatal(err)
	}
	var result struct{ Stdout, Stderr *string }
	if err := json.NewDecoder(jsonResp.Body).Decode(&result); err != nil {
		jsonResp.Body.Close()
		t.Fatal(err)
	}
	jsonResp.Body.Close()
	if result.Stdout == nil || result.Stderr == nil || *result.Stdout != hostileOutput || *result.Stderr != hostileOutput {
		t.Fatalf("hostile output not returned as JSON data: %#v", result)
	}
	htmlResp, err := h.client.Get(h.baseURL + "/submissions/" + created.SubmissionID)
	if err != nil {
		t.Fatal(err)
	}
	htmlBody, _ := io.ReadAll(htmlResp.Body)
	htmlResp.Body.Close()
	if bytes.Contains(htmlBody, []byte(hostileOutput)) {
		t.Fatal("server-rendered HTML embeds executable hostile result")
	}
	script, err := os.ReadFile("../web/static/script.js")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, forbidden := range []string{"innerHTML", "insertAdjacentHTML", "document.write"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("dangerous frontend sink %q found", forbidden)
		}
	}
	if !strings.Contains(text, "setTextContent('r-stdout'") || !strings.Contains(text, "setTextContent('r-stderr'") || !strings.Contains(text, ".textContent = val") {
		t.Fatal("stdout/stderr are not provably assigned through text-only sinks")
	}
	goSources, _ := filepath.Glob("../internal/web/*.go")
	for _, path := range goSources {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("template.HTML")) {
			t.Fatalf("template.HTML found in %s", path)
		}
	}
}
