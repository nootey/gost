package utils

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
)

var ErrFileTooLarge = errors.New("file is too large")

// ErrNoStoredFile means the bytes under a key are not on disk.
var ErrNoStoredFile = errors.New("stored file not found")

// keySize is the count of random bytes in a storage key. The key is their hex, so it has twice as many characters.
const keySize = 16

// tempPrefix starts the name of a file while it is written. A random key follows it, so the orphan sweep knows the
// name, and two writes of one key never share a temp file.
const tempPrefix = ".upload-"

// StoredFile is a file in the upload dir with a name in key form.
type StoredFile struct {
	Name    string
	Size    int64
	ModTime time.Time
}

// NewStorageKey returns a new random key for bytes in the upload dir.
func NewStorageKey() (string, error) {
	keyBytes := make([]byte, keySize)
	if _, err := rand.Read(keyBytes); err != nil {
		return "", fmt.Errorf("failed to generate storage key: %w", err)
	}
	return hex.EncodeToString(keyBytes), nil
}

// StoreFile writes src into dir under a new random key and returns the key and size.
// A file over maxBytes, or one that fails mid-write, is never kept under a real key.
func StoreFile(dir string, src io.Reader, maxBytes int64) (string, int64, error) {
	key, err := NewStorageKey()
	if err != nil {
		return "", 0, err
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", 0, fmt.Errorf("failed to open upload dir: %w", err)
	}
	defer func() { _ = root.Close() }()

	size, err := writeStoredFile(root, key, src, maxBytes)
	if err != nil {
		return "", 0, err
	}
	return key, size, nil
}

// CopyStoredFile writes a copy of the bytes under srcKey to dstKey and returns its size. The copy is new bytes with a
// fresh modified time, never a link, so the orphan sweep sees it as new. A missing source gives ErrNoStoredFile.
// A copy already under dstKey is replaced, so a job that runs again, or two runs at once, can write it twice.
func CopyStoredFile(dir, srcKey, dstKey string) (int64, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, fmt.Errorf("failed to open upload dir: %w", err)
	}
	defer func() { _ = root.Close() }()

	src, err := root.Open(srcKey)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, ErrNoStoredFile
	}
	if err != nil {
		return 0, fmt.Errorf("failed to open stored file: %w", err)
	}
	defer func() { _ = src.Close() }()

	info, err := src.Stat()
	if err != nil {
		return 0, fmt.Errorf("failed to read stored file: %w", err)
	}
	return writeStoredFile(root, dstKey, src, info.Size())
}

// OpenStoredFile opens the bytes under key for reading. A missing file gives ErrNoStoredFile.
// The file stays open after the dir closes, so the caller closes only the file.
func OpenStoredFile(dir, key string) (*os.File, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to open upload dir: %w", err)
	}
	defer func() { _ = root.Close() }()

	f, err := root.Open(key)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoStoredFile
	}
	if err != nil {
		return nil, fmt.Errorf("failed to open stored file: %w", err)
	}
	return f, nil
}

// writeStoredFile writes src under key through a temp file, so a file over maxBytes, or one that fails mid-write,
// is never kept under a real key. Each write has its own temp file, and the rename replaces key in one step.
func writeStoredFile(root *os.Root, key string, src io.Reader, maxBytes int64) (int64, error) {
	tmpKey, err := NewStorageKey()
	if err != nil {
		return 0, err
	}
	tmpName := tempPrefix + tmpKey
	tmp, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return 0, fmt.Errorf("failed to create stored file: %w", err)
	}
	defer func() { _ = root.Remove(tmpName) }()

	size, copyErr := io.Copy(tmp, io.LimitReader(src, maxBytes+1))
	if err := errors.Join(copyErr, tmp.Close()); err != nil {
		return 0, fmt.Errorf("failed to write stored file: %w", err)
	}
	if size > maxBytes {
		return 0, ErrFileTooLarge
	}

	if err := root.Rename(tmpName, key); err != nil {
		return 0, fmt.Errorf("failed to store file: %w", err)
	}
	return size, nil
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

// ListStoredFiles calls fn with the files in dir last changed before cutoff, up to batchSize at a time.
// It skips all but regular files with a name in key form, so a wrong dir loses nothing.
func ListStoredFiles(dir string, cutoff time.Time, batchSize int, fn func([]StoredFile) error) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("failed to open upload dir: %w", err)
	}
	defer func() { _ = root.Close() }()

	d, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("failed to open upload dir: %w", err)
	}
	defer func() { _ = d.Close() }()

	for {
		entries, err := d.ReadDir(batchSize)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("failed to read upload dir: %w", err)
		}

		var files []StoredFile
		for _, entry := range entries {
			if !entry.Type().IsRegular() || !isStorageName(entry.Name()) {
				continue
			}
			info, err := entry.Info()
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("failed to read upload file: %w", err)
			}
			if info.ModTime().Before(cutoff) {
				files = append(files, StoredFile{Name: entry.Name(), Size: info.Size(), ModTime: info.ModTime()})
			}
		}
		if len(files) > 0 {
			if err := fn(files); err != nil {
				return err
			}
		}
	}
}

// isStorageName reports whether name is a storage key or the temp name of an upload: lowercase hex of keySize bytes.
func isStorageName(name string) bool {
	key := strings.TrimPrefix(name, tempPrefix)
	return len(key) == 2*keySize && strings.Trim(key, "0123456789abcdef") == ""
}
