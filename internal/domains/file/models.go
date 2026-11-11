package file

import (
	"gost/internal/apperr"
	"time"
)

type File struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	ContentType string    `json:"content_type"`
	SizeBytes   int64     `json:"size_bytes"`
	CreatedAt   time.Time `json:"created_at"`
}

type UploadResponse struct {
	apperr.SuccessResponse
	File *File `json:"file"`
}
