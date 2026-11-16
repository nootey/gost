// Package uploads takes file uploads in chunks over the tus protocol (https://tus.io). Each request carries one
// chunk, so no proxy timeout cuts a large upload, and a broken upload continues from the offset the server has.
package uploads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gost/internal/apperr"
	"gost/internal/middleware"
	"gost/internal/utils"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tus/tusd/v2/pkg/filestore"
	tusd "github.com/tus/tusd/v2/pkg/handler"
	"github.com/tus/tusd/v2/pkg/memorylocker"
	"go.uber.org/zap"
	"golang.org/x/exp/slog"
)

// PartialDir is the folder in the upload dir for uploads still in progress. It is on the same disk as the store, so
// a finished upload moves into the store with a rename. The orphan sweep reads only the top level of the upload dir,
// so it never sees these files.
const PartialDir = ".partial"

// ownerKey is the metadata key for the user who made an upload. The server sets it, never the client.
const ownerKey = "user_id"

// infoSuffix ends the name of the file where tusd keeps an upload's info. The data file has the bare ID as its name.
const infoSuffix = ".info"

var (
	errUploadNotFound = apperr.New(apperr.KindNotFound, "upload not found")
	errNoLength       = apperr.New(apperr.KindInvalid, "the upload length is required")
)

// Hooks connect the uploads to the app. Meta is the metadata the client sent with the upload.
type Hooks struct {
	// Create checks a new upload before any bytes come in. It returns the metadata to keep.
	Create func(ctx context.Context, userID, size int64, meta map[string]string) (map[string]string, error)
	// Finish turns the bytes at path into a file. Its result goes to the client as JSON.
	Finish func(ctx context.Context, userID int64, meta map[string]string, path string) (any, error)
}

// Partial is an upload still in progress, as the upload sweep sees it.
type Partial struct {
	ID string
	// UserID is 0 when the info file is gone.
	UserID int64
	// Size is the bytes in so far, and Length the full size the client gave.
	Size   int64
	Length int64
	// ModTime is the last time bytes came in.
	ModTime time.Time
}

// Handler serves the uploads. It must run behind the auth middleware.
type Handler struct {
	next   http.Handler
	store  filestore.FileStore
	prefix string
	resp   *apperr.Responder
	logger *zap.Logger
}

// NewHandler serves the uploads under basePath, such as "/api/uploads/". Partial uploads go in PartialDir under dir.
func NewHandler(dir, basePath string, hooks Hooks, resp *apperr.Responder, logger *zap.Logger) (*Handler, error) {
	partialDir := filepath.Join(dir, PartialDir)
	if err := os.MkdirAll(partialDir, 0o750); err != nil {
		return nil, fmt.Errorf("failed to create partial upload dir: %w", err)
	}

	store := filestore.New(partialDir)
	store.DirModePerm = 0o750
	store.FileModePerm = 0o640
	composer := tusd.NewStoreComposer()
	store.UseIn(composer)
	// One API process takes the uploads, so the locks can live in memory and leave no lock files behind.
	memorylocker.New().UseIn(composer)

	h := &Handler{
		store:  store,
		prefix: strings.TrimSuffix(basePath, "/"),
		resp:   resp,
		logger: logger,
	}
	tus, err := tusd.NewHandler(tusd.Config{
		StoreComposer:        composer,
		BasePath:             basePath,
		DisableDownload:      true,
		DisableConcatenation: true,
		// The chi CORS middleware answers for the whole API.
		Cors:                      &tusd.CorsConfig{Disable: true},
		Logger:                    slog.New(zapHandler{logger: logger}),
		PreUploadCreateCallback:   h.create(hooks.Create),
		PreFinishResponseCallback: h.finish(hooks.Finish),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create upload handler: %w", err)
	}
	h.next = http.StripPrefix(h.prefix, tus)
	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	id := strings.Trim(strings.TrimPrefix(r.URL.Path, h.prefix), "/")
	if id == "" {
		w = &relativeLocation{ResponseWriter: w}
	} else if err := h.checkOwner(r.Context(), id, userID); err != nil {
		h.resp.Error(w, r, err)
		return
	}
	h.next.ServeHTTP(w, r)
}

// checkOwner answers 404 for an upload of another user, so an upload ID never shows that the upload exists.
// tusd takes the ID from the URL as it is, so the ID must also have the form the create hook gives it.
func (h *Handler) checkOwner(ctx context.Context, id string, userID int64) error {
	if !utils.IsStorageKey(id) {
		return errUploadNotFound
	}
	upload, err := h.store.GetUpload(ctx, id)
	if errors.Is(err, tusd.ErrNotFound) {
		return errUploadNotFound
	}
	if err != nil {
		return err
	}
	info, err := upload.GetInfo(ctx)
	if err != nil {
		return err
	}
	if info.MetaData[ownerKey] != strconv.FormatInt(userID, 10) {
		return errUploadNotFound
	}
	return nil
}

func (h *Handler) create(hook func(context.Context, int64, int64, map[string]string) (map[string]string, error)) func(tusd.HookEvent) (tusd.HTTPResponse, tusd.FileInfoChanges, error) {
	return func(event tusd.HookEvent) (tusd.HTTPResponse, tusd.FileInfoChanges, error) {
		userID, ok := middleware.UserIDFromContext(event.Context)
		if !ok {
			return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, h.tusError(apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		}
		// A deferred length would skip the checks on the size.
		if event.Upload.SizeIsDeferred {
			return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, h.tusError(errNoLength)
		}

		meta, err := hook(event.Context, userID, event.Upload.Size, event.Upload.MetaData)
		if err != nil {
			return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, h.tusError(err)
		}
		id, err := utils.NewStorageKey()
		if err != nil {
			return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, h.tusError(err)
		}

		kept := make(map[string]string, len(meta)+1)
		maps.Copy(kept, meta)
		kept[ownerKey] = strconv.FormatInt(userID, 10)
		return tusd.HTTPResponse{}, tusd.FileInfoChanges{ID: id, MetaData: kept}, nil
	}
}

// finish runs when the last byte is in. The partial files go in all cases: the hook moves the bytes on success, and
// after a failure the client cannot finish the upload again, because the server already has all of its bytes.
func (h *Handler) finish(hook func(context.Context, int64, map[string]string, string) (any, error)) func(tusd.HookEvent) (tusd.HTTPResponse, error) {
	return func(event tusd.HookEvent) (tusd.HTTPResponse, error) {
		upload := event.Upload
		defer h.removePartial(upload)

		userID, err := strconv.ParseInt(upload.MetaData[ownerKey], 10, 64)
		if err != nil {
			return tusd.HTTPResponse{}, h.tusError(fmt.Errorf("failed to read upload owner: %w", err))
		}
		result, err := hook(event.Context, userID, upload.MetaData, upload.Storage[filestore.StorageKeyPath])
		if err != nil {
			return tusd.HTTPResponse{}, h.tusError(err)
		}
		body, err := json.Marshal(result)
		if err != nil {
			return tusd.HTTPResponse{}, h.tusError(err)
		}
		return tusd.HTTPResponse{
			StatusCode: http.StatusOK,
			Body:       string(body),
			Header:     tusd.HTTPHeader{"Content-Type": "application/json"},
		}, nil
	}
}

func (h *Handler) removePartial(upload tusd.FileInfo) {
	for _, key := range []string{filestore.StorageKeyPath, filestore.StorageKeyInfoPath} {
		if err := os.Remove(upload.Storage[key]); err != nil && !errors.Is(err, os.ErrNotExist) {
			h.logger.Error("failed to remove partial upload", zap.String("id", upload.ID), zap.Error(err))
		}
	}
}

// tusError gives the JSON error answer of the rest of the API. A server error is logged, and its message stays generic.
func (h *Handler) tusError(err error) tusd.Error {
	status, message := apperr.Resolve(err)
	if status >= http.StatusInternalServerError {
		h.logger.Error("upload failed", zap.Error(err))
	}
	body, _ := json.Marshal(apperr.ErrorResponse{Title: "Error", Message: message})
	return tusd.Error{
		ErrorCode: "ERR_UPLOAD",
		Message:   message,
		HTTPResponse: tusd.HTTPResponse{
			StatusCode: status,
			Body:       string(body),
			Header:     tusd.HTTPHeader{"Content-Type": "application/json"},
		},
	}
}

// relativeLocation drops the scheme and host from the Location header. tusd takes them from the request, and a
// proxy that changes the host, such as the Vite proxy, would then send the chunks around the proxy.
type relativeLocation struct {
	http.ResponseWriter
}

func (w *relativeLocation) WriteHeader(status int) {
	if loc, err := url.Parse(w.Header().Get("Location")); err == nil && loc.Path != "" {
		w.Header().Set("Location", loc.Path)
	}
	w.ResponseWriter.WriteHeader(status)
}

// Unwrap lets tusd set the read deadlines through http.ResponseController.
func (w *relativeLocation) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// zapHandler sends the warnings and errors of tusd to zap, so they look like the other logs. tusd also logs every
// request at info level, and zapHandler drops those.
type zapHandler struct {
	logger *zap.Logger
	fields []zap.Field
}

func (h zapHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}

func (h zapHandler) Handle(_ context.Context, r slog.Record) error {
	fields := slices.Clone(h.fields)
	r.Attrs(func(a slog.Attr) bool {
		fields = append(fields, zap.Any(a.Key, a.Value.Any()))
		return true
	})
	if r.Level >= slog.LevelError {
		h.logger.Error(r.Message, fields...)
	} else {
		h.logger.Warn(r.Message, fields...)
	}
	return nil
}

func (h zapHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	fields := slices.Clone(h.fields)
	for _, a := range attrs {
		fields = append(fields, zap.Any(a.Key, a.Value.Any()))
	}
	return zapHandler{logger: h.logger, fields: fields}
}

// WithGroup keeps the fields flat. tusd uses no groups.
func (h zapHandler) WithGroup(string) slog.Handler {
	return h
}

// ListStale returns the uploads under dir that got no new bytes since cutoff. A half left of an upload, a data file
// with no info file or the other way round, counts too. It skips names it does not know, so a wrong dir loses nothing.
func ListStale(dir string, cutoff time.Time) ([]Partial, error) {
	root, err := os.OpenRoot(filepath.Join(dir, PartialDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to open partial upload dir: %w", err)
	}
	defer func() { _ = root.Close() }()

	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, fmt.Errorf("failed to read partial upload dir: %w", err)
	}

	type halves struct{ data, info fs.FileInfo }
	byID := map[string]*halves{}
	for _, entry := range entries {
		id, isInfo := strings.CutSuffix(entry.Name(), infoSuffix)
		if !entry.Type().IsRegular() || !utils.IsStorageKey(id) {
			continue
		}
		fi, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read partial upload: %w", err)
		}
		h := byID[id]
		if h == nil {
			h = &halves{}
			byID[id] = h
		}
		if isInfo {
			h.info = fi
		} else {
			h.data = fi
		}
	}

	var stale []Partial
	for id, h := range byID {
		p := Partial{ID: id}
		if h.data != nil {
			p.Size, p.ModTime = h.data.Size(), h.data.ModTime()
		} else {
			p.ModTime = h.info.ModTime()
		}
		if !p.ModTime.Before(cutoff) {
			continue
		}
		// A broken info file still goes. The sweep only loses the owner and the length for its log.
		if raw, err := root.ReadFile(id + infoSuffix); err == nil {
			var info tusd.FileInfo
			if json.Unmarshal(raw, &info) == nil {
				p.Length = info.Size
				p.UserID, _ = strconv.ParseInt(info.MetaData[ownerKey], 10, 64)
			}
		}
		stale = append(stale, p)
	}
	slices.SortFunc(stale, func(a, b Partial) int { return strings.Compare(a.ID, b.ID) })
	return stale, nil
}

// Remove removes both files of the upload id under dir. A missing file counts as removed, so the sweep can run twice.
func Remove(dir, id string) error {
	if !utils.IsStorageKey(id) {
		return fmt.Errorf("invalid upload id %q", id)
	}
	root, err := os.OpenRoot(filepath.Join(dir, PartialDir))
	if err != nil {
		return fmt.Errorf("failed to open partial upload dir: %w", err)
	}
	defer func() { _ = root.Close() }()

	for _, name := range []string{id, id + infoSuffix} {
		if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("failed to remove partial upload: %w", err)
		}
	}
	return nil
}
