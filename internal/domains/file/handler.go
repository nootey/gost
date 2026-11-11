package file

import (
	"gost/config"
	"gost/internal/apperr"
	"gost/internal/middleware"
	"gost/internal/utils"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

const multipartOverhead = 1 << 20

type FileHandler struct {
	service FileServiceInterface
	cfg     config.UploadConfig
	resp    *apperr.Responder
}

func NewFileHandler(srv FileServiceInterface, cfg config.UploadConfig, resp *apperr.Responder) *FileHandler {
	return &FileHandler{
		service: srv,
		cfg:     cfg,
		resp:    resp,
	}
}

func (h *FileHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.Upload)
	return r
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

	part, err := utils.FormFilePart(r, "file")
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}
	defer func() { _ = part.Close() }()

	f, err := h.service.Upload(r.Context(), userID, part.FileName(), part)
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.JSON(w, http.StatusCreated, UploadResponse{
		SuccessResponse: apperr.SuccessResponse{Title: "Success", Message: f.Name + " uploaded."},
		File:            f,
	})
}
