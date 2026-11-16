package user

import (
	"encoding/json"
	"gost/internal/apperr"
	"gost/internal/middleware"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

var errInvalidSettings = apperr.New(apperr.KindInvalid, "invalid settings")

type UserHandler struct {
	service UserServiceInterface
	resp    *apperr.Responder
}

func NewUserHandler(srv UserServiceInterface, resp *apperr.Responder) *UserHandler {
	return &UserHandler{
		service: srv,
		resp:    resp,
	}
}

func (h *UserHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.ListUsers)
	return r
}

func (h *UserHandler) ListUsers(w http.ResponseWriter, r *http.Request) {
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))

	result, err := h.service.ListUsers(r.Context(), r.URL.Query().Get("cursor"), pageSize)
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.JSON(w, http.StatusOK, result)
}

func (h *UserHandler) GetSettings(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	result, err := h.service.GetSettings(r.Context(), userID)
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.JSON(w, http.StatusOK, result)
}

// SaveSettings answers with the saved settings and no message: the page saves each switch as it changes.
func (h *UserHandler) SaveSettings(w http.ResponseWriter, r *http.Request) {
	userID, ok := middleware.GetUserID(r)
	if !ok {
		h.resp.Error(w, r, apperr.New(apperr.KindUnauthorized, "unauthenticated"))
		return
	}

	var req SettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		req.SortFavoritesFirst == nil || req.SortFoldersFirst == nil || req.ShowHidden == nil || req.ShowExtensions == nil {
		h.resp.Error(w, r, errInvalidSettings)
		return
	}

	result, err := h.service.SaveSettings(r.Context(), userID, Settings{
		SortFavoritesFirst: *req.SortFavoritesFirst,
		SortFoldersFirst:   *req.SortFoldersFirst,
		ShowHidden:         *req.ShowHidden,
		ShowExtensions:     *req.ShowExtensions,
	})
	if err != nil {
		h.resp.Error(w, r, err)
		return
	}

	h.resp.JSON(w, http.StatusOK, result)
}
