package file

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"gost/config"
	"gost/internal/apperr"
	"gost/internal/middleware"
	"gost/internal/uploads"
	"gost/internal/utils"
	"mime"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"
)

// The metadata keys of a chunked upload.
const (
	uploadNameKey   = "filename"
	uploadParentKey = "parent_id"
)

// maxTreeBodyBytes caps the body of a folder upload's tree: room for the most folders at a long path each.
const maxTreeBodyBytes = 16 << 20

var errInvalidBody = apperr.New(apperr.KindInvalid, "invalid request body")

type FileHandler struct {
	service  FileServiceInterface
	download config.DownloadConfig
	resp     *apperr.Responder
}

func NewFileHandler(srv FileServiceInterface, download config.DownloadConfig, resp *apperr.Responder) *FileHandler {
	return &FileHandler{
		service:  srv,
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

	sort, err := utils.QueryChoice(r, "sort", SortByDate, SortByName, SortBySize, SortByType, SortByDate, SortByModified)
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

	showHidden, err := utils.QueryChoice(r, "show_hidden", "false", "true", "false")
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))

	result, err := h.service.List(r.Context(), userID, ListQuery{
		ParentID:   parentID,
		Cursor:     r.URL.Query().Get("cursor"),
		PageSize:   pageSize,
		Search:     search,
		Sort:       sort,
		Desc:       order == "desc",
		ShowHidden: showHidden == "true",
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

// UploadHooks connect the chunked uploads to the service. The client sends the file name and the folder as metadata.
func (h *FileHandler) UploadHooks() uploads.Hooks {
	return uploads.Hooks{Create: h.checkUpload, Finish: h.finishUpload}
}

func (h *FileHandler) checkUpload(ctx context.Context, userID, size int64, meta map[string]string) (map[string]string, error) {
	parentID, err := utils.OptionalID(meta[uploadParentKey], "parent_id")
	if err != nil {
		return nil, err
	}
	if err := h.service.CheckUpload(ctx, userID, parentID, meta[uploadNameKey], size); err != nil {
		return nil, err
	}
	return map[string]string{uploadNameKey: meta[uploadNameKey], uploadParentKey: meta[uploadParentKey]}, nil
}

func (h *FileHandler) finishUpload(ctx context.Context, userID int64, meta map[string]string, path string) (any, error) {
	parentID, err := utils.OptionalID(meta[uploadParentKey], "parent_id")
	if err != nil {
		return nil, err
	}
	f, err := h.service.FinishUpload(ctx, userID, parentID, meta[uploadNameKey], path)
	if err != nil {
		return nil, err
	}
	return UploadResponse{
		SuccessResponse: apperr.SuccessResponse{Title: "Success", Message: f.Name + " uploaded."},
		File:            f,
	}, nil
}

// Download sends one file to save.
func (h *FileHandler) Download(w http.ResponseWriter, r *http.Request) {
	h.serveContent(w, r, h.service.OpenFile, false)
}

// Content sends one file for the browser to show, such as a PDF in a frame. A type that is not safe to show saves.
func (h *FileHandler) Content(w http.ResponseWriter, r *http.Request) {
	h.serveContent(w, r, h.service.OpenFile, true)
}

// Thumbnail sends the small copy of a picture that the file list shows.
func (h *FileHandler) Thumbnail(w http.ResponseWriter, r *http.Request) {
	h.serveContent(w, r, h.service.OpenThumbnail, false)
}

// DownloadZip sends a ready zip.
func (h *FileHandler) DownloadZip(w http.ResponseWriter, r *http.Request) {
	h.serveContent(w, r, h.service.OpenZip, false)
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

// ListRecents sends the user's newest added or changed files, newest first.
func (h *FileHandler) ListRecents(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	list, err := h.service.ListRecents(r.Context(), userID)
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.JSON(w, http.StatusOK, RecentList{Files: list})
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
// never runs as a page on this origin. With inline, a type on the allow list shows in the browser. The sandbox policy
// blocks scripts in any file the browser shows, also in a PDF.
func (h *FileHandler) serveContent(
	w http.ResponseWriter,
	r *http.Request,
	open func(ctx context.Context, userID, id int64) (*FileContent, error),
	inline bool,
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

	disposition := "attachment"
	if inline && utils.ShowsInline(content.ContentType) {
		disposition = "inline"
	}

	header := w.Header()
	header.Set("Content-Type", cmp.Or(content.ContentType, "application/octet-stream"))
	header.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": content.Name}))
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "sandbox")
	header.Set("ETag", content.ETag)
	// The browser can keep the bytes, but asks first, so it never shows a deleted file from its cache.
	header.Set("Cache-Control", "private, no-cache")
	http.ServeContent(w, r, content.Name, content.ModTime, content.Content)
}

func (h *FileHandler) CreateFile(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	var req CreateFileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.resp.Error(w, r, errInvalidBody)
		return
	}

	parentID, err := utils.OptionalBodyID(req.ParentID, "parent_id")
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	f, err := h.service.CreateFile(r.Context(), userID, parentID, req.Name)
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.JSON(w, http.StatusCreated, FileResponse{
		SuccessResponse: apperr.SuccessResponse{Title: "Success", Message: f.Name + " created."},
		File:            f,
	})
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

// CreateFolderTree makes the folders of a folder upload, so its files can go up into them.
func (h *FileHandler) CreateFolderTree(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxTreeBodyBytes)
	var req CreateFolderTreeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.resp.Error(w, r, errInvalidBody)
		return
	}

	parentID, err := utils.OptionalBodyID(req.ParentID, "parent_id")
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	folders, err := h.service.CreateFolderTree(r.Context(), userID, parentID, req.Paths)
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	message := fmt.Sprintf("%d folders created.", len(folders))
	if len(folders) == 1 {
		message = "1 folder created."
	}
	h.resp.JSON(w, http.StatusCreated, FolderTreeResponse{
		SuccessResponse: apperr.SuccessResponse{Title: "Success", Message: message},
		Folders:         folders,
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

// ListFavorites sends one page of the user's favorites from all folders, by name with folders first unless the query
// asks for another sort.
func (h *FileHandler) ListFavorites(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	sort, err := utils.QueryChoice(r, "sort", SortByName, SortByName, SortBySize, SortByType, SortByDate)
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	order, err := utils.QueryChoice(r, "order", "asc", "asc", "desc")
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	showHidden, err := utils.QueryChoice(r, "show_hidden", "false", "true", "false")
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))

	result, err := h.service.ListFavorites(r.Context(), userID, FavoriteQuery{
		Cursor:     r.URL.Query().Get("cursor"),
		PageSize:   pageSize,
		Sort:       sort,
		Desc:       order == "desc",
		ShowHidden: showHidden == "true",
	})
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.JSON(w, http.StatusOK, result)
}

func (h *FileHandler) Hide(w http.ResponseWriter, r *http.Request) {
	h.setHidden(w, r, true, "Hidden.")
}

func (h *FileHandler) Unhide(w http.ResponseWriter, r *http.Request) {
	h.setHidden(w, r, false, "Unhidden.")
}

func (h *FileHandler) setHidden(w http.ResponseWriter, r *http.Request, hidden bool, message string) {
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

	if err := h.service.SetHidden(r.Context(), userID, req.IDs, hidden); err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.Success(w, http.StatusOK, message)
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

// GetItem sends one live item, for its details.
func (h *FileHandler) GetItem(w http.ResponseWriter, r *http.Request) {
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

	f, err := h.service.GetItem(r.Context(), userID, id)
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.JSON(w, http.StatusOK, f)
}

// ListActivity sends one page of an item's audit log, newest first.
func (h *FileHandler) ListActivity(w http.ResponseWriter, r *http.Request) {
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

	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))

	result, err := h.service.ListActivity(r.Context(), userID, id, ActivityQuery{
		Cursor:   r.URL.Query().Get("cursor"),
		PageSize: pageSize,
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
