package file

import (
	"encoding/json"
	"gost/internal/apperr"
	"gost/internal/utils"
	"io"
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
	UpdatedAt   time.Time `json:"updated_at"`
	// IsCopying is true until the copy job ends. Until then, nothing can go into the item, and it cannot be deleted or copied.
	IsCopying  bool `json:"is_copying"`
	IsFavorite bool `json:"is_favorite"`
	// IsHidden is true when the user hid the item. The list leaves it out unless it asks for hidden items.
	IsHidden bool `json:"is_hidden"`
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

// fileList lists one folder. The args are the user id, the folder id (0 for the root), the search, and whether to show
// hidden items. Select * must match fileFields.
var fileList = utils.KeysetList[SortField]{
	Select: "*",
	From:   "files",
	// trashed_at IS NULL must stay as it is, so the list indexes match. Hidden items are few, so the hidden check
	// filters rows the index found and needs no index of its own.
	Where: "user_id = $1 AND COALESCE(parent_id, 0) = $2::bigint AND trashed_at IS NULL AND strpos(lower(name), lower($3::text)) > 0" +
		" AND ($4::boolean OR hidden_at IS NULL)",
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
// ShowHidden adds the hidden items.
type ListQuery struct {
	ParentID   int64
	Cursor     string
	PageSize   int
	Search     string
	Sort       SortField
	Desc       bool
	ShowHidden bool
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

// FileContent is one file's bytes, open for a download. The caller closes Content.
// ETag is the storage key: the bytes under a key never change, because a copy writes new keys.
type FileContent struct {
	Name        string
	ContentType string
	ModTime     time.Time
	ETag        string
	Content     io.ReadSeekCloser
}

// DownloadState is where a zip download is: building, ready or failed.
type DownloadState string

const (
	DownloadBuilding DownloadState = "building"
	DownloadReady    DownloadState = "ready"
	DownloadFailed   DownloadState = "failed"
)

// Download is a zip of a folder or many items. Error says why a failed zip failed. ExpiresAt is when the sweep
// removes it. Downloaded is true once the user got the ready zip.
type Download struct {
	ID         int64         `json:"id"`
	Name       string        `json:"name"`
	State      DownloadState `json:"state"`
	Error      string        `json:"error,omitempty"`
	SizeBytes  int64         `json:"size_bytes"`
	CreatedAt  time.Time     `json:"created_at"`
	ExpiresAt  time.Time     `json:"expires_at"`
	Downloaded bool          `json:"downloaded"`
}

// zipFolder is a folder the zip job still has to read, with its path in the zip.
type zipFolder struct {
	id   int64
	path string
}

type DownloadResponse struct {
	apperr.SuccessResponse
	Download *Download `json:"download"`
}

// DownloadList is the user's newest downloads, newest first.
type DownloadList struct {
	Downloads []*Download `json:"downloads"`
}

// PasteRequest moves or copies the items into one folder. A missing or null ParentID means the root.
type PasteRequest struct {
	IDs      []int64 `json:"ids"`
	ParentID *int64  `json:"parent_id"`
}

// ActivityEvent is one kind of event in an item's audit log.
type ActivityEvent string

const (
	EventUploaded ActivityEvent = "uploaded"
	EventCreated  ActivityEvent = "created"
	EventCopied   ActivityEvent = "copied"
	EventRenamed  ActivityEvent = "renamed"
	EventMoved    ActivityEvent = "moved"
	EventTrashed  ActivityEvent = "trashed"
	EventRestored ActivityEvent = "restored"
	EventPurged   ActivityEvent = "purged"
	// EventExpired is a purge the trash sweep asked for after the retention days.
	EventExpired  ActivityEvent = "expired"
	EventHidden   ActivityEvent = "hidden"
	EventUnhidden ActivityEvent = "unhidden"
)

// hiddenEvents is the audit event of a hide (true) or an unhide (false).
var hiddenEvents = map[bool]ActivityEvent{true: EventHidden, false: EventUnhidden}

// activityList lists one record's audit log, newest first. The args are the user id and the item id.
var activityList = utils.KeysetList[SortField]{
	Select: "id, event, metadata, created_at",
	From:   "activity_logs",
	Where:  "user_id = $1 AND category = 'file' AND record_id = $2::bigint",
	Sorts:  map[SortField][]string{SortByDate: {"id"}},
}

// Activity is one event in an item's audit log. Metadata holds the item's name and its folder's name at the time
// (a null folder is Home), and the values of the event: "old_name" for a rename, "from" for a move.
type Activity struct {
	ID        int64           `json:"id"`
	Event     ActivityEvent   `json:"event"`
	Metadata  json.RawMessage `json:"metadata"`
	CreatedAt time.Time       `json:"created_at"`
}

// ActivityQuery picks one page of an item's audit log, newest first.
type ActivityQuery struct {
	Cursor   string
	PageSize int
}
