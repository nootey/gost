package file

import (
	"gost/internal/apperr"
	"gost/internal/utils"
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

// foldersFirst splits a list into folders, then files. This is the only place that applies that rule.
// Postgres turns "(storage_key IS NULL) = false" into "storage_key IS NOT NULL", and the index does not match that. IS FALSE stays as it is.
var foldersFirst = []string{"(storage_key IS NULL) IS TRUE", "(storage_key IS NULL) IS FALSE"}

// fileList lists one folder. The args are the user id, the folder id (0 for the root) and the search.
// Select * must match fileFields.
var fileList = utils.KeysetList[SortField]{
	Select: "*",
	From:   "files",
	// trashed_at IS NULL must stay as it is, so the list indexes match.
	Where: "user_id = $1 AND COALESCE(parent_id, 0) = $2::bigint AND trashed_at IS NULL AND strpos(lower(name), lower($3::text)) > 0",
	Sorts: map[SortField][]string{
		SortByName: {"lower(name)", "id"},
		SortBySize: {"size_bytes", "lower(name)", "id"},
		SortByType: {"content_type", "lower(name)", "id"},
		SortByDate: {"created_at", "id"},
	},
	Parts: foldersFirst,
}

// trashList lists the items the user deleted, not the rows under them. An item marked for a permanent delete is gone
// for the user, so it is left out. The arg is the user id. Select * must match fileFields.
var trashList = utils.KeysetList[SortField]{
	Select: "*",
	From:   "files",
	// trash_root_id = id must stay as it is, so the trash index matches.
	Where: "user_id = $1 AND trash_root_id = id AND purge_requested_at IS NULL",
	Sorts: map[SortField][]string{
		SortByName: {"lower(name)", "id"},
		SortBySize: {"size_bytes", "lower(name)", "id"},
		SortByType: {"content_type", "lower(name)", "id"},
		SortByDate: {"trashed_at", "id"},
	},
	Parts: foldersFirst,
}

// ListQuery picks one page of a folder. A ParentID of 0 means the root. An empty Search matches every name.
type ListQuery struct {
	ParentID int64
	Cursor   string
	PageSize int
	Search   string
	Sort     SortField
	Desc     bool
}

// TrashQuery picks one page of the trash. The date sort is the time of the delete.
type TrashQuery struct {
	Cursor   string
	PageSize int
	Sort     SortField
	Desc     bool
}

// TrashedFile is an item the user deleted. ParentID is the folder it was in.
type TrashedFile struct {
	File
	TrashedAt time.Time `json:"trashed_at"`
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

type ItemsRequest struct {
	IDs []int64 `json:"ids"`
}
