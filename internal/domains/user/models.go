package user

import (
	"gost/internal/utils"
	"time"
)

type User struct {
	ID             int64      `json:"id"`
	Email          string     `json:"email"`
	Password       string     `json:"-"`
	DisplayName    string     `json:"display_name"`
	EmailConfirmed *time.Time `json:"email_confirmed,omitempty"`
	RoleID         int64      `json:"role_id"`
	Role           *Role      `json:"role,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DeletedAt      *time.Time `json:"-"`
}

// Settings are the user's own choices for the file lists and how times show. The list routes take the order as
// params, so the client sends these along with each list request.
type Settings struct {
	// SortFavoritesFirst puts the favorites on top of the file list, before the folders-first split.
	SortFavoritesFirst bool `json:"sort_favorites_first"`
	SortFoldersFirst   bool `json:"sort_folders_first"`
	// ShowHidden is the default of the "Show hidden" switch above a list.
	ShowHidden bool `json:"show_hidden"`
	// ShowExtensions false shows file names without their last extension.
	ShowExtensions bool `json:"show_extensions"`
	// Use24HourClock false shows times on a 12-hour clock, such as 2:30 PM.
	Use24HourClock bool `json:"use_24_hour_clock"`
}

// defaultSettings are the settings of a user who never saved any. They match the lists before settings existed.
var defaultSettings = Settings{SortFoldersFirst: true, ShowExtensions: true, Use24HourClock: true}

// SettingsRequest sets every setting at once. A missing value is an error, not false.
type SettingsRequest struct {
	SortFavoritesFirst *bool `json:"sort_favorites_first"`
	SortFoldersFirst   *bool `json:"sort_folders_first"`
	ShowHidden         *bool `json:"show_hidden"`
	ShowExtensions     *bool `json:"show_extensions"`
	Use24HourClock     *bool `json:"use_24_hour_clock"`
}

type sortField string

const sortByDate sortField = "date"

// userList lists the users that are not deleted.
// Select * must match userFields.
var userList = utils.KeysetList[sortField]{
	Select: "*",
	From:   "users",
	Where:  "deleted_at IS NULL",
	Sorts: map[sortField][]string{
		sortByDate: {"created_at", "id"},
	},
}

type Role struct {
	ID          int64        `json:"id"`
	Name        string       `json:"name"`
	IsDefault   bool         `json:"is_default"`
	Description *string      `json:"description,omitempty"`
	Permissions []Permission `json:"permissions,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

type Permission struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}
