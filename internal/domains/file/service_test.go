//go:build integration

package file_test

import (
	"bytes"
	"context"
	"fmt"
	"gost/internal/apperr"
	"gost/internal/database"
	"gost/internal/domains/file"
	"gost/tests/integration"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/suite"
)

type FileServiceTestSuite struct {
	integration.ServiceIntegrationSuite
}

func TestIntegrationFileServiceTestSuite(t *testing.T) {
	suite.Run(t, new(FileServiceTestSuite))
}

func (s *FileServiceTestSuite) TestUpload_StoresFileAndRecord() {
	userID := s.seedUser()

	f, err := s.TC.Services.FileService.Upload(context.Background(), userID, "notes.txt", strings.NewReader("hello world"))

	s.Require().NoError(err)
	s.Equal("notes.txt", f.Name)
	s.Equal(int64(11), f.SizeBytes)
	s.Equal("text/plain; charset=utf-8", f.ContentType)

	var key string
	var owner int64
	err = s.TC.DB.QueryRowContext(context.Background(),
		`SELECT storage_key, user_id FROM files WHERE id = $1`, f.ID).Scan(&key, &owner)
	s.Require().NoError(err)
	s.Equal(userID, owner)
	s.NotContains(key, "notes")

	content, err := os.ReadFile(filepath.Join(s.TC.UploadDir, key))
	s.Require().NoError(err)
	s.Equal("hello world", string(content))
}

func (s *FileServiceTestSuite) TestUpload_AtLimit() {
	userID := s.seedUser()

	f, err := s.TC.Services.FileService.Upload(context.Background(), userID, "exact.bin", bytes.NewReader(make([]byte, 1<<20)))

	s.Require().NoError(err)
	s.Equal(int64(1<<20), f.SizeBytes)
}

func (s *FileServiceTestSuite) TestUpload_TooLargeLeavesNothingBehind() {
	userID := s.seedUser()
	before := s.dirEntries()

	_, err := s.TC.Services.FileService.Upload(context.Background(), userID, "big.bin", bytes.NewReader(make([]byte, 1<<20+1)))

	status, msg := apperr.Resolve(err)
	s.Equal(http.StatusRequestEntityTooLarge, status)
	s.Equal("file is larger than the 1 MB limit", msg)
	s.Equal(before, s.dirEntries())
	s.Equal(0, s.fileRows())
}

func (s *FileServiceTestSuite) TestUpload_RejectsBadInput() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService

	cases := []struct {
		name    string
		content string
		message string
	}{
		{"", "x", "file name is required"},
		{"bad\xffname.txt", "x", "file name has invalid characters"},
		{"nul\x00.txt", "x", "file name has invalid characters"},
		{strings.Repeat("a", 256), "x", "file name is too long"},
		{"empty.txt", "", "file is empty"},
	}
	for _, tc := range cases {
		_, err := svc.Upload(context.Background(), userID, tc.name, strings.NewReader(tc.content))

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusBadRequest, status, tc.message)
		s.Equal(tc.message, msg)
	}
	s.Equal(0, s.fileRows())
}

func (s *FileServiceTestSuite) TestList_OnlyOwnFiles() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	s.seedFile(alice, "alice-1.txt")
	s.seedFile(alice, "alice-2.txt")
	s.seedFile(bob, "bob-1.txt")

	result, err := s.TC.Services.FileService.List(context.Background(), alice, 1, 10)

	s.Require().NoError(err)
	s.Equal(2, result.Total)
	s.Equal([]string{"alice-2.txt", "alice-1.txt"}, fileNames(result.Data))

	result, err = s.TC.Services.FileService.List(context.Background(), bob, 1, 10)

	s.Require().NoError(err)
	s.Equal(1, result.Total)
	s.Equal([]string{"bob-1.txt"}, fileNames(result.Data))
}

// Order must follow created_at, not insert order, so the oldest row is moved to the newest time.
func (s *FileServiceTestSuite) TestList_NewestFirst() {
	userID := s.seedUser()
	first := s.seedFile(userID, "a.txt")
	s.seedFile(userID, "b.txt")
	s.seedFile(userID, "c.txt")

	_, err := s.TC.DB.ExecContext(context.Background(),
		`UPDATE files SET created_at = NOW() + INTERVAL '1 hour' WHERE id = $1`, first)
	s.Require().NoError(err)

	result, err := s.TC.Services.FileService.List(context.Background(), userID, 1, 10)

	s.Require().NoError(err)
	s.Equal([]string{"a.txt", "c.txt", "b.txt"}, fileNames(result.Data))
}

func (s *FileServiceTestSuite) TestList_Pagination() {
	userID := s.seedUser()
	for i := 1; i <= 5; i++ {
		s.seedFile(userID, fmt.Sprintf("file-%d.txt", i))
	}
	svc := s.TC.Services.FileService

	second, err := svc.List(context.Background(), userID, 2, 2)

	s.Require().NoError(err)
	s.Equal([]string{"file-3.txt", "file-2.txt"}, fileNames(second.Data))
	s.Equal(5, second.Total)
	s.Equal(3, second.TotalPages)

	last, err := svc.List(context.Background(), userID, 3, 2)

	s.Require().NoError(err)
	s.Equal([]string{"file-1.txt"}, fileNames(last.Data))
}

func (s *FileServiceTestSuite) TestList_ClampsPageValues() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService

	cases := []struct {
		page, pageSize         int
		wantPage, wantPageSize int
	}{
		{0, 0, 1, 20},
		{-3, 101, 1, 20},
		{1, 100, 1, 100},
		{math.MaxInt, 20, math.MaxInt32 / 20, 20},
	}
	for _, tc := range cases {
		result, err := svc.List(context.Background(), userID, tc.page, tc.pageSize)

		s.Require().NoError(err)
		s.Equal(tc.wantPage, result.Page)
		s.Equal(tc.wantPageSize, result.PageSize)
		// An empty page must encode as [], not null.
		s.NotNil(result.Data)
		s.Empty(result.Data)
	}
}

func (s *FileServiceTestSuite) seedUser() int64 {
	return s.seedUserWithEmail("uploader@example.com")
}

func (s *FileServiceTestSuite) seedUserWithEmail(email string) int64 {
	role, err := s.TC.Queries.GetRoleByName(context.Background(), "user")
	s.Require().NoError(err)

	u, err := s.TC.Queries.CreateUser(context.Background(), database.CreateUserParams{
		Email:       email,
		Password:    "hashed",
		DisplayName: "Uploader",
		RoleID:      role.ID,
	})
	s.Require().NoError(err)
	return u.ID
}

func (s *FileServiceTestSuite) seedFile(userID int64, name string) int64 {
	f, err := s.TC.Queries.CreateFile(context.Background(), database.CreateFileParams{
		UserID:      userID,
		Name:        name,
		StorageKey:  fmt.Sprintf("%d-%s", userID, name),
		ContentType: "text/plain; charset=utf-8",
		SizeBytes:   1,
	})
	s.Require().NoError(err)
	return f.ID
}

func fileNames(files []*file.File) []string {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Name
	}
	return names
}

func (s *FileServiceTestSuite) dirEntries() []string {
	entries, err := os.ReadDir(s.TC.UploadDir)
	s.Require().NoError(err)
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}

func (s *FileServiceTestSuite) fileRows() int {
	var n int
	s.Require().NoError(s.TC.DB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM files`).Scan(&n))
	return n
}
