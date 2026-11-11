package utils

import (
	"gost/internal/apperr"
	"net/http"
	"strconv"
	"strings"
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

func parseID(raw, key string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		return 0, apperr.New(apperr.KindInvalid, "invalid "+key)
	}
	return id, nil
}
