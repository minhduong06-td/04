package storage

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var (
	ErrSizeExceeded = errors.New("source exceeds maximum size")
	ErrContainsNUL  = errors.New("source contains NUL byte")
	ErrExtension    = errors.New("file must have .c extension")
	ErrEmptySource  = errors.New("source is empty")
)

type Store interface {
	Commit(key string, tempPath string) error
	Read(key string) ([]byte, error)
	Delete(key string) error
	Path(key string) string
}

type LocalStore struct {
	root string
}

func NewLocalStore(root string) *LocalStore {
	return &LocalStore{root: root}
}

func (s *LocalStore) ensureRoot() error {
	return os.MkdirAll(s.root, 0750)
}

func (s *LocalStore) safePath(key string) string {
	if key == "" || strings.Contains(key, "..") || strings.ContainsAny(key, "/\\") {
		return ""
	}
	if runtime.GOOS == "windows" && strings.ContainsAny(key, ":") {
		return ""
	}
	return filepath.Join(s.root, key)
}

func (s *LocalStore) CreateTemp() (*os.File, error) {
	if err := s.ensureRoot(); err != nil {
		return nil, err
	}
	return os.CreateTemp(s.root, ".tmp-*")
}

func (s *LocalStore) SaveStream(reader io.Reader, maxSize int64) (tempPath string, size int64, sha256hex string, err error) {
	tmpFile, err := s.CreateTemp()
	if err != nil {
		return "", 0, "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	cleanup := func() {
		tmpFile.Close()
		os.Remove(tmpPath)
	}

	hasher := sha256.New()
	limited := io.LimitReader(reader, maxSize+1)
	multi := io.TeeReader(limited, hasher)

	buf := make([]byte, 32768)
	var total int64
	var hasNUL bool
	var hasContent bool

	for {
		n, readErr := multi.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if bytes.IndexByte(chunk, 0) >= 0 {
				hasNUL = true
			}
			if !hasContent {
				for _, b := range chunk {
					if b != ' ' && b != '\t' && b != '\n' && b != '\r' {
						hasContent = true
						break
					}
				}
			}
			if total+int64(n) > maxSize {
				cleanup()
				return "", 0, "", ErrSizeExceeded
			}
			if _, err := tmpFile.Write(chunk); err != nil {
				cleanup()
				return "", 0, "", fmt.Errorf("write temp file: %w", err)
			}
			total += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			cleanup()
			return "", 0, "", fmt.Errorf("read source: %w", readErr)
		}
	}

	if total == 0 || !hasContent {
		cleanup()
		return "", 0, "", ErrEmptySource
	}

	if hasNUL {
		cleanup()
		return "", 0, "", ErrContainsNUL
	}

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return "", 0, "", fmt.Errorf("close temp file: %w", err)
	}

	sha256hex = fmt.Sprintf("%x", hasher.Sum(nil))
	return tmpPath, total, sha256hex, nil
}

func (s *LocalStore) Commit(key string, tempPath string) error {
	if tempPath == "" {
		return fmt.Errorf("empty temp path")
	}

	fullPath := s.safePath(key)
	if fullPath == "" {
		os.Remove(tempPath)
		return fmt.Errorf("invalid storage key: %s", key)
	}

	if _, err := os.Stat(fullPath); err == nil {
		os.Remove(tempPath)
		return fmt.Errorf("source already exists: %s", key)
	}

	if err := os.Rename(tempPath, fullPath); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("rename temp file: %w", err)
	}

	if err := os.Chmod(fullPath, 0640); err != nil {
		return fmt.Errorf("chmod source: %w", err)
	}

	return nil
}

func (s *LocalStore) Read(key string) ([]byte, error) {
	fullPath := s.safePath(key)
	if fullPath == "" {
		return nil, fmt.Errorf("invalid storage key: %s", key)
	}
	return os.ReadFile(fullPath)
}

func (s *LocalStore) Delete(key string) error {
	fullPath := s.safePath(key)
	if fullPath == "" {
		return fmt.Errorf("invalid storage key: %s", key)
	}
	return os.Remove(fullPath)
}

func (s *LocalStore) Path(key string) string {
	return s.safePath(key)
}

func ValidateExtension(filename string) bool {
	if filename == "" {
		return false
	}
	return strings.HasSuffix(strings.ToLower(filename), ".c")
}
