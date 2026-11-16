//go:build integration

package user_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"gost/internal/apperr"
	"gost/internal/database"
	"gost/internal/domains/user"
	"gost/tests/integration"
	"net/http"
	"testing"

	"github.com/stretchr/testify/suite"
)

type UserServiceTestSuite struct {
	integration.ServiceIntegrationSuite
}

func TestIntegrationUserServiceTestSuite(t *testing.T) {
	suite.Run(t, new(UserServiceTestSuite))
}

func (s *UserServiceTestSuite) TestListUsers() {
	svc := s.TC.Services.UserService

	s.seedUser("test1@example.com", "Test User 1")
	s.seedUser("test2@example.com", "Test User 2")
	deleted := s.seedUser("test3@example.com", "Test User 3")
	_, err := s.TC.DB.ExecContext(context.Background(), `UPDATE users SET deleted_at = NOW() WHERE id = $1`, deleted.ID)
	s.Require().NoError(err)

	result, err := svc.ListUsers(context.Background(), "", 10)

	s.Require().NoError(err)
	s.ElementsMatch([]string{"test1@example.com", "test2@example.com"}, userEmails(result.Data))
	s.Nil(result.NextCursor)
}

// Newest first. Users 2 to 5 tie on created_at, so a cursor inside the tie must keep the id too.
func (s *UserServiceTestSuite) TestListUsers_Pages() {
	for i := 1; i <= 5; i++ {
		s.seedUser(fmt.Sprintf("user%d@example.com", i), fmt.Sprintf("User %d", i))
	}
	_, err := s.TC.DB.ExecContext(context.Background(), `
		UPDATE users SET created_at = CASE WHEN email = 'user1@example.com'
			THEN TIMESTAMP '2026-03-01 10:00:00' ELSE TIMESTAMP '2026-01-01 10:00:00' END`)
	s.Require().NoError(err)

	pages := s.walkPages(2)

	s.Equal([][]string{
		{"user1@example.com", "user5@example.com"},
		{"user4@example.com", "user3@example.com"},
		{"user2@example.com"},
	}, pages)
}

func (s *UserServiceTestSuite) TestListUsers_BadCursor() {
	svc := s.TC.Services.UserService
	s.seedUser("test1@example.com", "Test User 1")

	cases := []struct {
		label  string
		cursor string
	}{
		{"not base64", "!!!"},
		{"not json", cursorOf("nope")},
		{"another sort", cursorOf(`{"s":"name","d":true,"v":["2026-01-01 10:00:00","1"]}`)},
		{"another order", cursorOf(`{"s":"date","v":["2026-01-01 10:00:00","1"]}`)},
		{"too few values", cursorOf(`{"s":"date","d":true,"v":["2026-01-01 10:00:00"]}`)},
		{"unknown part", cursorOf(`{"s":"date","d":true,"p":1,"v":["2026-01-01 10:00:00","1"]}`)},
		{"date not a time", cursorOf(`{"s":"date","d":true,"v":["not a time","1"]}`)},
		{"id not a number", cursorOf(`{"s":"date","d":true,"v":["2026-01-01 10:00:00","abc"]}`)},
	}
	for _, tc := range cases {
		_, err := svc.ListUsers(context.Background(), tc.cursor, 10)

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusBadRequest, status, tc.label)
		s.Equal("invalid cursor", msg, tc.label)
	}
}

// The list scans rows by hand, so a field it misses would only show here.
func (s *UserServiceTestSuite) TestListUsers_ReturnsEveryField() {
	created := s.seedUser("test1@example.com", "Test User 1")
	s.Require().NoError(s.TC.Queries.ConfirmUserEmail(context.Background(), created.ID))
	dbUser, err := s.TC.Queries.GetUserByID(context.Background(), created.ID)
	s.Require().NoError(err)
	s.Require().True(dbUser.EmailConfirmed.Valid)

	result, err := s.TC.Services.UserService.ListUsers(context.Background(), "", 10)

	s.Require().NoError(err)
	s.Equal([]*user.User{{
		ID:             dbUser.ID,
		Email:          dbUser.Email,
		DisplayName:    dbUser.DisplayName,
		EmailConfirmed: &dbUser.EmailConfirmed.Time,
		RoleID:         dbUser.RoleID,
		CreatedAt:      dbUser.CreatedAt,
		UpdatedAt:      dbUser.UpdatedAt,
	}}, result.Data)
}

// A user who never saved settings gets the defaults: the lists as they were before settings existed.
func (s *UserServiceTestSuite) TestGetSettings_Defaults() {
	u := s.seedUser("test1@example.com", "Test User 1")

	settings, err := s.TC.Services.UserService.GetSettings(context.Background(), u.ID)

	s.Require().NoError(err)
	s.Equal(&user.Settings{SortFoldersFirst: true, ShowExtensions: true, Use24HourClock: true}, settings)
}

// The first save makes the row, a later save changes it, and each user keeps their own settings.
func (s *UserServiceTestSuite) TestSaveSettings() {
	svc := s.TC.Services.UserService
	ctx := context.Background()
	u := s.seedUser("test1@example.com", "Test User 1")
	other := s.seedUser("test2@example.com", "Test User 2")

	first := user.Settings{
		SortFavoritesFirst: true, SortFoldersFirst: false, ShowHidden: true, ShowExtensions: false, Use24HourClock: false,
	}
	saved, err := svc.SaveSettings(ctx, u.ID, first)
	s.Require().NoError(err)
	s.Equal(&first, saved)

	second := user.Settings{
		SortFavoritesFirst: false, SortFoldersFirst: true, ShowHidden: true, ShowExtensions: true, Use24HourClock: true,
	}
	saved, err = svc.SaveSettings(ctx, u.ID, second)
	s.Require().NoError(err)
	s.Equal(&second, saved)

	got, err := svc.GetSettings(ctx, u.ID)
	s.Require().NoError(err)
	s.Equal(&second, got)

	got, err = svc.GetSettings(ctx, other.ID)
	s.Require().NoError(err)
	s.Equal(&user.Settings{SortFoldersFirst: true, ShowExtensions: true, Use24HourClock: true}, got)

	var rows int
	s.Require().NoError(s.TC.DB.QueryRowContext(ctx, `SELECT count(*) FROM user_settings`).Scan(&rows))
	s.Equal(1, rows)
}

func (s *UserServiceTestSuite) seedUser(email, displayName string) database.User {
	role, err := s.TC.Queries.GetRoleByName(context.Background(), "user")
	s.Require().NoError(err)

	u, err := s.TC.Queries.CreateUser(context.Background(), database.CreateUserParams{
		Email:       email,
		Password:    "hashed",
		DisplayName: displayName,
		RoleID:      role.ID,
	})
	s.Require().NoError(err)
	return u
}

func (s *UserServiceTestSuite) walkPages(pageSize int) [][]string {
	var pages [][]string
	cursor := ""
	for range 50 {
		result, err := s.TC.Services.UserService.ListUsers(context.Background(), cursor, pageSize)
		s.Require().NoError(err)

		pages = append(pages, userEmails(result.Data))
		if result.NextCursor == nil {
			return pages
		}
		cursor = *result.NextCursor
	}
	s.FailNow("the pages never end")
	return nil
}

func cursorOf(raw string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func userEmails(users []*user.User) []string {
	emails := make([]string, len(users))
	for i, u := range users {
		emails[i] = u.Email
	}
	return emails
}
