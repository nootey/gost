package file

import (
	"gost/internal/apperr"
	"time"
)

type File struct {
	ID          int64     `json:"id"`
	ParentID    *int64    `json:"parent_id"`
	IsFolder    bool      `json:"is_folder"`
	Name        string    `json:"name"`
	ContentType string    `json:"content_type"`
	SizeBytes   int64     `json:"size_bytes"`
	CreatedAt   time.Time `json:"created_at"`
}

// SortField is a column the list sorts by. The values match the list query.
type SortField string

const (
	SortByName SortField = "name"
	SortBySize SortField = "size"
	SortByType SortField = "type"
	SortByDate SortField = "date"
)

// ListQuery picks one folder's page. A ParentID of 0 means the root. An empty Search matches every name.
type ListQuery struct {
	ParentID int64
	Page     int
	PageSize int
	Search   string
	Sort     SortField
	Desc     bool
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

type PathEntry struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// FolderPath lists a folder and its parents, root first. The root itself is not in it.
type FolderPath struct {
	Path []*PathEntry `json:"path"`
}

type CreateFolderRequest struct {
	Name     string `json:"name"`
	ParentID *int64 `json:"parent_id"`
}

type FolderResponse struct {
	apperr.SuccessResponse
	Folder *File `json:"folder"`
}

type RenameRequest struct {
	Name string `json:"name"`
}

type RenameResponse struct {
	apperr.SuccessResponse
	File *File `json:"file"`
}
