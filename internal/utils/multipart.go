package utils

import (
	"errors"
	"gost/internal/apperr"
	"io"
	"mime/multipart"
	"net/http"
)

// FormFilePart returns the first file sent under field as a stream. Unlike
// ParseMultipartForm it never buffers the body to memory or temp files.
func FormFilePart(r *http.Request, field string) (*multipart.Part, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, apperr.New(apperr.KindInvalid, "expected a multipart form")
	}

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, apperr.New(apperr.KindValidation, "no file was sent")
		}
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				return nil, apperr.New(apperr.KindTooLarge, "request body is too large")
			}
			return nil, apperr.Wrap(apperr.KindInvalid, "invalid multipart form", err)
		}

		if part.FormName() == field && part.FileName() != "" {
			return part, nil
		}
		_ = part.Close()
	}
}
