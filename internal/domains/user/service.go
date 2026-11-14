package user

import (
	"context"
	"database/sql"
	"gost/internal/database"
	"gost/internal/utils"
)

type UserServiceInterface interface {
	ListUsers(ctx context.Context, cursor string, pageSize int) (*utils.Page[*User], error)
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
