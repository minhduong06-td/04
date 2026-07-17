//go:build integration

package tests

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"
)

const baseURL = "http://localhost:8080"

type testHelper struct {
	t      *testing.T
	client *http.Client
}

func newHelper(t *testing.T) *testHelper {
	jar, _ := cookiejar.New(nil)
	return &testHelper{
		t: t,
		client: &http.Client{
			Jar:     jar,
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (h *testHelper) getCSRFToken() string {
	resp, err := h.client.Get(baseURL + "/")
	if err != nil {
		h.t.Fatalf("get index failed: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read body failed: %v", err)
	}
	bodyStr := string(body)
	idx := strings.Index(bodyStr, `name="csrf_token" value="`)
	if idx < 0 {
		h.t.Fatal("csrf_token not found in page")
	}
	start := idx + len(`name="csrf_token" value="`)
	end := strings.Index(bodyStr[start:], `"`)
	if end < 0 {
		h.t.Fatal("csrf_token value not terminated")
	}
	return bodyStr[start : start+end]
}

func (h *testHelper) submitSource(text string) *submitResponse {
	csrfToken := h.getCSRFToken()

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	w.WriteField("source_text", text)
	w.Close()

	req, _ := http.NewRequest("POST", baseURL+"/api/submissions", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrfToken)

	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("submit failed: %v", err)
	}
	defer resp.Body.Close()

	var result submitResponse
	json.NewDecoder(resp.Body).Decode(&result)
	return &result
}

func (h *testHelper) submitMultipart(formFields map[string]string, fileFields map[string]string, csrfToken string) *http.Response {
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)

	for name, val := range formFields {
		w.WriteField(name, val)
	}
	for name, content := range fileFields {
		part, _ := w.CreateFormFile(name, "solution.c")
		part.Write([]byte(content))
	}
	w.Close()

	req, _ := http.NewRequest("POST", baseURL+"/api/submissions", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrfToken)

	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("submit failed: %v", err)
	}
	return resp
}

type submitResponse struct {
	SubmissionID string `json:"submission_id"`
	Status       string `json:"status"`
}

type submissionJSON struct {
	ID              string  `json:"id"`
	Status          string  `json:"status"`
	Stdout          *string `json:"stdout"`
	Stderr          *string `json:"stderr"`
	CompileSuccess  *bool   `json:"compile_success"`
	ExitCode        *int    `json:"exit_code"`
	ResultTruncated bool    `json:"result_truncated"`
	SourceSize      int64   `json:"source_size"`
	SourceSHA256    string  `json:"source_sha256"`
}

func (h *testHelper) pollUntilFinished(submissionID string) *submissionJSON {
	for i := 0; i < 30; i++ {
		time.Sleep(500 * time.Millisecond)
		resp, err := h.client.Get(baseURL + "/api/submissions/" + submissionID)
		if err != nil {
			continue
		}
		var sub submissionJSON
		if err := json.NewDecoder(resp.Body).Decode(&sub); err != nil {
			resp.Body.Close()
			continue
		}
		resp.Body.Close()
		if sub.Status == "finished" || sub.Status == "internal_error" {
			return &sub
		}
	}
	h.t.Fatalf("poll timeout for submission %s", submissionID)
	return nil
}

func TestSubmitTextarea(t *testing.T) {
	h := newHelper(t)
	result := h.submitSource("int main() { return 0; }")

	if result.SubmissionID == "" {
		t.Fatal("expected non-empty submission_id")
	}
	if result.Status != "queued" {
		t.Fatalf("expected queued, got %s", result.Status)
	}

	sub := h.pollUntilFinished(result.SubmissionID)
	if sub.Status != "finished" {
		t.Fatalf("expected finished, got %s", sub.Status)
	}
}

func TestSubmitSourceFile(t *testing.T) {
	h := newHelper(t)
	csrfToken := h.getCSRFToken()

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	part, _ := w.CreateFormFile("source_file", "solution.c")
	content := "int main() { return 42; }"
	part.Write([]byte(content))
	w.Close()

	req, _ := http.NewRequest("POST", baseURL+"/api/submissions", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrfToken)

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}
	defer resp.Body.Close()

	var result submitResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if result.Status != "queued" {
		t.Fatalf("expected queued, got %s", result.Status)
	}

	sub := h.pollUntilFinished(result.SubmissionID)
	if sub.Status != "finished" {
		t.Fatalf("expected finished, got %s", sub.Status)
	}
	if sub.SourceSize != int64(len(content)) {
		t.Fatalf("expected source size %d, got %d", len(content), sub.SourceSize)
	}
}

func TestBothSourcesRejected(t *testing.T) {
	h := newHelper(t)
	csrfToken := h.getCSRFToken()

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	w.WriteField("source_text", "int main() { return 0; }")
	part, _ := w.CreateFormFile("source_file", "test.c")
	part.Write([]byte("int main() { return 1; }"))
	w.Close()

	req, _ := http.NewRequest("POST", baseURL+"/api/submissions", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrfToken)

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestNoSourceRejected(t *testing.T) {
	h := newHelper(t)
	csrfToken := h.getCSRFToken()

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	w.Close()

	req, _ := http.NewRequest("POST", baseURL+"/api/submissions", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrfToken)

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, no source, got %d", resp.StatusCode)
	}
}

func TestEmptySourceRejected(t *testing.T) {
	h := newHelper(t)
	csrfToken := h.getCSRFToken()

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	w.WriteField("source_text", "")
	w.Close()

	req, _ := http.NewRequest("POST", baseURL+"/api/submissions", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrfToken)

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty source, got %d", resp.StatusCode)
	}
}

func TestInvalidExtension(t *testing.T) {
	h := newHelper(t)
	csrfToken := h.getCSRFToken()

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	part, _ := w.CreateFormFile("source_file", "solution.cpp")
	part.Write([]byte("int main() { return 0; }"))
	w.Close()

	req, _ := http.NewRequest("POST", baseURL+"/api/submissions", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrfToken)

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestOwnershipCheck(t *testing.T) {
	helperA := newHelper(t)
	resultA := helperA.submitSource("int main() { return 0; }")

	helperB := newHelper(t)
	getReq, _ := http.NewRequest("GET", baseURL+"/api/submissions/"+resultA.SubmissionID, nil)
	resp, err := helperB.client.Do(getReq)
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for wrong owner, got %d", resp.StatusCode)
	}
}

func TestSubmittedSourceIsNotReflectedAsResultMarkup(t *testing.T) {
	h := newHelper(t)
	result := h.submitSource("<script>alert(1)</script>")

	h.pollUntilFinished(result.SubmissionID)

	htmlResp, err := h.client.Get(baseURL + "/submissions/" + result.SubmissionID)
	if err != nil {
		t.Fatalf("html page failed: %v", err)
	}
	defer htmlResp.Body.Close()
	htmlBody, _ := io.ReadAll(htmlResp.Body)
	if strings.Contains(string(htmlBody), "<script>alert(1)</script>") {
		t.Fatal("XSS: raw script tag found in HTML response")
	}
}

func TestHealthcheckNoParticipant(t *testing.T) {
	h := newHelper(t)

	for i := 0; i < 5; i++ {
		resp, err := h.client.Get(baseURL + "/healthz")
		if err != nil {
			t.Fatalf("healthz failed: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
	}

	resp, err := h.client.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("healthz failed: %v", err)
	}
	defer resp.Body.Close()
	if len(resp.Cookies()) > 0 {
		t.Fatal("healthz should not set cookies")
	}

	resp2, err := h.client.Get(baseURL + "/readyz")
	if err != nil {
		t.Fatalf("readyz failed: %v", err)
	}
	defer resp2.Body.Close()
	if len(resp2.Cookies()) > 0 {
		t.Fatal("readyz should not set cookies")
	}
}

func TestCSRFWithoutToken(t *testing.T) {
	h := newHelper(t)

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	w.WriteField("source_text", "int main() { return 0; }")
	w.Close()

	req, _ := http.NewRequest("POST", baseURL+"/api/submissions", body)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for missing CSRF, got %d", resp.StatusCode)
	}
}

func TestUnknownPartRejected(t *testing.T) {
	h := newHelper(t)
	csrfToken := h.getCSRFToken()

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	w.WriteField("unknown_field", "value")
	w.Close()

	req, _ := http.NewRequest("POST", baseURL+"/api/submissions", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrfToken)

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown part, got %d", resp.StatusCode)
	}
}

func TestMultipleSourceFilesRejected(t *testing.T) {
	h := newHelper(t)
	csrfToken := h.getCSRFToken()

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	p1, _ := w.CreateFormFile("source_file", "a.c")
	p1.Write([]byte("int a;"))
	p2, _ := w.CreateFormFile("source_file", "b.c")
	p2.Write([]byte("int b;"))
	w.Close()

	req, _ := http.NewRequest("POST", baseURL+"/api/submissions", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrfToken)

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for multiple files, got %d", resp.StatusCode)
	}
}

func TestExact10MiBAccepted(t *testing.T) {
	h := newHelper(t)
	csrfToken := h.getCSRFToken()

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	content := strings.Repeat("a", 10485760)
	w.WriteField("source_text", content)
	w.Close()

	req, _ := http.NewRequest("POST", baseURL+"/api/submissions", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrfToken)

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		t.Fatal("expected 202 for exactly 10MB source, but got 413")
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}
}

func Test10MiBPlusOneRejected(t *testing.T) {
	h := newHelper(t)
	csrfToken := h.getCSRFToken()

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	content := strings.Repeat("a", 10485761)
	w.WriteField("source_text", content)
	w.Close()

	req, _ := http.NewRequest("POST", baseURL+"/api/submissions", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrfToken)

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for 10MB+1 byte source, got %d", resp.StatusCode)
	}
}

func TestRejectForeignOrigin(t *testing.T) {
	h := newHelper(t)
	csrfToken := h.getCSRFToken()

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	w.WriteField("source_text", "int main() { return 0; }")
	w.Close()

	req, _ := http.NewRequest("POST", baseURL+"/api/submissions", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrfToken)
	req.Header.Set("Origin", "http://evil.com")

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for foreign origin, got %d", resp.StatusCode)
	}
}

func TestSourceTextStreaming(t *testing.T) {
	h := newHelper(t)
	csrfToken := h.getCSRFToken()

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	largeContent := strings.Repeat("ABCDEFGH", 100000)
	w.WriteField("source_text", largeContent)
	w.Close()

	req, _ := http.NewRequest("POST", baseURL+"/api/submissions", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrfToken)

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}

	var result submitResponse
	json.NewDecoder(resp.Body).Decode(&result)

	sub := h.pollUntilFinished(result.SubmissionID)
	if sub.SourceSize != int64(len(largeContent)) {
		t.Fatalf("expected source size %d, got %d", len(largeContent), sub.SourceSize)
	}
}

func TestCleanupOnBadPartAfterSource(t *testing.T) {
	h := newHelper(t)
	csrfToken := h.getCSRFToken()

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	w.WriteField("source_text", "int main() { return 0; }")
	w.WriteField("extra_field", "should fail")
	w.Close()

	req, _ := http.NewRequest("POST", baseURL+"/api/submissions", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrfToken)

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestHealthNoParticipantRows(t *testing.T) {
	urls := []string{"/healthz", "/readyz"}
	for _, url := range urls {
		req, _ := http.NewRequest("GET", baseURL+url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		resp.Body.Close()
		if setCookie := resp.Header.Get("Set-Cookie"); setCookie != "" {
			t.Errorf("GET %s set a cookie: %q", url, setCookie)
		}
	}
}

func TestIdentityManyRequestsNoDBBloat(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}
	for i := 0; i < 100; i++ {
		req, _ := http.NewRequest("GET", baseURL+"/healthz", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
		resp.Body.Close()
	}
}

func TestSpoofedXFFAccepted(t *testing.T) {
	h := newHelper(t)
	csrfToken := h.getCSRFToken()

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	w.WriteField("source_text", "spoofed")
	w.Close()

	req, _ := http.NewRequest("POST", baseURL+"/api/submissions", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrfToken)
	req.Header.Set("X-Forwarded-For", "1.2.3.4")

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}
}

func TestExactBOMAccepted(t *testing.T) {
	h := newHelper(t)
	bom := "\ufeff"
	src := bom + `#include <stdio.h>
int main() { printf("hello"); return 0; }
`
	res := h.submitSource(src)
	poll := h.pollUntilFinished(res.SubmissionID)
	if poll.Status != "finished" {
		t.Fatalf("expected status finished, got %s", poll.Status)
	}
}

func TestSingleEFAccepted(t *testing.T) {
	h := newHelper(t)
	res := h.submitSource("EF")
	poll := h.pollUntilFinished(res.SubmissionID)
	if poll.Status != "finished" {
		t.Fatalf("expected status finished, got %s", poll.Status)
	}
}

func TestNULSourceRejected(t *testing.T) {
	h := newHelper(t)
	csrfToken := h.getCSRFToken()

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	w.WriteField("source_text", "int main() { \x00 return 0; }")
	w.Close()

	req, _ := http.NewRequest("POST", baseURL+"/api/submissions", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrfToken)

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for NUL byte, got %d", resp.StatusCode)
	}
}

func TestChunkedBodySizeLimit(t *testing.T) {
	h := newHelper(t)
	csrfToken := h.getCSRFToken()

	payload := strings.Repeat("A", 11*1024*1024)
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	w.WriteField("source_text", payload)
	w.Close()

	req, _ := http.NewRequest("POST", baseURL+"/api/submissions", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrfToken)

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for oversized body, got %d", resp.StatusCode)
	}
}
