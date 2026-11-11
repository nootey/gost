//go:build integration

package file_test

import (
	"bytes"
	"context"
	"gost/internal/apperr"
	"gost/internal/database"
	"gost/tests/integration"
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

func (s *FileServiceTestSuite) seedUser() int64 {
	role, err := s.TC.Queries.GetRoleByName(context.Background(), "user")
	s.Require().NoError(err)

	u, err := s.TC.Queries.CreateUser(context.Background(), database.CreateUserParams{
		Email:       "uploader@example.com",
		Password:    "hashed",
		DisplayName: "Uploader",
		RoleID:      role.ID,
	})
	s.Require().NoError(err)
	return u.ID
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
