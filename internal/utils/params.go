package utils

import (
	"gost/internal/apperr"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// OptionalQueryID reads a positive id from the query string. A missing value is 0.
func OptionalQueryID(r *http.Request, key string) (int64, error) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return 0, nil
	}
	return parseID(raw, key)
}

// OptionalBodyID checks an optional positive id from a JSON body. A missing value is 0.
func OptionalBodyID(id *int64, key string) (int64, error) {
	if id == nil {
		return 0, nil
	}
	if *id < 1 {
		return 0, apperr.New(apperr.KindInvalid, "invalid "+key)
	}
	return *id, nil
}

// URLParamID reads a positive id from a chi route parameter such as {id}.
func URLParamID(r *http.Request, key string) (int64, error) {
	return parseID(chi.URLParam(r, key), key)
}

func parseID(raw, key string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		return 0, apperr.New(apperr.KindInvalid, "invalid "+key)
	}
	return id, nil
}
