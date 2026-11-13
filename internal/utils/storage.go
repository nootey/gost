package utils

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

var ErrFileTooLarge = errors.New("file is too large")

// StoreFile writes src into dir under a new random key and returns the key and size.
// A file over maxBytes, or one that fails mid-write, is never kept under a real key.
func StoreFile(dir string, src io.Reader, maxBytes int64) (string, int64, error) {
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		return "", 0, fmt.Errorf("failed to generate storage key: %w", err)
	}
	key := hex.EncodeToString(keyBytes)

	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", 0, fmt.Errorf("failed to open upload dir: %w", err)
	}
	defer func() { _ = root.Close() }()

	tmpName := ".upload-" + key
	tmp, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return "", 0, fmt.Errorf("failed to create upload file: %w", err)
	}
	defer func() { _ = root.Remove(tmpName) }()

	size, copyErr := io.Copy(tmp, io.LimitReader(src, maxBytes+1))
	if err := errors.Join(copyErr, tmp.Close()); err != nil {
		return "", 0, fmt.Errorf("failed to write upload: %w", err)
	}
	if size > maxBytes {
		return "", 0, ErrFileTooLarge
	}

	if err := root.Rename(tmpName, key); err != nil {
		return "", 0, fmt.Errorf("failed to store upload: %w", err)
	}

	return key, size, nil
}

// RemoveStoredFile removes the bytes under key. A missing file counts as removed, so a job that runs again can call it twice.
func RemoveStoredFile(dir, key string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	if err := root.Remove(key); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
