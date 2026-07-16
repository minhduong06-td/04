package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testStore(t *testing.T) *LocalStore {
	t.Helper()
	return NewLocalStore(t.TempDir())
}

func TestSaveStreamAndCommit(t *testing.T) {
	store := testStore(t)
	content := strings.NewReader("int main() { return 0; }")
	tmpPath, size, sha256hex, err := store.SaveStream(content, 10485760)
	if err != nil {
		t.Fatalf("SaveStream failed: %v", err)
	}
	if size == 0 {
		t.Fatal("expected non-zero size")
	}
	if sha256hex == "" {
		t.Fatal("expected non-empty sha256")
	}

	if err := store.Commit("test-1.c", tmpPath); err != nil {
		t.Fatalf("Commit failed: %v", err)
	}

	data, err := store.Read("test-1.c")
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if string(data) != "int main() { return 0; }" {
		t.Fatalf("unexpected content: %s", data)
	}

	if err := store.Delete("test-1.c"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
}

func TestEmptySource(t *testing.T) {
	store := testStore(t)
	_, _, _, err := store.SaveStream(strings.NewReader(""), 10485760)
	if err != ErrEmptySource {
		t.Fatalf("expected ErrEmptySource, got %v", err)
	}
}

func TestExactMaxSize(t *testing.T) {
	store := testStore(t)
	content := strings.NewReader(strings.Repeat("a", 100))
	tmpPath, size, _, err := store.SaveStream(content, 100)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if size != 100 {
		t.Fatalf("expected size 100, got %d", size)
	}
	if err := store.Commit("exact.c", tmpPath); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	store.Delete("exact.c")
}

func TestOneByteOverMaxSize(t *testing.T) {
	store := testStore(t)
	content := strings.NewReader(strings.Repeat("a", 101))
	_, _, _, err := store.SaveStream(content, 100)
	if err != ErrSizeExceeded {
		t.Fatalf("expected ErrSizeExceeded, got %v", err)
	}
}

func TestNULByte(t *testing.T) {
	store := testStore(t)
	content := strings.NewReader("abc\x00def")
	_, _, _, err := store.SaveStream(content, 10485760)
	if err != ErrContainsNUL {
		t.Fatalf("expected ErrContainsNUL, got %v", err)
	}
}

func TestCommitNoOverwrite(t *testing.T) {
	store := testStore(t)
	tmpPath, _, _, err := store.SaveStream(strings.NewReader("first"), 10485760)
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := store.Commit("unique.c", tmpPath); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	defer store.Delete("unique.c")

	tmpPath2, _, _, err := store.SaveStream(strings.NewReader("second"), 10485760)
	if err != nil {
		t.Fatalf("second save: %v", err)
	}

	err = store.Commit("unique.c", tmpPath2)
	if err == nil {
		t.Fatal("expected error for overwrite")
	}

	if _, err := os.Stat(tmpPath2); err == nil {
		t.Fatal("temp file should be removed on commit failure")
	}
}

func TestValidateExtension(t *testing.T) {
	cases := []struct {
		name  string
		valid bool
	}{
		{"test.c", true},
		{"test.C", true},
		{"test.cxx", false},
		{"test.cpp", false},
		{"test.h", false},
		{"", false},
		{"noextension", false},
		{"test.c.backup", false},
	}
	for _, tc := range cases {
		got := ValidateExtension(tc.name)
		if got != tc.valid {
			t.Errorf("ValidateExtension(%q) = %v, want %v", tc.name, got, tc.valid)
		}
	}
}

func TestPathTraversal(t *testing.T) {
	store := testStore(t)
	path := store.safePath("../../etc/passwd")
	if path != "" {
		t.Fatal("expected empty path for traversal")
	}
	path = store.safePath("normal-file.c")
	if path == "" {
		t.Fatal("expected non-empty path for safe filename")
	}
}

func TestCommitPermissions(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewLocalStore(tmpDir)
	tmpPath, _, _, err := store.SaveStream(strings.NewReader("atomic test"), 10485760)
	if err != nil {
		t.Fatalf("SaveStream failed: %v", err)
	}
	if err := store.Commit("atomic.c", tmpPath); err != nil {
		t.Fatalf("Commit failed: %v", err)
	}
	fullPath := filepath.Join(tmpDir, "atomic.c")
	info, err := os.Stat(fullPath)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if info.Mode().Perm() != 0640 {
		t.Fatalf("expected 0640, got %o", info.Mode().Perm())
	}
	store.Delete("atomic.c")
}

func TestCreateTemp(t *testing.T) {
	store := testStore(t)
	f, err := store.CreateTemp()
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	f.Close()
	os.Remove(f.Name())
}
