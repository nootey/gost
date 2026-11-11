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

type PaginatedFiles struct {
	Data       []*File `json:"data"`
	Page       int     `json:"page"`
	PageSize   int     `json:"page_size"`
	Total      int     `json:"total"`
	TotalPages int     `json:"total_pages"`
}

type UploadResponse struct {
	apperr.SuccessResponse
	File *File `json:"file"`
}
