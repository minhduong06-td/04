package submissions

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestErrorMessages(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{ErrBothSources, "provide either source_text or source_file, not both"},
		{ErrNoSource, "provide either source_text or source_file"},
		{ErrEmptySource, "source is empty"},
		{ErrInvalidExtension, "file must have .c extension"},
		{ErrContainsNUL, "source contains NUL byte"},
		{ErrSizeExceeded, "source exceeds maximum size"},
		{ErrCSRFInvalid, "invalid CSRF token"},
		{ErrOwnership, "submission not found"},
		{ErrMultipleFiles, "multiple source files not accepted"},
		{ErrMultipleText, "multiple source_text parts not accepted"},
		{ErrUnknownPart, "unknown form part"},
		{ErrOriginNotAllowed, "origin not allowed"},
		{ErrBodyTooLarge, "request body too large"},
	}
	for _, tc := range cases {
		if tc.err.Error() != tc.want {
			t.Errorf("(%v).Error() = %q, want %q", tc.err, tc.err.Error(), tc.want)
		}
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"../../etc/passwd", ".._.._etc_passwd"},
		{"normal.c", "normal.c"},
		{"path\\traversal", "path_traversal"},
		{"file\x00withnul.c", "filewithnul.c"},
		{"a.c", "a.c"},
	}
	for _, tc := range cases {
		got := sanitizeFilename(tc.input)
		if got != tc.expected {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestSanitizeFilenameTruncation(t *testing.T) {
	long := strings.Repeat("a", 300) + ".c"
	result := sanitizeFilename(long)
	if len(result) > 255 {
		t.Fatalf("expected max 255 chars, got %d", len(result))
	}
	if !strings.HasSuffix(result, ".c") {
		t.Fatal("expected .c suffix preserved")
	}
}

func TestSanitizeFilenameShort(t *testing.T) {
	short := "x.c"
	result := sanitizeFilename(short)
	if result != "x.c" {
		t.Fatalf("expected 'x.c', got %q", result)
	}
}

func TestStripBOM(t *testing.T) {
	input := []byte{0xEF, 0xBB, 0xBF, 'i', 'n', 't', ' ', 'm', 'a', 'i', 'n', '(', ')', ' ', '{', ' ', 'r', 'e', 't', 'u', 'r', 'n', ' ', '0', ';', ' ', '}'}
	reader := stripBOM(bytes.NewReader(input))
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(data) != 24 || data[0] != 'i' {
		t.Fatalf("expected BOM stripped, got len=%d first=%d", len(data), data[0])
	}
}

func TestStripBOMPreserveSingleEF(t *testing.T) {
	input := []byte{0xEF, 'i', 'n', 't'}
	reader := stripBOM(bytes.NewReader(input))
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(data) != 4 || data[0] != 0xEF {
		t.Fatalf("expected single EF preserved, got len=%d first=%x", len(data), data[0])
	}
}

func TestStripBOMNoBOM(t *testing.T) {
	input := []byte("int main() {}")
	reader := stripBOM(bytes.NewReader(input))
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "int main() {}" {
		t.Fatalf("expected original content, got %s", data)
	}
}

func TestStripBOMShortInput(t *testing.T) {
	input := []byte{0xEF, 0xBB}
	reader := stripBOM(bytes.NewReader(input))
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(data) != 2 {
		t.Fatalf("expected 2 bytes preserved, got %d", len(data))
	}
}

func TestRateLimitError(t *testing.T) {
	err := &RateLimitError{RetryAfter: 30}
	if err.Error() != "rate limit exceeded, retry after 30 seconds" {
		t.Fatalf("unexpected message: %s", err.Error())
	}
}
