package utils

import (
	"gost/internal/apperr"
	"net/http"
	"slices"
	"strconv"
	"strings"

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

// QueryDir reads a folder path such as "/Photos/Cars" from the query string as its names. The root has none.
func QueryDir(r *http.Request, key string) ([]string, error) {
	dir := strings.Trim(r.URL.Query().Get(key), "/")
	if dir == "" {
		return nil, nil
	}

	names := strings.Split(dir, "/")
	for _, name := range names {
		if ValidateName(name, FolderName) != nil {
			return nil, apperr.New(apperr.KindInvalid, "invalid "+key)
		}
	}
	return names, nil
}

// QueryChoice reads a query value that must be one of choices. A missing value is def.
func QueryChoice[T ~string](r *http.Request, key string, def T, choices ...T) (T, error) {
	raw := T(r.URL.Query().Get(key))
	if raw == "" {
		return def, nil
	}
	if !slices.Contains(choices, raw) {
		return "", apperr.New(apperr.KindInvalid, "invalid "+key)
	}
	return raw, nil
}

// ValidateIDs checks a list of ids from a JSON body. A selection is one page, so it holds at most maxPageSize ids.
func ValidateIDs(ids []int64, key string) error {
	if len(ids) == 0 || len(ids) > maxPageSize || slices.ContainsFunc(ids, func(id int64) bool { return id < 1 }) {
		return apperr.New(apperr.KindInvalid, "invalid "+key)
	}
	return nil
}

func parseID(raw, key string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		return 0, apperr.New(apperr.KindInvalid, "invalid "+key)
	}
	return id, nil
}
