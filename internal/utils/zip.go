package utils

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"
)

// ZipDir holds the built zips, under the upload dir. The orphan sweep reads only regular files at the top of the
// upload dir, so it never sees them.
const ZipDir = "zips"

// zipCopyBufferSize is the chunk a zip reads and writes at a time. Big chunks keep the disk busy with few calls.
const zipCopyBufferSize = 1 << 20

// ErrZipTooLarge means the next file would take a zip past its cap.
var ErrZipTooLarge = errors.New("zip is too large")

// ZipBuilder writes a zip into ZipDir through a temp file, so a zip that fails mid-write is never kept under a real
// key. Entries use Store, not Deflate: media does not compress, and Store keeps the CPU free.
type ZipBuilder struct {
	uploads  *os.Root
	zips     *os.Root
	tmpName  string
	tmp      *os.File
	zw       *zip.Writer
	buf      []byte
	maxBytes int64
	size     int64
	done     bool

	check      func() error
	checkEvery time.Duration
	checkedAt  time.Time
}

// NewZipBuilder starts a zip of the files in the upload dir. maxBytes caps the bytes of the files in it.
// The caller defers Abort, which removes the temp file unless Finish moved it to its key.
func NewZipBuilder(dir string, maxBytes int64) (*ZipBuilder, error) {
	uploads, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to open upload dir: %w", err)
	}
	if err := uploads.Mkdir(ZipDir, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
		_ = uploads.Close()
		return nil, fmt.Errorf("failed to create zip dir: %w", err)
	}
	zips, err := uploads.OpenRoot(ZipDir)
	if err != nil {
		_ = uploads.Close()
		return nil, fmt.Errorf("failed to open zip dir: %w", err)
	}

	b := &ZipBuilder{uploads: uploads, zips: zips, buf: make([]byte, zipCopyBufferSize), maxBytes: maxBytes}
	tmpKey, err := NewStorageKey()
	if err == nil {
		b.tmpName = tempPrefix + tmpKey
		b.tmp, err = zips.OpenFile(b.tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	}
	if err != nil {
		b.closeRoots()
		return nil, fmt.Errorf("failed to create zip: %w", err)
	}
	b.zw = zip.NewWriter(b.tmp)
	return b, nil
}

// CheckEvery makes the builder run check before each entry and while it copies a file, at most once per interval.
// An error from check stops the zip with that error. The first check comes one interval after this call.
func (b *ZipBuilder) CheckEvery(interval time.Duration, check func() error) {
	b.check, b.checkEvery, b.checkedAt = check, interval, time.Now()
}

func (b *ZipBuilder) runCheck() error {
	if b.check == nil || time.Since(b.checkedAt) < b.checkEvery {
		return nil
	}
	b.checkedAt = time.Now()
	return b.check()
}

// AddFolder adds an entry for a folder, so an empty folder stays in the zip. path has no trailing slash.
func (b *ZipBuilder) AddFolder(path string, modTime time.Time) error {
	if err := b.runCheck(); err != nil {
		return err
	}
	_, err := b.zw.CreateHeader(&zip.FileHeader{Name: path + "/", Method: zip.Store, Modified: modTime})
	return err
}

// AddStoredFile adds the bytes under key as path and returns their size. A missing file gives ErrNoStoredFile.
// A file that would take the zip past its cap gives ErrZipTooLarge before any of its bytes go in.
// The copy stops when ctx ends, so a big file does not hold up a stop of the app.
func (b *ZipBuilder) AddStoredFile(ctx context.Context, path, key string, modTime time.Time) (int64, error) {
	if err := b.runCheck(); err != nil {
		return 0, err
	}
	src, err := b.uploads.Open(key)
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
	if b.size+info.Size() > b.maxBytes {
		return 0, ErrZipTooLarge
	}

	w, err := b.zw.CreateHeader(&zip.FileHeader{Name: path, Method: zip.Store, Modified: modTime})
	if err != nil {
		return 0, err
	}
	n, err := io.CopyBuffer(w, zipReader{ctx: ctx, b: b, r: src}, b.buf)
	b.size += n
	if err != nil {
		return n, fmt.Errorf("failed to write zip: %w", err)
	}
	return n, nil
}

// Finish closes the zip and moves it to a new key in ZipDir. It returns the key and the size of the zip on disk.
func (b *ZipBuilder) Finish() (string, int64, error) {
	if err := b.zw.Close(); err != nil {
		return "", 0, fmt.Errorf("failed to write zip: %w", err)
	}
	info, err := b.tmp.Stat()
	if err != nil {
		return "", 0, fmt.Errorf("failed to read zip: %w", err)
	}
	if err := b.tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("failed to write zip: %w", err)
	}
	key, err := NewStorageKey()
	if err != nil {
		return "", 0, err
	}
	if err := b.zips.Rename(b.tmpName, key); err != nil {
		return "", 0, fmt.Errorf("failed to store zip: %w", err)
	}

	b.done = true
	b.closeRoots()
	return key, info.Size(), nil
}

// Abort removes the temp file of a zip that did not finish. After Finish it does nothing, so a defer can call it.
func (b *ZipBuilder) Abort() {
	if b.done {
		return
	}
	b.done = true
	_ = b.tmp.Close()
	_ = b.zips.Remove(b.tmpName)
	b.closeRoots()
}

func (b *ZipBuilder) closeRoots() {
	_ = b.zips.Close()
	_ = b.uploads.Close()
}

// zipReader fails a read once ctx ends or the builder's check fails.
type zipReader struct {
	ctx context.Context
	b   *ZipBuilder
	r   io.Reader
}

func (z zipReader) Read(p []byte) (int, error) {
	if err := z.ctx.Err(); err != nil {
		return 0, err
	}
	if err := z.b.runCheck(); err != nil {
		return 0, err
	}
	return z.r.Read(p)
}
