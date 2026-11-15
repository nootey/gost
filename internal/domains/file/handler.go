package file

import (
	"cmp"
	"context"
	"encoding/json"
	"gost/config"
	"gost/internal/apperr"
	"gost/internal/middleware"
	"gost/internal/utils"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"
)

const multipartOverhead = 1 << 20

var errInvalidBody = apperr.New(apperr.KindInvalid, "invalid request body")

type FileHandler struct {
	service  FileServiceInterface
	cfg      config.UploadConfig
	download config.DownloadConfig
	resp     *apperr.Responder
}

func NewFileHandler(srv FileServiceInterface, cfg config.UploadConfig, download config.DownloadConfig, resp *apperr.Responder) *FileHandler {
	return &FileHandler{
		service:  srv,
		cfg:      cfg,
		download: download,
		resp:     resp,
	}
}

func (h *FileHandler) List(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	parentID, err := utils.OptionalQueryID(r, "parent_id")
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	sort, err := utils.QueryChoice(r, "sort", SortByDate, SortByName, SortBySize, SortByType, SortByDate)
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	order, err := utils.QueryChoice(r, "order", "desc", "asc", "desc")
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	search := r.URL.Query().Get("search")
	if utf8.RuneCountInString(search) > utils.MaxNameLength {
		h.resp.Error(w, r, apperr.New(apperr.KindInvalid, "invalid search"))
		return
	}

	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))

	result, err := h.service.List(r.Context(), userID, ListQuery{
		ParentID: parentID,
		Cursor:   r.URL.Query().Get("cursor"),
		PageSize: pageSize,
		Search:   search,
		Sort:     sort,
		Desc:     order == "desc",
	})
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.JSON(w, http.StatusOK, result)
}

func (h *FileHandler) ResolveFolder(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	names, err := utils.QueryDir(r, "dir")
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	result, err := h.service.ResolveFolder(r.Context(), userID, names)
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.JSON(w, http.StatusOK, result)
}

func (h *FileHandler) Upload(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	rc := http.NewResponseController(w)
	deadline := time.Now().Add(time.Duration(h.cfg.TimeoutSeconds) * time.Second)
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline)

	r.Body = http.MaxBytesReader(w, r.Body, h.cfg.MaxSizeMB<<20+multipartOverhead)
	// Read what is left of the body, so an early error reaches the browser instead of a reset connection.
	defer func() { _, _ = io.Copy(io.Discard, r.Body) }()

	parentID, err := utils.OptionalQueryID(r, "parent_id")
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	part, err := utils.FormFilePart(r, "file")
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}
	defer func() { _ = part.Close() }()

	f, err := h.service.Upload(r.Context(), userID, parentID, part.FileName(), part)
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.JSON(w, http.StatusCreated, UploadResponse{
		SuccessResponse: apperr.SuccessResponse{Title: "Success", Message: f.Name + " uploaded."},
		File:            f,
	})
}

// Download sends one file.
func (h *FileHandler) Download(w http.ResponseWriter, r *http.Request) {
	h.serveContent(w, r, h.service.OpenFile)
}

// DownloadZip sends a ready zip.
func (h *FileHandler) DownloadZip(w http.ResponseWriter, r *http.Request) {
	h.serveContent(w, r, h.service.OpenZip)
}

// RequestZip answers once the download row and its job are in place. The client reads the downloads list until the zip
// is ready.
func (h *FileHandler) RequestZip(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	var req ItemsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.resp.Error(w, r, errInvalidBody)
		return
	}

	if err := utils.ValidateIDs(req.IDs, "ids"); err != nil {
		h.resp.Error(w, r, err)
		return
	}

	dl, err := h.service.RequestZip(r.Context(), userID, req.IDs)
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.JSON(w, http.StatusAccepted, DownloadResponse{
		SuccessResponse: apperr.SuccessResponse{Title: "Success", Message: "Preparing " + dl.Name + "."},
		Download:        dl,
	})
}

func (h *FileHandler) GetDownload(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	id, err := utils.URLParamID(r, "id")
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	dl, err := h.service.GetDownload(r.Context(), userID, id)
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.JSON(w, http.StatusOK, dl)
}

func (h *FileHandler) ListDownloads(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	list, err := h.service.ListDownloads(r.Context(), userID)
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.JSON(w, http.StatusOK, DownloadList{Downloads: list})
}

func (h *FileHandler) RemoveDownload(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	id, err := utils.URLParamID(r, "id")
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	if err := h.service.RemoveDownload(r.Context(), userID, id); err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.Success(w, http.StatusOK, "Removed.")
}

// serveContent sends the bytes open returns for the id in the URL. ServeContent answers range requests and the
// If-None-Match and If-Modified-Since checks. The bytes come as an attachment with nosniff, so a user's HTML file
// never runs as a page on this origin.
func (h *FileHandler) serveContent(
	w http.ResponseWriter,
	r *http.Request,
	open func(ctx context.Context, userID, id int64) (*FileContent, error),
) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	// The server write timeout fits small answers. A big file on a slow link needs much longer.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Duration(h.download.TimeoutSeconds) * time.Second))

	id, err := utils.URLParamID(r, "id")
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	content, err := open(r.Context(), userID, id)
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}
	defer func() { _ = content.Content.Close() }()

	header := w.Header()
	header.Set("Content-Type", cmp.Or(content.ContentType, "application/octet-stream"))
	header.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": content.Name}))
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("ETag", content.ETag)
	// The browser can keep the bytes, but asks first, so it never shows a deleted file from its cache.
	header.Set("Cache-Control", "private, no-cache")
	http.ServeContent(w, r, content.Name, content.ModTime, content.Content)
}

func (h *FileHandler) CreateFolder(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	var req CreateFolderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.resp.Error(w, r, errInvalidBody)
		return
	}

	parentID, err := utils.OptionalBodyID(req.ParentID, "parent_id")
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	f, err := h.service.CreateFolder(r.Context(), userID, parentID, req.Name)
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.JSON(w, http.StatusCreated, FolderResponse{
		SuccessResponse: apperr.SuccessResponse{Title: "Success", Message: f.Name + " created."},
		Folder:          f,
	})
}

func (h *FileHandler) Move(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	var req PasteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.resp.Error(w, r, errInvalidBody)
		return
	}

	if err := utils.ValidateIDs(req.IDs, "ids"); err != nil {
		h.resp.Error(w, r, err)
		return
	}

	parentID, err := utils.OptionalBodyID(req.ParentID, "parent_id")
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	if err := h.service.Move(r.Context(), userID, parentID, req.IDs); err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.Success(w, http.StatusOK, "Moved.")
}

// Copy answers once the copy roots are in place. A job copies the bytes and the rows under the folders.
func (h *FileHandler) Copy(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	var req PasteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.resp.Error(w, r, errInvalidBody)
		return
	}

	if err := utils.ValidateIDs(req.IDs, "ids"); err != nil {
		h.resp.Error(w, r, err)
		return
	}

	parentID, err := utils.OptionalBodyID(req.ParentID, "parent_id")
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	if err := h.service.Copy(r.Context(), userID, parentID, req.IDs); err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.Success(w, http.StatusOK, "Copying.")
}

func (h *FileHandler) AddFavorites(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	var req ItemsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.resp.Error(w, r, errInvalidBody)
		return
	}

	if err := utils.ValidateIDs(req.IDs, "ids"); err != nil {
		h.resp.Error(w, r, err)
		return
	}

	if err := h.service.AddFavorites(r.Context(), userID, req.IDs); err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.Success(w, http.StatusOK, "Added to favorites.")
}

func (h *FileHandler) RemoveFavorites(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	var req ItemsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.resp.Error(w, r, errInvalidBody)
		return
	}

	if err := utils.ValidateIDs(req.IDs, "ids"); err != nil {
		h.resp.Error(w, r, err)
		return
	}

	if err := h.service.RemoveFavorites(r.Context(), userID, req.IDs); err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.Success(w, http.StatusOK, "Removed from favorites.")
}

func (h *FileHandler) Trash(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	var req ItemsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.resp.Error(w, r, errInvalidBody)
		return
	}

	if err := utils.ValidateIDs(req.IDs, "ids"); err != nil {
		h.resp.Error(w, r, err)
		return
	}

	if err := h.service.Trash(r.Context(), userID, req.IDs); err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.Success(w, http.StatusOK, "Moved to the trash.")
}

func (h *FileHandler) Restore(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	var req ItemsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.resp.Error(w, r, errInvalidBody)
		return
	}

	if err := utils.ValidateIDs(req.IDs, "ids"); err != nil {
		h.resp.Error(w, r, err)
		return
	}

	if err := h.service.Restore(r.Context(), userID, req.IDs); err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.Success(w, http.StatusOK, "Restored from the trash.")
}

func (h *FileHandler) Purge(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	var req ItemsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.resp.Error(w, r, errInvalidBody)
		return
	}

	if err := utils.ValidateIDs(req.IDs, "ids"); err != nil {
		h.resp.Error(w, r, err)
		return
	}

	if err := h.service.Purge(r.Context(), userID, req.IDs); err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.Success(w, http.StatusOK, "Deleted forever.")
}

func (h *FileHandler) ListTrash(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	sort, err := utils.QueryChoice(r, "sort", SortByDate, SortByName, SortBySize, SortByType, SortByDate)
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	order, err := utils.QueryChoice(r, "order", "desc", "asc", "desc")
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))

	result, err := h.service.ListTrash(r.Context(), userID, TrashQuery{
		Cursor:   r.URL.Query().Get("cursor"),
		PageSize: pageSize,
		Sort:     sort,
		Desc:     order == "desc",
	})
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.JSON(w, http.StatusOK, result)
}

func (h *FileHandler) Rename(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	id, err := utils.URLParamID(r, "id")
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	var req RenameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.resp.Error(w, r, errInvalidBody)
		return
	}

	f, err := h.service.Rename(r.Context(), userID, id, req.Name)
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.JSON(w, http.StatusOK, RenameResponse{
		SuccessResponse: apperr.SuccessResponse{Title: "Success", Message: "Renamed to " + f.Name + "."},
		File:            f,
	})
}
