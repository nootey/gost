package user

import (
	"context"
	"database/sql"
	"errors"
	"gost/internal/database"
	"gost/internal/utils"
)

type UserServiceInterface interface {
	ListUsers(ctx context.Context, cursor string, pageSize int) (*utils.Page[*User], error)
	GetSettings(ctx context.Context, userID int64) (*Settings, error)
	SaveSettings(ctx context.Context, userID int64, settings Settings) (*Settings, error)
}

var _ UserServiceInterface = (*UserService)(nil)

type UserService struct {
	queries *database.Queries
	db      *sql.DB
}

func NewUserService(queries *database.Queries, db *sql.DB) *UserService {
	return &UserService{
		queries: queries,
		db:      db,
	}
}

func toUserModel(dbUser database.User) *User {
	user := &User{
		ID:          dbUser.ID,
		Email:       dbUser.Email,
		DisplayName: dbUser.DisplayName,
		RoleID:      dbUser.RoleID,
		CreatedAt:   dbUser.CreatedAt,
		UpdatedAt:   dbUser.UpdatedAt,
	}

	if dbUser.EmailConfirmed.Valid {
		user.EmailConfirmed = &dbUser.EmailConfirmed.Time
	}
	if dbUser.DeletedAt.Valid {
		user.DeletedAt = &dbUser.DeletedAt.Time
	}

	return user
}

// userFields follows the column order of the users table, which SELECT * returns.
func userFields(u *database.User) []any {
	return []any{&u.ID, &u.Email, &u.Password, &u.DisplayName, &u.EmailConfirmed, &u.RoleID, &u.CreatedAt, &u.UpdatedAt, &u.DeletedAt}
}

func (s *UserService) ListUsers(ctx context.Context, cursor string, pageSize int) (*utils.Page[*User], error) {
	page, err := utils.FetchPage(ctx, s.db, userList, utils.PageQuery[sortField]{
		Sort:     sortByDate,
		Desc:     true,
		Cursor:   cursor,
		PageSize: pageSize,
	}, userFields)
	if err != nil {
		return nil, err
	}

	users := make([]*User, len(page.Data))
	for i, dbUser := range page.Data {
		users[i] = toUserModel(dbUser)
	}

	return &utils.Page[*User]{Data: users, NextCursor: page.NextCursor}, nil
}

// GetSettings reads the user's settings. A user who never saved any gets the defaults.
func (s *UserService) GetSettings(ctx context.Context, userID int64) (*Settings, error) {
	row, err := s.queries.GetUserSettings(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		settings := defaultSettings
		return &settings, nil
	}
	if err != nil {
		return nil, err
	}
	return toSettingsModel(row), nil
}

// SaveSettings stores every setting of the user at once. The first save makes the user's row.
func (s *UserService) SaveSettings(ctx context.Context, userID int64, settings Settings) (*Settings, error) {
	row, err := s.queries.SaveUserSettings(ctx, database.SaveUserSettingsParams{
		UserID:             userID,
		SortFavoritesFirst: settings.SortFavoritesFirst,
		SortFoldersFirst:   settings.SortFoldersFirst,
		ShowHidden:         settings.ShowHidden,
		ShowExtensions:     settings.ShowExtensions,
		Use24HourClock:     settings.Use24HourClock,
	})
	if err != nil {
		return nil, err
	}
	return toSettingsModel(row), nil
}

func toSettingsModel(row database.UserSetting) *Settings {
	return &Settings{
		SortFavoritesFirst: row.SortFavoritesFirst,
		SortFoldersFirst:   row.SortFoldersFirst,
		ShowHidden:         row.ShowHidden,
		ShowExtensions:     row.ShowExtensions,
		Use24HourClock:     row.Use24HourClock,
	}
}
