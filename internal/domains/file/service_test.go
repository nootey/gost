//go:build integration

package file_test

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"gost/internal/apperr"
	"gost/internal/database"
	"gost/internal/domains/file"
	"gost/internal/jobs"
	"gost/internal/uploads"
	"gost/internal/utils"
	"gost/tests/integration"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/suite"
	"github.com/tus/tusd/v2/pkg/filestore"
	tusd "github.com/tus/tusd/v2/pkg/handler"
)

// oldUpdatedAt is a fixed past time. A test sets updated_at to it, then checks which rows an action changed.
var oldUpdatedAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

type FileServiceTestSuite struct {
	integration.ServiceIntegrationSuite
}

func TestIntegrationFileServiceTestSuite(t *testing.T) {
	suite.Run(t, new(FileServiceTestSuite))
}

func (s *FileServiceTestSuite) TestList_OnlyOwnFiles() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	s.seedFile(alice, "alice-1.txt")
	s.seedFile(alice, "alice-2.txt")
	s.seedFile(bob, "bob-1.txt")

	result, err := s.TC.Services.FileService.List(context.Background(), alice, newestFirst(0))

	s.Require().NoError(err)
	s.Equal([]string{"alice-2.txt", "alice-1.txt"}, fileNames(result.Data))

	result, err = s.TC.Services.FileService.List(context.Background(), bob, newestFirst(0))

	s.Require().NoError(err)
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

	result, err := s.TC.Services.FileService.List(context.Background(), userID, newestFirst(0))

	s.Require().NoError(err)
	s.Equal([]string{"a.txt", "c.txt", "b.txt"}, fileNames(result.Data))
}

func (s *FileServiceTestSuite) TestList_Pages() {
	userID := s.seedUser()
	for i := 1; i <= 5; i++ {
		s.seedFile(userID, fmt.Sprintf("file-%d.txt", i))
	}

	pages := s.walkPages(userID, file.ListQuery{PageSize: 2, Sort: file.SortByName})

	s.Equal([][]string{{"file-1.txt", "file-2.txt"}, {"file-3.txt", "file-4.txt"}, {"file-5.txt"}}, pages)
}

// Page breaks fall inside the folders, at the end of the folders and inside the files.
// Rows tie on every sort key but id, so the cursor must keep the tie-break too.
func (s *FileServiceTestSuite) TestList_WalkEverySort() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	for _, name := range []string{"Beta", "alpha", "Gamma"} {
		s.createFolder(userID, 0, name)
	}
	s.seedFileWith(userID, "a.txt", "text/plain", 10, 0)
	s.seedFileWith(userID, "A.txt", "text/plain", 10, 0)
	s.seedFileWith(userID, "b.png", "image/png", 10, 0)
	s.seedFileWith(userID, "c.pdf", "text/plain", 5, 1)
	_, err := s.TC.DB.ExecContext(context.Background(),
		`UPDATE files SET created_at = '2026-01-01 10:00:00' WHERE name IN ('alpha', 'Gamma', 'a.txt', 'A.txt', 'b.png')`)
	s.Require().NoError(err)

	for _, sort := range []file.SortField{file.SortByName, file.SortBySize, file.SortByType, file.SortByDate, file.SortByModified} {
		var full [2][]string
		for i, desc := range []bool{false, true} {
			result, err := svc.List(context.Background(), userID, file.ListQuery{PageSize: 100, Sort: sort, Desc: desc})
			s.Require().NoError(err)
			s.Nil(result.NextCursor)
			full[i] = fileNames(result.Data)

			for _, pageSize := range []int{2, 3} {
				pages := s.walkPages(userID, file.ListQuery{PageSize: pageSize, Sort: sort, Desc: desc})

				s.Equal(full[i], slices.Concat(pages...), "%s desc=%t page size %d", sort, desc, pageSize)
				for _, page := range pages[:len(pages)-1] {
					s.Len(page, pageSize, "%s desc=%t page size %d", sort, desc, pageSize)
				}
			}
		}

		s.ElementsMatch([]string{"Beta", "alpha", "Gamma"}, full[0][:3], sort)
		ascFolders, ascFiles := slices.Clone(full[0][:3]), slices.Clone(full[0][3:])
		slices.Reverse(ascFolders)
		slices.Reverse(ascFiles)
		s.Equal(slices.Concat(ascFolders, ascFiles), full[1], "%s desc must reverse asc", sort)
	}
}

func (s *FileServiceTestSuite) TestList_ClampsPageSize() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService

	empty, err := svc.List(context.Background(), userID, newestFirst(0))
	s.Require().NoError(err)
	// An empty page must encode as [], not null.
	s.NotNil(empty.Data)
	s.Empty(empty.Data)
	s.Nil(empty.NextCursor)

	for i := 1; i <= 30; i++ {
		s.seedFile(userID, fmt.Sprintf("file-%d.txt", i))
	}
	cases := []struct {
		pageSize, want int
		more           bool
	}{
		{0, 25, true},
		{-3, 25, true},
		{101, 25, true},
		{100, 30, false},
		{30, 30, false},
	}
	for _, tc := range cases {
		q := newestFirst(0)
		q.PageSize = tc.pageSize
		result, err := svc.List(context.Background(), userID, q)

		s.Require().NoError(err)
		s.Len(result.Data, tc.want, tc.pageSize)
		s.Equal(tc.more, result.NextCursor != nil, tc.pageSize)
	}
}

// Tampered values reach Postgres, which rejects them by the type of their sort key.
func (s *FileServiceTestSuite) TestList_BadCursor() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	s.createFolder(userID, 0, "Docs")
	s.seedFile(userID, "a.txt")

	first, err := svc.List(context.Background(), userID, file.ListQuery{PageSize: 1, Sort: file.SortByName})
	s.Require().NoError(err)
	s.Require().NotNil(first.NextCursor)
	byName := *first.NextCursor

	cases := []struct {
		label  string
		cursor string
		sort   file.SortField
		desc   bool
	}{
		{"not base64", "!!!", file.SortByName, false},
		{"not json", cursorOf("nope"), file.SortByName, false},
		{"another sort", byName, file.SortBySize, false},
		{"another order", byName, file.SortByName, true},
		{"too few values", cursorOf(`{"s":"name","v":["a"]}`), file.SortByName, false},
		{"unknown part", cursorOf(`{"s":"name","p":2,"v":["a","1"]}`), file.SortByName, false},
		{"negative part", cursorOf(`{"s":"name","p":-1,"v":["a","1"]}`), file.SortByName, false},
		{"id not a number", cursorOf(`{"s":"name","v":["a","abc"]}`), file.SortByName, false},
		{"size out of range", cursorOf(`{"s":"size","v":["99999999999999999999","a","1"]}`), file.SortBySize, false},
		{"date not a time", cursorOf(`{"s":"date","p":1,"v":["not a time","1"]}`), file.SortByDate, false},
		{"nul byte in a name", cursorOf(`{"s":"name","v":["a\u0000","1"]}`), file.SortByName, false},
	}
	for _, tc := range cases {
		_, err := svc.List(context.Background(), userID, file.ListQuery{Cursor: tc.cursor, Sort: tc.sort, Desc: tc.desc})

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusBadRequest, status, tc.label)
		s.Equal("invalid cursor", msg, tc.label)
	}
}

// The list scans rows by hand, so a field it misses would only show here.
func (s *FileServiceTestSuite) TestList_ReturnsEveryField() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	folder := s.createFolder(userID, 0, "Docs")
	uploaded, err := s.finishUpload(userID, folder.ID, "notes.txt", "hello")
	s.Require().NoError(err)

	inFolder, err := svc.List(context.Background(), userID, newestFirst(folder.ID))
	s.Require().NoError(err)
	atRoot, err := svc.List(context.Background(), userID, newestFirst(0))
	s.Require().NoError(err)

	s.Equal([]*file.File{uploaded}, inFolder.Data)
	// The folder row stores no size. The list gives the total of the files under it.
	withSize := *folder
	withSize.SizeBytes = uploaded.SizeBytes
	s.Equal([]*file.File{&withSize}, atRoot.Data)
}

// A folder's size is the total of the files at any depth under it. Hidden files count. A file in the trash does not,
// and nor do the rows under a folder in the trash, before or after the trash job marks them. The details and the
// favorites list give the same size.
func (s *FileServiceTestSuite) TestList_FolderSizes() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	work := s.createFolder(userID, docs.ID, "Work")
	old := s.createFolder(userID, work.ID, "Old")
	s.createFolder(userID, 0, "Empty")
	s.upload(userID, docs.ID, "a.txt", "aaaa")
	s.upload(userID, work.ID, "b.txt", "bb")
	hidden := s.upload(userID, work.ID, "c.txt", "c")
	s.upload(userID, old.ID, "d.txt", "ddddd")
	gone := s.upload(userID, docs.ID, "e.txt", "eeeeee")
	s.upload(userID, 0, "root.txt", "r")
	s.Require().NoError(svc.SetHidden(context.Background(), userID, []int64{hidden.ID}, true))
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{old.ID, gone.ID}))
	s.Require().NoError(svc.AddFavorites(context.Background(), userID, []int64{docs.ID}))

	for _, label := range []string{"before the trash job", "after the trash job"} {
		atRoot, err := svc.List(context.Background(), userID, file.ListQuery{Sort: file.SortByName})
		s.Require().NoError(err)
		s.Equal(map[string]int64{"Docs": 7, "Empty": 0, "root.txt": 1}, fileSizes(atRoot.Data), label)

		inDocs, err := svc.List(context.Background(), userID, file.ListQuery{ParentID: docs.ID, Sort: file.SortByName})
		s.Require().NoError(err)
		s.Equal(map[string]int64{"Work": 3, "a.txt": 4}, fileSizes(inDocs.Data), label)

		details, err := svc.GetItem(context.Background(), userID, docs.ID)
		s.Require().NoError(err)
		s.Equal(int64(7), details.SizeBytes, label)

		favorites, err := svc.ListFavorites(context.Background(), userID, file.FavoriteQuery{Sort: file.SortByName})
		s.Require().NoError(err)
		s.Require().Len(favorites.Data, 1)
		s.Equal(int64(7), favorites.Data[0].SizeBytes, label)

		s.workTrashJobs()
	}
}

// The modified sort follows updated_at, folders first. A rename makes an item the newest.
func (s *FileServiceTestSuite) TestList_SortByModified() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	a := s.createFolder(userID, 0, "A")
	s.createFolder(userID, 0, "B")
	x := s.seedFile(userID, "x.txt")
	s.seedFile(userID, "y.txt")
	_, err := s.TC.DB.ExecContext(context.Background(), `UPDATE files SET updated_at = $1 WHERE user_id = $2`, oldUpdatedAt, userID)
	s.Require().NoError(err)
	_, err = svc.Rename(context.Background(), userID, a.ID, "A2")
	s.Require().NoError(err)
	_, err = svc.Rename(context.Background(), userID, x, "x2.txt")
	s.Require().NoError(err)

	asc, err := svc.List(context.Background(), userID, file.ListQuery{Sort: file.SortByModified})
	s.Require().NoError(err)
	desc, err := svc.List(context.Background(), userID, file.ListQuery{Sort: file.SortByModified, Desc: true})
	s.Require().NoError(err)

	s.Equal([]string{"B", "A2", "y.txt", "x2.txt"}, fileNames(asc.Data))
	s.Equal([]string{"A2", "B", "x2.txt", "y.txt"}, fileNames(desc.Data))
}

func (s *FileServiceTestSuite) TestFinishUpload_IntoFolder() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	folder := s.createFolder(userID, 0, "Docs")

	f, err := s.finishUpload(userID, folder.ID, "notes.txt", "hello")

	s.Require().NoError(err)
	s.Require().NotNil(f.ParentID)
	s.Equal(folder.ID, *f.ParentID)

	inFolder, err := svc.List(context.Background(), userID, newestFirst(folder.ID))
	s.Require().NoError(err)
	s.Equal([]string{"notes.txt"}, fileNames(inFolder.Data))

	atRoot, err := svc.List(context.Background(), userID, newestFirst(0))
	s.Require().NoError(err)
	s.Equal([]string{"Docs"}, fileNames(atRoot.Data))
}

func (s *FileServiceTestSuite) TestFinishUpload_UnknownFolderLeavesNothingBehind() {
	userID := s.seedUser()
	path := s.writePartial("x")
	before := s.dirEntries()

	_, err := s.TC.Services.FileService.FinishUpload(context.Background(), userID, 999, "a.txt", path)

	status, msg := apperr.Resolve(err)
	s.Equal(http.StatusNotFound, status)
	s.Equal("folder not found", msg)
	s.Equal(before, s.dirEntries())
	s.Equal(0, s.fileRows())
}

func (s *FileServiceTestSuite) TestFinishUpload_RenamesOnClash() {
	userID := s.seedUser()

	cases := []struct{ name, want string }{
		{"a.txt", "a.txt"},
		{"a.txt", "a (1).txt"},
		{"a.txt", "a (2).txt"},
		{".env", ".env"},
		{".env", ".env (1)"},
		{"archive.tar.gz", "archive.tar.gz"},
		{"archive.tar.gz", "archive.tar (1).gz"},
	}
	for _, tc := range cases {
		f, err := s.finishUpload(userID, 0, tc.name, "x")

		s.Require().NoError(err)
		s.Equal(tc.want, f.Name)
	}
}

// The lowest free number wins, so a gap left by a missing copy is filled first.
func (s *FileServiceTestSuite) TestFinishUpload_RenameFillsGaps() {
	userID := s.seedUser()
	s.seedFile(userID, "b.txt")
	s.seedFile(userID, "b (2).txt")

	f, err := s.finishUpload(userID, 0, "b.txt", "x")

	s.Require().NoError(err)
	s.Equal("b (1).txt", f.Name)
}

func (s *FileServiceTestSuite) TestCheckUpload_AcceptsGoodUpload() {
	userID := s.seedUser()
	folder := s.createFolder(userID, 0, "Videos")
	svc := s.TC.Services.FileService

	// Chunked uploads have no size cap.
	s.NoError(svc.CheckUpload(context.Background(), userID, 0, "film.mp4", 10<<30))
	s.NoError(svc.CheckUpload(context.Background(), userID, folder.ID, "film.mp4", 1))
	s.Equal(1, s.fileRows())
}

func (s *FileServiceTestSuite) TestCheckUpload_RejectsBadInput() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	theirs := s.createFolder(s.seedUserWithEmail("other@example.com"), 0, "Theirs")
	trashed := s.createFolder(userID, 0, "Trashed")
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{trashed.ID}))

	cases := []struct {
		parentID int64
		name     string
		size     int64
		status   int
		message  string
	}{
		{0, "", 1, http.StatusBadRequest, "file name is required"},
		{0, "   ", 1, http.StatusBadRequest, "file name is required"},
		{0, "bad\xffname.txt", 1, http.StatusBadRequest, "file name has invalid characters"},
		{0, "nul\x00.txt", 1, http.StatusBadRequest, "file name has invalid characters"},
		{0, "a/b.txt", 1, http.StatusBadRequest, "file name has invalid characters"},
		{0, "..", 1, http.StatusBadRequest, "file name has invalid characters"},
		{0, strings.Repeat("a", 256), 1, http.StatusBadRequest, "file name is too long"},
		{0, "empty.txt", 0, http.StatusBadRequest, "file is empty"},
		{999, "a.txt", 1, http.StatusNotFound, "folder not found"},
		{theirs.ID, "a.txt", 1, http.StatusNotFound, "folder not found"},
		{trashed.ID, "a.txt", 1, http.StatusNotFound, "folder not found"},
	}
	for _, tc := range cases {
		err := svc.CheckUpload(context.Background(), userID, tc.parentID, tc.name, tc.size)

		status, msg := apperr.Resolve(err)
		s.Equal(tc.status, status, tc.message)
		s.Equal(tc.message, msg)
	}
}

func (s *FileServiceTestSuite) TestFinishUpload_StoresFileAndRecord() {
	userID := s.seedUser()
	folder := s.createFolder(userID, 0, "Docs")
	path := s.writePartial("hello world")

	f, err := s.TC.Services.FileService.FinishUpload(context.Background(), userID, folder.ID, "notes.txt", path)

	s.Require().NoError(err)
	s.Equal("notes.txt", f.Name)
	s.Equal(folder.ID, *f.ParentID)
	s.Equal(int64(11), f.SizeBytes)
	s.Equal("text/plain; charset=utf-8", f.ContentType)

	var key string
	s.Require().NoError(s.TC.DB.QueryRowContext(context.Background(),
		`SELECT storage_key FROM files WHERE id = $1`, f.ID).Scan(&key))
	content, err := os.ReadFile(filepath.Join(s.TC.UploadDir, key))
	s.Require().NoError(err)
	s.Equal("hello world", string(content))
	s.NoFileExists(path)
}

func (s *FileServiceTestSuite) TestFinishUpload_FolderGoneLeavesNothingBehind() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	folder := s.createFolder(userID, 0, "Docs")
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{folder.ID}))
	path := s.writePartial("hello")
	before := s.dirEntries()

	_, err := svc.FinishUpload(context.Background(), userID, folder.ID, "notes.txt", path)

	status, msg := apperr.Resolve(err)
	s.Equal(http.StatusNotFound, status)
	s.Equal("folder not found", msg)
	s.Equal(before, s.dirEntries())
	s.Equal(1, s.fileRows())
	s.NoFileExists(path)
}

// A new file is empty bytes on disk. Its type is sniffed like an upload's, so the name's extension does not set it.
func (s *FileServiceTestSuite) TestCreateFile_StoresEmptyFile() {
	userID := s.seedUser()
	folder := s.createFolder(userID, 0, "Docs")

	f, err := s.TC.Services.FileService.CreateFile(context.Background(), userID, folder.ID, "  report.png  ")

	s.Require().NoError(err)
	s.Equal("report.png", f.Name)
	s.False(f.IsFolder)
	s.Equal(folder.ID, *f.ParentID)
	s.Equal(int64(0), f.SizeBytes)
	s.Equal("text/plain; charset=utf-8", f.ContentType)
	content, err := os.ReadFile(filepath.Join(s.TC.UploadDir, s.storageKey(f.ID)))
	s.Require().NoError(err)
	s.Empty(content)
	s.Equal([]map[string]any{{"event": "created", "name": "report.png", "folder": "Docs"}}, s.activity(userID, f.ID))
}

// A name with no extension gets its number at the end, and one with an extension gets it before the extension.
func (s *FileServiceTestSuite) TestCreateFile_RenamesOnClash() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService

	cases := []struct{ name, want string }{
		{"README", "README"},
		{"README", "README (1)"},
		{"notes.txt", "notes.txt"},
		{"notes.txt", "notes (1).txt"},
	}
	for _, tc := range cases {
		f, err := svc.CreateFile(context.Background(), userID, 0, tc.name)

		s.Require().NoError(err)
		s.Equal(tc.want, f.Name)
	}
}

// A bad name or folder fails before or after the bytes are made, and no row or bytes stay behind.
func (s *FileServiceTestSuite) TestCreateFile_FailureLeavesNothingBehind() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	bobFolder := s.createFolder(bob, 0, "Bob")
	svc := s.TC.Services.FileService
	rows := s.fileRows()
	before := s.dirEntries()

	cases := []struct {
		parentID int64
		name     string
		status   int
		message  string
	}{
		{0, "   ", http.StatusBadRequest, "file name is required"},
		{0, "a/b.txt", http.StatusBadRequest, "file name has invalid characters"},
		{0, strings.Repeat("a", 256), http.StatusBadRequest, "file name is too long"},
		{999, "a.txt", http.StatusNotFound, "folder not found"},
		{bobFolder.ID, "a.txt", http.StatusNotFound, "folder not found"},
	}
	for _, tc := range cases {
		_, err := svc.CreateFile(context.Background(), alice, tc.parentID, tc.name)

		status, msg := apperr.Resolve(err)
		s.Equal(tc.status, status, tc.message)
		s.Equal(tc.message, msg)
	}
	s.Equal(rows, s.fileRows())
	s.Equal(before, s.dirEntries())
}

func (s *FileServiceTestSuite) TestCreateFolder_AtRootAndNested() {
	userID := s.seedUser()

	photos := s.createFolder(userID, 0, "Photos")
	s.True(photos.IsFolder)
	s.Nil(photos.ParentID)
	s.Equal(int64(0), photos.SizeBytes)

	year := s.createFolder(userID, photos.ID, "2026")
	s.Require().NotNil(year.ParentID)
	s.Equal(photos.ID, *year.ParentID)

	var key sql.NullString
	err := s.TC.DB.QueryRowContext(context.Background(),
		`SELECT storage_key FROM files WHERE id = $1`, year.ID).Scan(&key)
	s.Require().NoError(err)
	s.False(key.Valid)
}

func (s *FileServiceTestSuite) TestCreateFolder_TrimsName() {
	userID := s.seedUser()

	f := s.createFolder(userID, 0, "  Photos  ")

	s.Equal("Photos", f.Name)
}

func (s *FileServiceTestSuite) TestCreateFolder_RejectsBadNames() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService

	cases := []struct{ name, message string }{
		{"", "folder name is required"},
		{"   ", "folder name is required"},
		{"a/b", "folder name has invalid characters"},
		{".", "folder name has invalid characters"},
		{"..", "folder name has invalid characters"},
		{"nul\x00", "folder name has invalid characters"},
		{strings.Repeat("a", 256), "folder name is too long"},
	}
	for _, tc := range cases {
		_, err := svc.CreateFolder(context.Background(), userID, 0, tc.name)

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusBadRequest, status, tc.message)
		s.Equal(tc.message, msg)
	}
	s.Equal(0, s.fileRows())
}

// A parent must be a folder the user owns: unknown ids, files and other users' folders all fail the same way.
func (s *FileServiceTestSuite) TestCreateFolder_UnknownParent() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	aliceFile := s.seedFile(alice, "a.txt")
	bobFolder := s.createFolder(bob, 0, "Bob")

	for _, parentID := range []int64{999, -1, aliceFile, bobFolder.ID} {
		_, err := s.TC.Services.FileService.CreateFolder(context.Background(), alice, parentID, "New")

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusNotFound, status, parentID)
		s.Equal("folder not found", msg)
	}
}

// Folders and files share one set of names in a folder. A folder has no extension, so the number goes at the end.
func (s *FileServiceTestSuite) TestCreateFolder_RenamesOnClash() {
	userID := s.seedUser()
	s.seedFile(userID, "a.txt")

	s.Equal("Photos", s.createFolder(userID, 0, "Photos").Name)
	s.Equal("Photos (1)", s.createFolder(userID, 0, "Photos").Name)
	s.Equal("a.txt (1)", s.createFolder(userID, 0, "a.txt").Name)

	f, err := s.finishUpload(userID, 0, "Photos", "x")
	s.Require().NoError(err)
	s.Equal("Photos (2)", f.Name)
}

// Names only clash inside one folder of one user.
func (s *FileServiceTestSuite) TestCreateFolder_SameNameElsewhere() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")

	parent := s.createFolder(alice, 0, "Photos")
	s.Equal("Photos", s.createFolder(alice, parent.ID, "Photos").Name)
	s.Equal("Photos", s.createFolder(bob, 0, "Photos").Name)
}

func (s *FileServiceTestSuite) TestCreateFolder_NoRoomForNumber() {
	userID := s.seedUser()
	name := strings.Repeat("a", 255)
	s.createFolder(userID, 0, name)

	_, err := s.TC.Services.FileService.CreateFolder(context.Background(), userID, 0, name)

	status, msg := apperr.Resolve(err)
	s.Equal(http.StatusBadRequest, status)
	s.Equal("folder name is too long", msg)
}

// Parallel creates all pick the same free name first; the losers must retry and get the next one.
func (s *FileServiceTestSuite) TestCreateFolder_ParallelClash() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService

	const n = 3
	names := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			f, err := svc.CreateFolder(context.Background(), userID, 0, "Shared")
			errs[i] = err
			if err == nil {
				names[i] = f.Name
			}
		})
	}
	wg.Wait()

	for _, err := range errs {
		s.Require().NoError(err)
	}
	s.ElementsMatch([]string{"Shared", "Shared (1)", "Shared (2)"}, names)
}

// The paths may leave out the folders above them. Each folder gets the right parent and one "created" row.
func (s *FileServiceTestSuite) TestCreateFolderTree_MakesTheTree() {
	userID := s.seedUser()
	into := s.createFolder(userID, 0, "Uploads")

	ids, err := s.TC.Services.FileService.CreateFolderTree(context.Background(), userID, into.ID,
		[]string{"Photos/2024/Trip", "Photos/2023", "Docs", "Photos/2024"})

	s.Require().NoError(err)
	s.Equal([]string{"Docs/", "Photos/", "Photos/2023/", "Photos/2024/", "Photos/2024/Trip/"}, s.treeOf(into.ID))
	s.Len(ids, 5)
	for path, id := range ids {
		parent := into.ID
		if i := strings.LastIndex(path, "/"); i >= 0 {
			parent = ids[path[:i]]
		}
		s.Equal(parent, s.parentOf(id), path)
		events := s.activity(userID, id)
		s.Require().Len(events, 1, path)
		s.Equal("created", events[0]["event"], path)
	}
}

// Only the folders on top can clash, with folders and files alike. The folders under them keep their names.
func (s *FileServiceTestSuite) TestCreateFolderTree_RenamesOnlyTheTop() {
	userID := s.seedUser()
	s.createFolder(userID, 0, "Photos")
	s.createFolder(userID, 0, "Photos (1)")
	s.upload(userID, 0, "Docs", "x")

	ids, err := s.TC.Services.FileService.CreateFolderTree(context.Background(), userID, 0,
		[]string{"Photos/Photos", "Docs"})

	s.Require().NoError(err)
	s.Equal(s.childID(userID, 0, "Photos (2)"), ids["Photos"])
	s.Equal(s.childID(userID, 0, "Docs (1)"), ids["Docs"])
	s.Equal(s.childID(userID, ids["Photos"], "Photos"), ids["Photos/Photos"])
}

// One bad path fails the whole call, and nothing goes in.
func (s *FileServiceTestSuite) TestCreateFolderTree_RejectsBadInput() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	trashed := s.createFolder(userID, 0, "Trashed")
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{trashed.ID}))
	tooMany := make([]string, 10001)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("f%d", i)
	}

	cases := []struct {
		parentID int64
		paths    []string
		status   int
		message  string
	}{
		{0, nil, http.StatusBadRequest, "invalid paths"},
		{0, []string{"Docs", ""}, http.StatusBadRequest, "folder name is required"},
		{0, []string{"Docs/a//b"}, http.StatusBadRequest, "folder name is required"},
		{0, []string{"/Docs"}, http.StatusBadRequest, "folder name is required"},
		{0, []string{"Docs/.."}, http.StatusBadRequest, "folder name has invalid characters"},
		{0, []string{"Docs/nul\x00"}, http.StatusBadRequest, "folder name has invalid characters"},
		{0, []string{"Docs/" + strings.Repeat("a", 256)}, http.StatusBadRequest, "folder name is too long"},
		{0, tooMany, http.StatusBadRequest, "a folder upload can make at most 10000 folders"},
		{999, []string{"Docs"}, http.StatusNotFound, "folder not found"},
		{trashed.ID, []string{"Docs"}, http.StatusNotFound, "folder not found"},
	}
	for _, tc := range cases {
		_, err := svc.CreateFolderTree(context.Background(), userID, tc.parentID, tc.paths)

		status, msg := apperr.Resolve(err)
		s.Equal(tc.status, status, tc.message)
		s.Equal(tc.message, msg)
	}
	s.Equal(1, s.fileRows())
}

// The cap fits in one call: 100 folders on top with 99 folders in each.
func (s *FileServiceTestSuite) TestCreateFolderTree_AtTheCap() {
	userID := s.seedUser()
	var paths []string
	for i := range 100 {
		for j := range 99 {
			paths = append(paths, fmt.Sprintf("d%d/e%d", i, j))
		}
	}

	ids, err := s.TC.Services.FileService.CreateFolderTree(context.Background(), userID, 0, paths)

	s.Require().NoError(err)
	s.Len(ids, 10000)
	s.Equal(10000, s.fileRows())
	s.Equal(ids["d42"], s.parentOf(ids["d42/e7"]))
}

// A delete of a folder and creates into it run at once. A create may fail, but it must never leave a live row inside the trash.
func (s *FileServiceTestSuite) TestFinishUploadAndCreateFolder_ParallelWithTrashOfParent() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService

	const n = 20
	folders := make([]int64, n)
	for i := range n {
		folders[i] = s.createFolder(userID, 0, fmt.Sprintf("Docs %d", i)).ID
	}

	partials := make([]string, n)
	for i := range n {
		partials[i] = s.writePartial("n")
	}

	trashErrs := make([]error, n)
	createErrs := make([]error, 2*n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { trashErrs[i] = svc.Trash(context.Background(), userID, []int64{folders[i]}) })
		wg.Go(func() {
			_, createErrs[2*i] = svc.FinishUpload(context.Background(), userID, folders[i], "new.txt", partials[i])
		})
		wg.Go(func() { _, createErrs[2*i+1] = svc.CreateFolder(context.Background(), userID, folders[i], "New") })
	}
	wg.Wait()

	for _, err := range trashErrs {
		s.Require().NoError(err)
	}
	for _, err := range createErrs {
		if err != nil {
			_, msg := apperr.Resolve(err)
			s.Equal("folder not found", msg)
		}
	}
	s.workTrashJobs()
	s.Zero(s.liveRowsInTrash(userID))
}

func (s *FileServiceTestSuite) TestList_FoldersFirst() {
	userID := s.seedUser()
	s.seedFile(userID, "z.txt")
	s.createFolder(userID, 0, "B")
	s.seedFile(userID, "y.txt")
	s.createFolder(userID, 0, "A")

	result, err := s.TC.Services.FileService.List(context.Background(), userID, newestFirst(0))

	s.Require().NoError(err)
	s.Equal([]string{"A", "B", "y.txt", "z.txt"}, fileNames(result.Data))
}

// Folder names and file names differ in case, so a byte-order sort would give another order.
// Folders tie on size and type, and a descending sort reverses the tie too.
func (s *FileServiceTestSuite) TestList_SortByEachColumn() {
	userID := s.seedUser()
	s.moveCreatedAt(s.createFolder(userID, 0, "Zeta").ID, 2)
	s.moveCreatedAt(s.createFolder(userID, 0, "alpha").ID, 1)
	s.seedFileWith(userID, "b.txt", "text/plain", 10, 2)
	s.seedFileWith(userID, "C.pdf", "application/pdf", 30, 3)
	s.seedFileWith(userID, "a.png", "image/png", 20, 1)

	cases := []struct {
		sort file.SortField
		desc bool
		want []string
	}{
		{file.SortByName, false, []string{"alpha", "Zeta", "a.png", "b.txt", "C.pdf"}},
		{file.SortByName, true, []string{"Zeta", "alpha", "C.pdf", "b.txt", "a.png"}},
		{file.SortBySize, false, []string{"alpha", "Zeta", "b.txt", "a.png", "C.pdf"}},
		{file.SortBySize, true, []string{"Zeta", "alpha", "C.pdf", "a.png", "b.txt"}},
		{file.SortByType, false, []string{"alpha", "Zeta", "C.pdf", "a.png", "b.txt"}},
		{file.SortByType, true, []string{"Zeta", "alpha", "b.txt", "a.png", "C.pdf"}},
		{file.SortByDate, false, []string{"Zeta", "alpha", "C.pdf", "b.txt", "a.png"}},
		{file.SortByDate, true, []string{"alpha", "Zeta", "a.png", "b.txt", "C.pdf"}},
	}
	for _, tc := range cases {
		result, err := s.TC.Services.FileService.List(context.Background(), userID,
			file.ListQuery{PageSize: 10, Sort: tc.sort, Desc: tc.desc})

		s.Require().NoError(err)
		s.Equal(tc.want, fileNames(result.Data), "%s desc=%t", tc.sort, tc.desc)
	}
}

func (s *FileServiceTestSuite) TestList_Search() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	folder := s.createFolder(userID, 0, "Reports")
	s.seedFile(userID, "q1-report.pdf")
	s.seedFile(userID, "notes.txt")
	s.seedFile(userID, "100%_done.txt")
	_, err := s.finishUpload(userID, folder.ID, "report-2.txt", "x")
	s.Require().NoError(err)

	cases := []struct {
		search string
		want   []string
	}{
		{"REPORT", []string{"Reports", "q1-report.pdf"}},
		{"  notes  ", []string{"notes.txt"}},
		{"%", []string{"100%_done.txt"}},
		{"_", []string{"100%_done.txt"}},
		{"   ", []string{"Reports", "100%_done.txt", "notes.txt", "q1-report.pdf"}},
		{"missing", []string{}},
	}
	for _, tc := range cases {
		result, err := svc.List(context.Background(), userID,
			file.ListQuery{PageSize: 10, Search: tc.search, Sort: file.SortByName})

		s.Require().NoError(err)
		s.Equal(tc.want, fileNames(result.Data), tc.search)
	}
}

func (s *FileServiceTestSuite) TestList_UnknownFolder() {
	userID := s.seedUser()
	fileID := s.seedFile(userID, "a.txt")

	for _, parentID := range []int64{999, fileID} {
		_, err := s.TC.Services.FileService.List(context.Background(), userID, newestFirst(parentID))

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusNotFound, status, parentID)
		s.Equal("folder not found", msg)
	}
}

func (s *FileServiceTestSuite) TestResolveFolder_RootFirst() {
	userID := s.seedUser()
	photos := s.createFolder(userID, 0, "My Photos")
	cars := s.createFolder(userID, photos.ID, "Cars")

	result, err := s.TC.Services.FileService.ResolveFolder(context.Background(), userID, []string{"My Photos", "Cars"})

	s.Require().NoError(err)
	s.Require().Len(result.Path, 2)
	s.Equal([]int64{photos.ID, cars.ID}, []int64{result.Path[0].ID, result.Path[1].ID})
	s.Equal([]string{"My Photos", "Cars"}, []string{result.Path[0].Name, result.Path[1].Name})
}

func (s *FileServiceTestSuite) TestResolveFolder_Root() {
	userID := s.seedUser()
	s.createFolder(userID, 0, "Photos")

	for _, names := range [][]string{nil, {}} {
		result, err := s.TC.Services.FileService.ResolveFolder(context.Background(), userID, names)

		s.Require().NoError(err, names)
		s.NotNil(result.Path, names)
		s.Empty(result.Path, names)
	}
}

// Wrong case, a missing level, a file and another user's folder all fail the same way.
func (s *FileServiceTestSuite) TestResolveFolder_UnknownFolder() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	photos := s.createFolder(alice, 0, "Photos")
	s.createFolder(alice, photos.ID, "Cars")
	s.seedFile(alice, "a.txt")
	s.createFolder(bob, 0, "Bob")

	for _, names := range [][]string{{"photos"}, {"Cars"}, {"Photos", "Cars", "Old"}, {"a.txt"}, {"Bob"}} {
		_, err := s.TC.Services.FileService.ResolveFolder(context.Background(), alice, names)

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusNotFound, status, names)
		s.Equal("folder not found", msg, names)
	}
}

func (s *FileServiceTestSuite) TestRename_File() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	id := s.seedFile(userID, "notes.txt")

	f, err := svc.Rename(context.Background(), userID, id, "  report.md  ")

	s.Require().NoError(err)
	s.Equal(id, f.ID)
	s.Equal("report.md", f.Name)
	s.False(f.IsFolder)
	s.Equal("text/plain; charset=utf-8", f.ContentType)

	result, err := svc.List(context.Background(), userID, newestFirst(0))
	s.Require().NoError(err)
	s.Equal([]string{"report.md"}, fileNames(result.Data))
}

// Children point at the folder by id, so they stay inside it, and the path resolves only by the new name.
func (s *FileServiceTestSuite) TestRename_FolderKeepsChildren() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	photos := s.createFolder(userID, 0, "Photos")
	s.createFolder(userID, photos.ID, "Cars")

	f, err := svc.Rename(context.Background(), userID, photos.ID, "Pictures")

	s.Require().NoError(err)
	s.True(f.IsFolder)
	s.Equal("Pictures", f.Name)

	path, err := svc.ResolveFolder(context.Background(), userID, []string{"Pictures", "Cars"})
	s.Require().NoError(err)
	s.Len(path.Path, 2)

	_, err = svc.ResolveFolder(context.Background(), userID, []string{"Photos"})
	status, _ := apperr.Resolve(err)
	s.Equal(http.StatusNotFound, status)
}

// The same name, or the same name in another case, is a valid rename. Names are case-sensitive.
func (s *FileServiceTestSuite) TestRename_SameNameAndCase() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	id := s.seedFile(userID, "a.txt")

	for _, name := range []string{"a.txt", "A.txt"} {
		f, err := svc.Rename(context.Background(), userID, id, name)

		s.Require().NoError(err, name)
		s.Equal(name, f.Name)
	}
}

// The messages name the kind of the item, as on upload and folder create.
func (s *FileServiceTestSuite) TestRename_RejectsBadNames() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	fileID := s.seedFile(userID, "a.txt")
	folder := s.createFolder(userID, 0, "Photos")

	cases := []struct {
		id      int64
		name    string
		message string
	}{
		{fileID, "", "file name is required"},
		{fileID, "   ", "file name is required"},
		{fileID, "a/b.txt", "file name has invalid characters"},
		{fileID, strings.Repeat("a", 256), "file name is too long"},
		{folder.ID, "", "folder name is required"},
		{folder.ID, "..", "folder name has invalid characters"},
		{folder.ID, "nul\x00", "folder name has invalid characters"},
	}
	for _, tc := range cases {
		_, err := svc.Rename(context.Background(), userID, tc.id, tc.name)

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusBadRequest, status, tc.message)
		s.Equal(tc.message, msg)
	}

	result, err := svc.List(context.Background(), userID, newestFirst(0))
	s.Require().NoError(err)
	s.Equal([]string{"Photos", "a.txt"}, fileNames(result.Data))
}

// Folders and files share one set of names, so a clash with either kind is refused. Nothing changes.
func (s *FileServiceTestSuite) TestRename_NameTaken() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	fileID := s.seedFile(userID, "a.txt")
	s.seedFile(userID, "b.txt")
	folder := s.createFolder(userID, 0, "Photos")

	cases := []struct {
		id   int64
		name string
	}{
		{fileID, "b.txt"},
		{fileID, "Photos"},
		{folder.ID, "a.txt"},
	}
	for _, tc := range cases {
		_, err := svc.Rename(context.Background(), userID, tc.id, tc.name)

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusConflict, status, tc.name)
		s.Equal("an item with this name already exists", msg)
	}

	result, err := svc.List(context.Background(), userID, newestFirst(0))
	s.Require().NoError(err)
	s.ElementsMatch([]string{"Photos", "a.txt", "b.txt"}, fileNames(result.Data))
}

// A name in use in another folder is free here.
func (s *FileServiceTestSuite) TestRename_SameNameElsewhere() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	s.createFolder(userID, docs.ID, "Old")
	id := s.seedFile(userID, "a.txt")

	f, err := svc.Rename(context.Background(), userID, id, "Old")

	s.Require().NoError(err)
	s.Equal("Old", f.Name)
}

// Unknown ids and other users' items fail the same way, and the other user's item keeps its name.
func (s *FileServiceTestSuite) TestRename_NotFound() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	bobFile := s.seedFile(bob, "bob.txt")
	bobFolder := s.createFolder(bob, 0, "Bob")

	for _, id := range []int64{999, bobFile, bobFolder.ID} {
		_, err := s.TC.Services.FileService.Rename(context.Background(), alice, id, "mine.txt")

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusNotFound, status, id)
		s.Equal("item not found", msg)
	}

	result, err := s.TC.Services.FileService.List(context.Background(), bob, newestFirst(0))
	s.Require().NoError(err)
	s.Equal([]string{"Bob", "bob.txt"}, fileNames(result.Data))
}

// Favorites show in the list. Adding a favorite again changes nothing.
func (s *FileServiceTestSuite) TestAddFavorites_MarksItems() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	a := s.upload(userID, 0, "a.txt", "a")
	b := s.upload(userID, 0, "b.txt", "b")

	s.Require().NoError(svc.AddFavorites(context.Background(), userID, []int64{docs.ID, a.ID, a.ID}))

	s.Equal(map[string]bool{"Docs": true, "a.txt": true, "b.txt": false}, s.favoriteMarks(userID, 0))

	s.Require().NoError(svc.AddFavorites(context.Background(), userID, []int64{a.ID, b.ID}))

	s.Equal(map[string]bool{"Docs": true, "a.txt": true, "b.txt": true}, s.favoriteMarks(userID, 0))
	s.Equal(3, s.favoriteRows())
}

// One bad id undoes the rest: an unknown id, another user's item, or an item in the trash.
func (s *FileServiceTestSuite) TestAddFavorites_AllOrNone() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	a := s.upload(alice, 0, "a.txt", "a")
	trashed := s.upload(alice, 0, "trashed.txt", "t")
	s.Require().NoError(svc.Trash(context.Background(), alice, []int64{trashed.ID}))
	bobFile := s.upload(bob, 0, "bob.txt", "b")

	for _, bad := range []int64{999, bobFile.ID, trashed.ID} {
		err := svc.AddFavorites(context.Background(), alice, []int64{a.ID, bad})

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusNotFound, status, bad)
		s.Equal("item not found", msg)
	}
	s.Equal(0, s.favoriteRows())
}

// Remove drops only the user's own marks. An id that is no favorite changes nothing.
func (s *FileServiceTestSuite) TestRemoveFavorites() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	a := s.upload(alice, 0, "a.txt", "a")
	b := s.upload(alice, 0, "b.txt", "b")
	s.Require().NoError(svc.AddFavorites(context.Background(), alice, []int64{a.ID, b.ID}))

	s.Require().NoError(svc.RemoveFavorites(context.Background(), bob, []int64{a.ID, b.ID}))
	s.Require().NoError(svc.RemoveFavorites(context.Background(), alice, []int64{a.ID, 999}))

	s.Equal(map[string]bool{"a.txt": false, "b.txt": true}, s.favoriteMarks(alice, 0))
}

// A favorite keeps its mark through a rename, the trash and a restore. A copy is no favorite, and a purge removes
// the mark with the row.
func (s *FileServiceTestSuite) TestFavorites_FollowTheItem() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	inner := s.upload(userID, docs.ID, "in.txt", "in")
	other := s.createFolder(userID, 0, "Other")
	s.Require().NoError(svc.AddFavorites(context.Background(), userID, []int64{docs.ID, inner.ID}))

	renamed, err := svc.Rename(context.Background(), userID, docs.ID, "Papers")
	s.Require().NoError(err)
	s.True(renamed.IsFavorite)

	s.Require().NoError(svc.Copy(context.Background(), userID, other.ID, []int64{docs.ID}))
	s.workCopyJobs()
	s.Equal(map[string]bool{"Papers": false}, s.favoriteMarks(userID, other.ID))

	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{docs.ID}))
	s.workTrashJobs()
	trash, err := svc.ListTrash(context.Background(), userID, file.TrashQuery{Sort: file.SortByDate})
	s.Require().NoError(err)
	s.Require().Len(trash.Data, 1)
	s.True(trash.Data[0].IsFavorite)

	s.Require().NoError(svc.Restore(context.Background(), userID, []int64{docs.ID}))
	s.Equal(map[string]bool{"in.txt": true}, s.favoriteMarks(userID, docs.ID))

	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{docs.ID}))
	s.workTrashJobs()
	s.Require().NoError(svc.Purge(context.Background(), userID, []int64{docs.ID}))
	s.workPurgeJobs()
	s.Equal(0, s.favoriteRows())
}

// The list holds the user's live favorites from every folder, folders first. Each item names the path of its folder,
// read at the time of the list, so it follows a rename. Items in the trash and other users' favorites stay out.
func (s *FileServiceTestSuite) TestListFavorites_FromAllFolders() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	docs := s.createFolder(alice, 0, "Docs")
	work := s.createFolder(alice, docs.ID, "Work")
	report := s.upload(alice, work.ID, "report.txt", "r")
	notes := s.upload(alice, 0, "notes.txt", "n")
	s.upload(alice, 0, "plain.txt", "p")
	trashed := s.upload(alice, docs.ID, "trashed.txt", "t")
	bobFile := s.upload(bob, 0, "bob.txt", "b")
	s.Require().NoError(svc.AddFavorites(context.Background(), alice, []int64{report.ID, notes.ID, work.ID, trashed.ID}))
	s.Require().NoError(svc.AddFavorites(context.Background(), bob, []int64{bobFile.ID}))
	s.Require().NoError(svc.Trash(context.Background(), alice, []int64{trashed.ID}))
	_, err := svc.Rename(context.Background(), alice, docs.ID, "Papers")
	s.Require().NoError(err)

	result, err := svc.ListFavorites(context.Background(), alice, file.FavoriteQuery{Sort: file.SortByName})

	s.Require().NoError(err)
	s.Nil(result.NextCursor)
	s.Equal([]string{"Work", "notes.txt", "report.txt"}, favoriteNames(result.Data))
	s.Equal(map[string][]string{
		"Work":       {"Papers"},
		"notes.txt":  {},
		"report.txt": {"Papers", "Work"},
	}, favoritePaths(result.Data))
	s.True(result.Data[0].IsFolder)
	for _, item := range result.Data {
		s.True(item.IsFavorite, item.Name)
	}

	empty, err := svc.ListFavorites(context.Background(), s.seedUserWithEmail("carol@example.com"), file.FavoriteQuery{Sort: file.SortByName})
	s.Require().NoError(err)
	s.NotNil(empty.Data)
	s.Empty(empty.Data)
}

// The recents hold the user's newest added or changed files from all folders, newest first. A rename, a move, a
// restore and a copy each put a file on top. Folders, hidden files, files in the trash and other users' files stay
// out. Each file names the path of its folder and keeps its favorite mark.
func (s *FileServiceTestSuite) TestListRecents_NewestChangesFirst() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	docs := s.createFolder(alice, 0, "Docs")
	work := s.createFolder(alice, docs.ID, "Work")
	report := s.upload(alice, work.ID, "report.txt", "r")
	notes := s.upload(alice, 0, "notes.txt", "n")
	s.upload(alice, 0, "old.txt", "o")
	moved := s.upload(alice, 0, "moved.txt", "m")
	restored := s.upload(alice, 0, "restored.txt", "x")
	hidden := s.upload(alice, 0, "hidden.txt", "h")
	trashed := s.upload(alice, 0, "trashed.txt", "t")
	s.upload(bob, 0, "bob.txt", "b")
	s.Require().NoError(svc.Trash(context.Background(), alice, []int64{restored.ID, trashed.ID}))
	s.Require().NoError(svc.SetHidden(context.Background(), alice, []int64{hidden.ID}, true))
	s.Require().NoError(svc.AddFavorites(context.Background(), alice, []int64{notes.ID}))
	// Every row gets the same old time, so only the changes below set the order, and the id breaks the tie.
	_, err := s.TC.DB.ExecContext(context.Background(), `UPDATE files SET updated_at = $1 WHERE user_id = $2`, oldUpdatedAt, alice)
	s.Require().NoError(err)
	_, err = svc.Rename(context.Background(), alice, notes.ID, "notes-v2.txt")
	s.Require().NoError(err)
	s.Require().NoError(svc.Move(context.Background(), alice, docs.ID, []int64{moved.ID}))
	s.Require().NoError(svc.Restore(context.Background(), alice, []int64{restored.ID}))
	s.Require().NoError(svc.Copy(context.Background(), alice, docs.ID, []int64{report.ID}))

	list, err := svc.ListRecents(context.Background(), alice)

	s.Require().NoError(err)
	s.Equal([]string{
		"Docs/report.txt",
		"restored.txt",
		"Docs/moved.txt",
		"notes-v2.txt",
		"old.txt",
		"Docs/Work/report.txt",
	}, recentEntries(list))
	s.True(list[0].IsCopying)
	for _, item := range list {
		s.Equal(item.ID == notes.ID, item.IsFavorite, item.Name)
	}

	empty, err := svc.ListRecents(context.Background(), s.seedUserWithEmail("carol@example.com"))
	s.Require().NoError(err)
	s.NotNil(empty)
	s.Empty(empty)
}

// The recents stop at the newest 10 files.
func (s *FileServiceTestSuite) TestListRecents_NewestTenOnly() {
	userID := s.seedUser()
	for i := range 12 {
		s.upload(userID, 0, fmt.Sprintf("file-%02d.txt", i), "f")
	}

	list, err := s.TC.Services.FileService.ListRecents(context.Background(), userID)

	s.Require().NoError(err)
	s.Require().Len(list, 10)
	s.Equal("file-11.txt", list[0].Name)
	s.Equal("file-02.txt", list[9].Name)
}

// Hidden favorites show only with ShowHidden. Like the file list, the check looks at the item only, so an item in a
// hidden folder still shows.
func (s *FileServiceTestSuite) TestListFavorites_HiddenOnlyWithToggle() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	secret := s.createFolder(userID, 0, "Secret")
	inner := s.upload(userID, secret.ID, "inner.txt", "i")
	shown := s.upload(userID, 0, "shown.txt", "s")
	s.Require().NoError(svc.AddFavorites(context.Background(), userID, []int64{secret.ID, inner.ID, shown.ID}))
	s.Require().NoError(svc.SetHidden(context.Background(), userID, []int64{secret.ID}, true))

	result, err := svc.ListFavorites(context.Background(), userID, file.FavoriteQuery{Sort: file.SortByName})
	s.Require().NoError(err)
	s.Equal([]string{"inner.txt", "shown.txt"}, favoriteNames(result.Data))

	all, err := svc.ListFavorites(context.Background(), userID, file.FavoriteQuery{Sort: file.SortByName, ShowHidden: true})
	s.Require().NoError(err)
	s.Equal([]string{"Secret", "inner.txt", "shown.txt"}, favoriteNames(all.Data))
	s.True(all.Data[0].IsHidden)
	s.False(all.Data[1].IsHidden)
}

// The same walk as the file list, over favorites in different folders: every page joins up to the full list, and desc
// reverses asc inside each part. a.txt and A.txt tie on size and name, so the cursor must keep the id tie-break.
func (s *FileServiceTestSuite) TestListFavorites_WalkEverySort() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	box := s.createFolder(userID, 0, "Box")
	ids := []int64{box.ID, s.createFolder(userID, box.ID, "Beta").ID, s.createFolder(userID, box.ID, "alpha").ID}
	ids = append(ids,
		s.seedFileWith(userID, "a.txt", "text/plain", 10, 0),
		s.seedFileWith(userID, "b.png", "image/png", 10, 2),
		s.seedFileWith(userID, "c.pdf", "text/plain", 5, 1),
		s.upload(userID, box.ID, "A.txt", "0123456789").ID,
	)
	s.moveCreatedAt(ids[1], 3)
	s.Require().NoError(svc.AddFavorites(context.Background(), userID, ids))

	for _, sort := range []file.SortField{file.SortByName, file.SortBySize, file.SortByType, file.SortByDate} {
		var full [2][]string
		for i, desc := range []bool{false, true} {
			result, err := svc.ListFavorites(context.Background(), userID, file.FavoriteQuery{PageSize: 100, Sort: sort, Desc: desc})
			s.Require().NoError(err)
			full[i] = favoriteNames(result.Data)

			pages := s.walkFavorites(userID, file.FavoriteQuery{PageSize: 2, Sort: sort, Desc: desc})
			s.Equal(full[i], slices.Concat(pages...), "%s desc=%t", sort, desc)
			for _, page := range pages[:len(pages)-1] {
				s.Len(page, 2, "%s desc=%t", sort, desc)
			}
		}

		s.Len(full[0], 7, sort)
		s.ElementsMatch([]string{"Box", "Beta", "alpha"}, full[0][:3], sort)
		ascFolders, ascFiles := slices.Clone(full[0][:3]), slices.Clone(full[0][3:])
		slices.Reverse(ascFolders)
		slices.Reverse(ascFiles)
		s.Equal(slices.Concat(ascFolders, ascFiles), full[1], "%s desc must reverse asc", sort)
	}
}

// Upload, new folder and rename each write one audit row with the name and folder after the change. A rename to the
// same name writes none.
func (s *FileServiceTestSuite) TestActivity_UploadCreateAndRename() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	a := s.upload(userID, docs.ID, "a.txt", "a")
	_, err := svc.Rename(context.Background(), userID, a.ID, "b.txt")
	s.Require().NoError(err)
	_, err = svc.Rename(context.Background(), userID, a.ID, "b.txt")
	s.Require().NoError(err)

	s.Equal([]map[string]any{
		{"event": "renamed", "name": "b.txt", "folder": "Docs", "old_name": "a.txt"},
		{"event": "uploaded", "name": "a.txt", "folder": "Docs"},
	}, s.activity(userID, a.ID))
	s.Equal([]map[string]any{{"event": "created", "name": "Docs", "folder": nil}}, s.activity(userID, docs.ID))
}

// A move logs the folder each item leaves and the one it goes to. A copy logs only the copies the user pasted.
func (s *FileServiceTestSuite) TestActivity_MoveAndCopy() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	photos := s.createFolder(userID, 0, "Photos")
	a := s.upload(userID, docs.ID, "a.txt", "a")
	inner := s.upload(userID, photos.ID, "in.txt", "i")
	s.Require().NoError(svc.Move(context.Background(), userID, photos.ID, []int64{a.ID}))
	s.Require().NoError(svc.Move(context.Background(), userID, 0, []int64{a.ID}))
	s.Require().NoError(svc.Copy(context.Background(), userID, docs.ID, []int64{photos.ID}))
	s.workCopyJobs()

	s.Equal([]map[string]any{
		{"event": "moved", "name": "a.txt", "from": "Photos", "folder": nil},
		{"event": "moved", "name": "a.txt", "from": "Docs", "folder": "Photos"},
		{"event": "uploaded", "name": "a.txt", "folder": "Docs"},
	}, s.activity(userID, a.ID))
	copyID := s.childID(userID, docs.ID, "Photos")
	s.Equal([]map[string]any{{"event": "copied", "name": "Photos", "folder": "Docs"}}, s.activity(userID, copyID))
	s.Empty(s.activity(userID, s.childID(userID, copyID, "in.txt")))
	s.Len(s.activity(userID, photos.ID), 1)
	s.Len(s.activity(userID, inner.ID), 1)
}

// Trash, restore and purge log the items the user picked, not the rows under them. A restore logs the name and folder
// the item came back with. The log stays after the purge.
func (s *FileServiceTestSuite) TestActivity_TrashRestoreAndPurge() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	inner := s.upload(userID, docs.ID, "in.txt", "i")
	a := s.upload(userID, docs.ID, "a.txt", "a")
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{a.ID}))
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{docs.ID}))
	s.workTrashJobs()
	s.upload(userID, 0, "a.txt", "taken")
	// Docs is still in the trash, so a.txt comes back to Home, where its name is taken.
	s.Require().NoError(svc.Restore(context.Background(), userID, []int64{a.ID}))
	s.Require().NoError(svc.Purge(context.Background(), userID, []int64{docs.ID}))
	s.workPurgeJobs()

	s.Equal([]map[string]any{
		{"event": "restored", "name": "a (1).txt", "folder": nil},
		{"event": "trashed", "name": "a.txt", "folder": "Docs"},
		{"event": "uploaded", "name": "a.txt", "folder": "Docs"},
	}, s.activity(userID, a.ID))
	s.Equal([]map[string]any{
		{"event": "purged", "name": "Docs", "folder": nil},
		{"event": "trashed", "name": "Docs", "folder": nil},
		{"event": "created", "name": "Docs", "folder": nil},
	}, s.activity(userID, docs.ID))
	s.Len(s.activity(userID, inner.ID), 1)
}

// The trash sweep logs its purge as "expired", so the log tells it from a purge the user asked for.
func (s *FileServiceTestSuite) TestActivity_ExpiredBySweep() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	a := s.upload(userID, 0, "a.txt", "a")
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{a.ID}))
	s.moveTrashedAt(a.ID, 31*24)

	s.Require().NoError(svc.SweepTrash(context.Background()))

	s.Equal("expired", s.activity(userID, a.ID)[0]["event"])
}

// A change that fails writes no audit row: the row is in the same transaction.
func (s *FileServiceTestSuite) TestActivity_FailedChangeLogsNothing() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	s.upload(userID, docs.ID, "a.txt", "a")
	a := s.upload(userID, 0, "a.txt", "a")
	b := s.upload(userID, 0, "b.txt", "b")

	s.Equal("a.txt already exists in this folder", s.errorMessage(svc.Move(context.Background(), userID, docs.ID, []int64{a.ID})))
	_, err := svc.Rename(context.Background(), userID, b.ID, "a.txt")
	s.Equal("an item with this name already exists", s.errorMessage(err))
	s.Equal("item not found", s.errorMessage(svc.Trash(context.Background(), userID, []int64{a.ID, 999})))

	s.Len(s.activity(userID, a.ID), 1)
	s.Len(s.activity(userID, b.ID), 1)
}

// The log comes newest first, in pages. Another user's id gives an empty page.
func (s *FileServiceTestSuite) TestListActivity_Pages() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	a := s.upload(alice, 0, "a0.txt", "a")
	for i := 1; i <= 11; i++ {
		_, err := svc.Rename(context.Background(), alice, a.ID, fmt.Sprintf("a%d.txt", i))
		s.Require().NoError(err)
	}

	var names []string
	cursor := ""
	pages := 0
	for {
		page, err := svc.ListActivity(context.Background(), alice, a.ID, file.ActivityQuery{Cursor: cursor, PageSize: 5})
		s.Require().NoError(err)
		pages++
		for _, entry := range page.Data {
			var meta map[string]any
			s.Require().NoError(json.Unmarshal(entry.Metadata, &meta))
			names = append(names, meta["name"].(string))
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	s.Equal(3, pages)
	s.Len(names, 12)
	s.Equal("a11.txt", names[0])
	s.Equal("a0.txt", names[11])

	page, err := svc.ListActivity(context.Background(), bob, a.ID, file.ActivityQuery{})
	s.Require().NoError(err)
	s.Empty(page.Data)

	_, err = svc.ListActivity(context.Background(), alice, a.ID, file.ActivityQuery{Cursor: "bad"})
	s.Equal("invalid cursor", s.errorMessage(err))
}

func (s *FileServiceTestSuite) TestGetItem() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	docs := s.createFolder(alice, 0, "Docs")
	a := s.upload(alice, docs.ID, "a.txt", "alpha")
	trashed := s.upload(alice, 0, "trashed.txt", "t")
	s.Require().NoError(svc.Trash(context.Background(), alice, []int64{trashed.ID}))
	s.Require().NoError(svc.AddFavorites(context.Background(), alice, []int64{a.ID}))

	got, err := svc.GetItem(context.Background(), alice, a.ID)

	s.Require().NoError(err)
	s.Equal("a.txt", got.Name)
	s.Equal(&docs.ID, got.ParentID)
	s.Equal(int64(5), got.SizeBytes)
	s.True(got.IsFavorite)
	s.False(got.UpdatedAt.IsZero())

	for _, check := range []struct{ user, id int64 }{{bob, a.ID}, {alice, trashed.ID}, {alice, 999}} {
		_, err := svc.GetItem(context.Background(), check.user, check.id)
		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusNotFound, status, check)
		s.Equal("item not found", msg)
	}
}

// A hidden item leaves the list and the search, unless the list asks for hidden items. Unhide brings it back. Each
// change writes one audit row, and a second hide changes nothing.
func (s *FileServiceTestSuite) TestSetHidden_HidesAndShows() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	a := s.upload(userID, 0, "a.txt", "a")
	s.upload(userID, 0, "b.txt", "b")

	s.Require().NoError(svc.SetHidden(context.Background(), userID, []int64{docs.ID, a.ID}, true))
	s.Require().NoError(svc.SetHidden(context.Background(), userID, []int64{a.ID}, true))

	shown, err := svc.List(context.Background(), userID, newestFirst(0))
	s.Require().NoError(err)
	s.Equal([]string{"b.txt"}, fileNames(shown.Data))
	q := newestFirst(0)
	q.Search = "a."
	found, err := svc.List(context.Background(), userID, q)
	s.Require().NoError(err)
	s.Empty(found.Data)
	q = newestFirst(0)
	q.ShowHidden = true
	all, err := svc.List(context.Background(), userID, q)
	s.Require().NoError(err)
	s.Equal(map[string]bool{"Docs": true, "a.txt": true, "b.txt": false}, hiddenMarks(all.Data))

	s.Require().NoError(svc.SetHidden(context.Background(), userID, []int64{a.ID}, false))

	shown, err = svc.List(context.Background(), userID, newestFirst(0))
	s.Require().NoError(err)
	s.Equal([]string{"b.txt", "a.txt"}, fileNames(shown.Data))
	s.Equal([]map[string]any{
		{"event": "unhidden", "name": "a.txt", "folder": nil},
		{"event": "hidden", "name": "a.txt", "folder": nil},
		{"event": "uploaded", "name": "a.txt", "folder": nil},
	}, s.activity(userID, a.ID))
}

// One bad id undoes the rest: an unknown id, another user's item, or an item in the trash.
func (s *FileServiceTestSuite) TestSetHidden_AllOrNone() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	a := s.upload(alice, 0, "a.txt", "a")
	trashed := s.upload(alice, 0, "trashed.txt", "t")
	s.Require().NoError(svc.Trash(context.Background(), alice, []int64{trashed.ID}))
	bobFile := s.upload(bob, 0, "bob.txt", "b")

	for _, bad := range []int64{999, bobFile.ID, trashed.ID} {
		err := svc.SetHidden(context.Background(), alice, []int64{a.ID, bad}, true)

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusNotFound, status, bad)
		s.Equal("item not found", msg)
	}
	shown, err := svc.List(context.Background(), alice, newestFirst(0))
	s.Require().NoError(err)
	s.Equal([]string{"a.txt"}, fileNames(shown.Data))
	s.Len(s.activity(alice, a.ID), 1)
}

// A zip keeps hidden items, and a copy keeps the hidden mark, for the pasted item and the rows under it.
func (s *FileServiceTestSuite) TestSetHidden_ZipAndCopyKeepHiddenItems() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	other := s.createFolder(userID, 0, "Other")
	h := s.upload(userID, docs.ID, "h.txt", "hidden")
	s.upload(userID, docs.ID, "v.txt", "shown")
	secret := s.upload(userID, 0, "secret.txt", "s")
	s.Require().NoError(svc.SetHidden(context.Background(), userID, []int64{h.ID, secret.ID}, true))

	dl, err := svc.RequestZip(context.Background(), userID, []int64{docs.ID})
	s.Require().NoError(err)
	s.workZipJobs()
	s.Equal([]string{"Docs/", "Docs/h.txt=hidden", "Docs/v.txt=shown"}, s.zipEntries(userID, dl.ID))

	s.Require().NoError(svc.Copy(context.Background(), userID, other.ID, []int64{docs.ID, secret.ID}))
	s.workCopyJobs()
	q := newestFirst(other.ID)
	q.ShowHidden = true
	copies, err := svc.List(context.Background(), userID, q)
	s.Require().NoError(err)
	s.Equal(map[string]bool{"Docs": false, "secret.txt": true}, hiddenMarks(copies.Data))
	q = newestFirst(s.childID(userID, other.ID, "Docs"))
	q.ShowHidden = true
	inner, err := svc.List(context.Background(), userID, q)
	s.Require().NoError(err)
	s.Equal(map[string]bool{"h.txt": true, "v.txt": false}, hiddenMarks(inner.Data))
}

func (s *FileServiceTestSuite) TestOpenFile_Success() {
	userID := s.seedUser()
	docs := s.createFolder(userID, 0, "Docs")
	f := s.upload(userID, docs.ID, "notes.txt", "hello world")

	content, err := s.TC.Services.FileService.OpenFile(context.Background(), userID, f.ID)

	s.Require().NoError(err)
	defer func() { _ = content.Content.Close() }()
	s.Equal("notes.txt", content.Name)
	s.Equal("text/plain; charset=utf-8", content.ContentType)
	s.Equal(`"`+s.storageKey(f.ID)+`"`, content.ETag)
	s.WithinDuration(f.CreatedAt, content.ModTime, time.Second)
	body, err := io.ReadAll(content.Content)
	s.Require().NoError(err)
	s.Equal("hello world", string(body))
}

// Another user's file and a file in the trash look the same as an unknown id.
func (s *FileServiceTestSuite) TestOpenFile_NotFound() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	bobFile := s.upload(bob, 0, "bob.txt", "bob")
	trashed := s.upload(alice, 0, "old.txt", "old")
	s.Require().NoError(svc.Trash(context.Background(), alice, []int64{trashed.ID}))

	for _, id := range []int64{999, bobFile.ID, trashed.ID} {
		_, err := svc.OpenFile(context.Background(), alice, id)

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusNotFound, status, id)
		s.Equal("item not found", msg)
	}
}

func (s *FileServiceTestSuite) TestOpenFile_Folder() {
	userID := s.seedUser()
	docs := s.createFolder(userID, 0, "Docs")

	_, err := s.TC.Services.FileService.OpenFile(context.Background(), userID, docs.ID)

	status, msg := apperr.Resolve(err)
	s.Equal(http.StatusBadRequest, status)
	s.Equal("a folder downloads as a zip", msg)
}

// A copy root has no bytes until the job writes them. After the job, it downloads like any file.
func (s *FileServiceTestSuite) TestOpenFile_Copying() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	archive := s.createFolder(userID, 0, "Archive")
	f := s.upload(userID, 0, "notes.txt", "hello world")
	s.Require().NoError(svc.Copy(context.Background(), userID, archive.ID, []int64{f.ID}))
	copyID := s.copiesOf(f.ID)[0]

	_, err := svc.OpenFile(context.Background(), userID, copyID)

	status, msg := apperr.Resolve(err)
	s.Equal(http.StatusConflict, status)
	s.Equal("notes.txt is still copying", msg)

	s.workCopyJobs()
	content, err := svc.OpenFile(context.Background(), userID, copyID)
	s.Require().NoError(err)
	defer func() { _ = content.Content.Close() }()
	body, err := io.ReadAll(content.Content)
	s.Require().NoError(err)
	s.Equal("hello world", string(body))
}

func (s *FileServiceTestSuite) TestOpenFile_NoBytes() {
	userID := s.seedUser()
	id := s.seedFile(userID, "lost.txt")

	_, err := s.TC.Services.FileService.OpenFile(context.Background(), userID, id)

	status, msg := apperr.Resolve(err)
	s.Equal(http.StatusNotFound, status)
	s.Equal("file not found on disk", msg)
	s.Len(s.logs("a file has no bytes on disk"), 1)
}

// A picture gets a thumbnail with a long edge of 256 pixels. The second request reads the one on disk.
func (s *FileServiceTestSuite) TestOpenThumbnail_MakesAndKeeps() {
	userID := s.seedUser()
	f := s.upload(userID, 0, "wide.png", s.encodePNG(testPicture(600, 300, 255)))
	key := s.storageKey(f.ID)

	img, content := s.thumbnail(userID, f.ID)

	s.Equal("wide.png", content.Name)
	s.Equal("image/jpeg", content.ContentType)
	s.Equal(`"`+key+`-thumb"`, content.ETag)
	s.Equal(image.Rect(0, 0, 256, 128), img.Bounds())
	s.Equal("green", colorName(img.At(20, 20)))
	s.Equal("red", colorName(img.At(20, 108)))
	s.Equal("blue", colorName(img.At(236, 64)))

	path := filepath.Join(s.thumbDir(), key)
	past := time.Now().Add(-time.Hour)
	s.Require().NoError(os.Chtimes(path, past, past))
	s.thumbnail(userID, f.ID)
	info, err := os.Stat(path)
	s.Require().NoError(err)
	s.WithinDuration(past, info.ModTime(), time.Second)
}

// A picture with transparency keeps it in a PNG. A small picture keeps its size.
func (s *FileServiceTestSuite) TestOpenThumbnail_KeepsTransparency() {
	userID := s.seedUser()
	f := s.upload(userID, 0, "logo.png", s.encodePNG(testPicture(40, 20, 128)))

	img, content := s.thumbnail(userID, f.ID)

	s.Equal("image/png", content.ContentType)
	s.Equal(image.Rect(0, 0, 40, 20), img.Bounds())
	_, _, _, alpha := img.At(35, 10).RGBA()
	s.InDelta(0x8080, alpha, 0x200)
}

// A JPEG turns by its EXIF orientation, the same as the browser shows it. The green corner of the picture is at the top
// left before the turn, so its place tells each of the eight orientations apart.
func (s *FileServiceTestSuite) TestOpenThumbnail_TurnsByExif() {
	userID := s.seedUser()
	wide, tall := image.Rect(0, 0, 60, 30), image.Rect(0, 0, 30, 60)
	cases := []struct {
		orientation uint16
		bounds      image.Rectangle
		green       image.Point
	}{
		{1, wide, image.Pt(5, 5)},
		{2, wide, image.Pt(55, 5)},
		{3, wide, image.Pt(55, 25)},
		{4, wide, image.Pt(5, 25)},
		{5, tall, image.Pt(5, 5)},
		{6, tall, image.Pt(25, 5)},
		{7, tall, image.Pt(25, 55)},
		{8, tall, image.Pt(5, 55)},
	}
	for _, tc := range cases {
		name := fmt.Sprintf("turn-%d.jpg", tc.orientation)
		f := s.upload(userID, 0, name, s.encodeJPEG(testPicture(60, 30, 255), tc.orientation))

		img, content := s.thumbnail(userID, f.ID)

		s.Equal("image/jpeg", content.ContentType, name)
		s.Equal(tc.bounds, img.Bounds(), name)
		s.Equal("green", colorName(img.At(tc.green.X, tc.green.Y)), name)
	}
}

// A folder, a file that is not a picture, a copy that has no bytes yet, a broken picture and a picture over the pixel
// cap have no thumbnail. Only the two that fail to decode are logged.
func (s *FileServiceTestSuite) TestOpenThumbnail_NoThumbnail() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	archive := s.createFolder(userID, 0, "Archive")
	text := s.upload(userID, 0, "notes.txt", "hello")
	broken := s.upload(userID, 0, "broken.png", "\x89PNG\r\n\x1a\nnot a picture")
	huge := s.upload(userID, 0, "huge.png", s.hugePNG())
	picture := s.upload(userID, 0, "picture.png", s.encodePNG(testPicture(4, 4, 255)))
	s.Require().NoError(svc.Copy(context.Background(), userID, archive.ID, []int64{picture.ID}))
	copying := s.copiesOf(picture.ID)[0]

	for _, id := range []int64{archive.ID, text.ID, copying, broken.ID, huge.ID} {
		_, err := svc.OpenThumbnail(context.Background(), userID, id)

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusNotFound, status, id)
		s.Equal("no thumbnail for this file", msg, id)
	}
	s.Len(s.logs("a picture has no thumbnail"), 2)
	for _, id := range []int64{text.ID, broken.ID, huge.ID} {
		s.NoFileExists(filepath.Join(s.thumbDir(), s.storageKey(id)), id)
	}
}

// Another user's picture and a picture in the trash look the same as an unknown id.
func (s *FileServiceTestSuite) TestOpenThumbnail_NotFound() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	picture := s.encodePNG(testPicture(4, 4, 255))
	bobPicture := s.upload(bob, 0, "bob.png", picture)
	trashed := s.upload(alice, 0, "old.png", picture)
	s.Require().NoError(svc.Trash(context.Background(), alice, []int64{trashed.ID}))

	for _, id := range []int64{999, bobPicture.ID, trashed.ID} {
		_, err := svc.OpenThumbnail(context.Background(), alice, id)

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusNotFound, status, id)
		s.Equal("item not found", msg)
	}
}

func (s *FileServiceTestSuite) TestOpenThumbnail_NoBytes() {
	userID := s.seedUser()
	id := s.seedFileWith(userID, "lost.png", "image/png", 1, 0)

	_, err := s.TC.Services.FileService.OpenThumbnail(context.Background(), userID, id)

	status, msg := apperr.Resolve(err)
	s.Equal(http.StatusNotFound, status)
	s.Equal("file not found on disk", msg)
	s.Len(s.logs("a file has no bytes on disk"), 1)
}

func (s *FileServiceTestSuite) TestRequestZip_AddsDownloadAndJob() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")

	dl, err := svc.RequestZip(context.Background(), userID, []int64{docs.ID, docs.ID})

	s.Require().NoError(err)
	s.Equal("Docs.zip", dl.Name)
	s.Equal(file.DownloadBuilding, dl.State)
	s.Equal([]jobs.BuildZipArgs{{UserID: userID, DownloadID: dl.ID}}, s.zipJobs())
	var queue string
	s.Require().NoError(s.TC.DB.QueryRowContext(context.Background(),
		`SELECT queue FROM river_job WHERE kind = 'build_zip'`).Scan(&queue))
	s.Equal(jobs.QueueDownloads, queue)

	got := s.download(userID, dl.ID)
	s.Equal(dl, got)
}

func (s *FileServiceTestSuite) TestRequestZip_Names() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	other := s.createFolder(userID, 0, "Other")
	a := s.upload(userID, docs.ID, "a.txt", "a")
	b := s.upload(userID, docs.ID, "b.txt", "b")
	c := s.upload(userID, other.ID, "c.txt", "c")
	home := s.upload(userID, 0, "home.txt", "h")

	cases := []struct {
		ids  []int64
		name string
	}{
		{[]int64{docs.ID}, "Docs.zip"},
		{[]int64{a.ID}, "a.txt.zip"},
		{[]int64{a.ID, b.ID}, "Docs.zip"},
		{[]int64{docs.ID, home.ID}, "Home.zip"},
		{[]int64{a.ID, c.ID}, "Download.zip"},
	}
	for _, tc := range cases {
		dl, err := svc.RequestZip(context.Background(), userID, tc.ids)

		s.Require().NoError(err)
		s.Equal(tc.name, dl.Name, tc.ids)
		s.endDownload(dl.ID)
	}
}

// Another user's item and an item in the trash look the same as an unknown id. Nothing is added.
func (s *FileServiceTestSuite) TestRequestZip_NotFound() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	mine := s.upload(alice, 0, "mine.txt", "m")
	bobFile := s.upload(bob, 0, "bob.txt", "b")
	trashed := s.upload(alice, 0, "old.txt", "o")
	s.Require().NoError(svc.Trash(context.Background(), alice, []int64{trashed.ID}))

	for _, id := range []int64{999, bobFile.ID, trashed.ID} {
		_, err := svc.RequestZip(context.Background(), alice, []int64{mine.ID, id})

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusNotFound, status, id)
		s.Equal("item not found", msg)
	}
	s.Equal(0, s.downloadRows())
	s.Empty(s.zipJobs())
}

func (s *FileServiceTestSuite) TestRequestZip_Copying() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	archive := s.createFolder(userID, 0, "Archive")
	s.Require().NoError(svc.Copy(context.Background(), userID, archive.ID, []int64{docs.ID}))

	_, err := svc.RequestZip(context.Background(), userID, []int64{s.copiesOf(docs.ID)[0]})

	status, msg := apperr.Resolve(err)
	s.Equal(http.StatusConflict, status)
	s.Equal("Docs is still copying", msg)
	s.Equal(0, s.downloadRows())
}

// A ready or failed zip no longer counts. Each user has their own limit.
func (s *FileServiceTestSuite) TestRequestZip_LimitPerUser() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	aliceDocs := s.createFolder(alice, 0, "Docs")
	bobDocs := s.createFolder(bob, 0, "Docs")
	var first int64
	for i := range 3 {
		dl, err := svc.RequestZip(context.Background(), alice, []int64{aliceDocs.ID})
		s.Require().NoError(err)
		if i == 0 {
			first = dl.ID
		}
	}

	_, err := svc.RequestZip(context.Background(), alice, []int64{aliceDocs.ID})

	status, msg := apperr.Resolve(err)
	s.Equal(http.StatusConflict, status)
	s.Equal("wait for your other downloads to finish", msg)

	_, err = svc.RequestZip(context.Background(), bob, []int64{bobDocs.ID})
	s.Require().NoError(err)

	s.Require().NoError(svc.FailZip(context.Background(), alice, first))
	_, err = svc.RequestZip(context.Background(), alice, []int64{aliceDocs.ID})
	s.Require().NoError(err)
}

// Many requests at once wait for each other on the user lock, so only the limit of them gets in.
func (s *FileServiceTestSuite) TestRequestZip_LimitHoldsUnderRace() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")

	errs := make([]error, 10)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() { _, errs[i] = svc.RequestZip(context.Background(), userID, []int64{docs.ID}) })
	}
	wg.Wait()

	var passed int
	for _, err := range errs {
		if err == nil {
			passed++
			continue
		}
		s.Equal("wait for your other downloads to finish", s.errorMessage(err))
	}
	s.Equal(3, passed)
	s.Equal(3, s.downloadRows())
}

func (s *FileServiceTestSuite) TestBuildZip_FoldersAndFiles() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	sub := s.createFolder(userID, docs.ID, "Sub")
	s.createFolder(userID, docs.ID, "Empty")
	s.upload(userID, docs.ID, "a.txt", "alpha")
	s.upload(userID, sub.ID, "b.txt", "beta")
	c := s.upload(userID, 0, "c.txt", "gamma")
	s.upload(userID, 0, "left-out.txt", "x")
	dl, err := svc.RequestZip(context.Background(), userID, []int64{docs.ID, c.ID})
	s.Require().NoError(err)

	s.workZipJobs()

	got := s.download(userID, dl.ID)
	s.Equal(file.DownloadReady, got.State)
	s.Equal([]string{
		"Docs/", "Docs/Empty/", "Docs/Sub/", "Docs/Sub/b.txt=beta", "Docs/a.txt=alpha", "c.txt=gamma",
	}, s.zipEntries(userID, dl.ID))
	s.Len(s.zipFiles(), 1)
	s.Equal([]map[string]any{{
		"user_id": userID, "download_id": dl.ID, "files": int64(3), "folders": int64(3), "bytes": got.SizeBytes,
	}}, s.logs("built a zip"))
}

// Each folder reads in pages, so a folder with more rows than a page, and a level with more folders than a page,
// still go in whole.
func (s *FileServiceTestSuite) TestBuildZip_ManyPagesAndLevels() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	big := s.createFolder(userID, 0, "Big")
	for _, insert := range []string{
		`INSERT INTO files (user_id, parent_id, name, storage_key, content_type, size_bytes)
		SELECT $1, $2, 'f' || i || '.txt', md5(random()::text), 'text/plain', 1 FROM generate_series(1, 260) i`,
		`INSERT INTO files (user_id, parent_id, name, content_type, size_bytes)
		SELECT $1, $2, 'Sub ' || i, '', 0 FROM generate_series(1, 120) i`,
		`INSERT INTO files (user_id, parent_id, name, storage_key, content_type, size_bytes)
		SELECT user_id, id, 'in.txt', md5(random()::text), 'text/plain', 1 FROM files
		WHERE user_id = $1 AND parent_id = $2 AND storage_key IS NULL`,
	} {
		_, err := s.TC.DB.ExecContext(context.Background(), insert, userID, big.ID)
		s.Require().NoError(err)
	}
	s.writeSeededBytes(userID)
	dl, err := svc.RequestZip(context.Background(), userID, []int64{big.ID})
	s.Require().NoError(err)

	s.workZipJobs()

	entries := s.zipEntries(userID, dl.ID)
	s.Len(entries, 1+260+120+120)
	s.Contains(entries, "Big/f260.txt=f260.txt")
	s.Contains(entries, "Big/Sub 120/in.txt=in.txt")
	s.Equal(int64(380), s.logs("built a zip")[0]["files"])
	s.Equal(int64(121), s.logs("built a zip")[0]["folders"])
}

// A copy that is not done yet is left out with all under it. A file with no bytes on disk is logged and left out.
func (s *FileServiceTestSuite) TestBuildZip_LeavesOutCopyingAndLostFiles() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	s.upload(userID, docs.ID, "a.txt", "alpha")
	photos := s.createFolder(userID, 0, "Photos")
	s.upload(userID, photos.ID, "p.jpg", "p")
	s.Require().NoError(svc.Copy(context.Background(), userID, docs.ID, []int64{photos.ID}))
	lost := s.seedFile(userID, "lost.txt")
	s.Require().NoError(svc.Move(context.Background(), userID, docs.ID, []int64{lost}))
	dl, err := svc.RequestZip(context.Background(), userID, []int64{docs.ID})
	s.Require().NoError(err)

	s.workZipJobs()

	s.Equal([]string{"Docs/", "Docs/a.txt=alpha"}, s.zipEntries(userID, dl.ID))
	s.Equal([]map[string]any{{"user_id": userID, "file_id": lost}}, s.logs("left a file with no bytes out of a zip"))
}

// Names in one folder are unique, but items from two folders can share one. The second gets a free name.
func (s *FileServiceTestSuite) TestBuildZip_SameNameFromTwoFolders() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	other := s.createFolder(userID, 0, "Other")
	first := s.upload(userID, docs.ID, "a.txt", "one")
	second := s.upload(userID, other.ID, "a.txt", "two")
	dl, err := svc.RequestZip(context.Background(), userID, []int64{first.ID, second.ID})
	s.Require().NoError(err)

	s.workZipJobs()

	s.Equal("Download.zip", dl.Name)
	s.Equal([]string{"a (1).txt=two", "a.txt=one"}, s.zipEntries(userID, dl.ID))
}

// The test cap is 1 MB. The zip fails at once, because another run would stop at the same place.
func (s *FileServiceTestSuite) TestBuildZip_OverTheSizeCap() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	for _, name := range []string{"a.bin", "b.bin"} {
		_, err := s.finishUpload(userID, docs.ID, name, strings.Repeat("x", 600<<10))
		s.Require().NoError(err)
	}
	dl, err := svc.RequestZip(context.Background(), userID, []int64{docs.ID})
	s.Require().NoError(err)

	s.workZipJobs()

	got := s.download(userID, dl.ID)
	s.Equal(file.DownloadFailed, got.State)
	s.Equal("the zip is larger than the 1 MB limit", got.Error)
	s.Empty(s.zipFiles())
	s.Len(s.logs("stopped a zip over the size cap"), 1)
}

// River gives up after the last attempt, so the worker marks the zip failed and the client stops waiting.
// The run context is over by then, and the mark still goes in.
func (s *FileServiceTestSuite) TestBuildZip_FailsAfterLastAttempt() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	dl, err := svc.RequestZip(context.Background(), userID, []int64{docs.ID})
	s.Require().NoError(err)
	worker := jobs.NewBuildZipWorker(svc)
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	run := func(attempt int) error {
		return worker.Work(stopped, &river.Job[jobs.BuildZipArgs]{
			JobRow: &rivertype.JobRow{Attempt: attempt, MaxAttempts: 3},
			Args:   jobs.BuildZipArgs{UserID: userID, DownloadID: dl.ID},
		})
	}

	s.Require().Error(run(2))
	got := s.download(userID, dl.ID)
	s.Equal(file.DownloadBuilding, got.State)

	s.Require().Error(run(3))
	got = s.download(userID, dl.ID)
	s.Equal(file.DownloadFailed, got.State)
	s.Equal("the zip could not be built", got.Error)
}

// A failed or deleted download gets no zip.
func (s *FileServiceTestSuite) TestBuildZip_SkipsFailedAndGoneDownloads() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	failed, err := svc.RequestZip(context.Background(), userID, []int64{docs.ID})
	s.Require().NoError(err)
	gone, err := svc.RequestZip(context.Background(), userID, []int64{docs.ID})
	s.Require().NoError(err)
	s.Require().NoError(svc.FailZip(context.Background(), userID, failed.ID))
	_, err = s.TC.DB.ExecContext(context.Background(), `DELETE FROM downloads WHERE id = $1`, gone.ID)
	s.Require().NoError(err)

	s.workZipJobs()

	got := s.download(userID, failed.ID)
	s.Equal(file.DownloadFailed, got.State)
	s.Equal("the zip could not be built", got.Error)
	s.Empty(s.zipFiles())
	s.Empty(s.logs("built a zip"))
}

func (s *FileServiceTestSuite) TestOpenZip() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	docs := s.createFolder(alice, 0, "Docs")
	s.upload(alice, docs.ID, "a.txt", "alpha")
	dl, err := svc.RequestZip(context.Background(), alice, []int64{docs.ID})
	s.Require().NoError(err)

	_, err = svc.OpenZip(context.Background(), alice, dl.ID)
	status, msg := apperr.Resolve(err)
	s.Equal(http.StatusConflict, status)
	s.Equal("the zip is not ready yet", msg)

	s.workZipJobs()
	got := s.download(alice, dl.ID)
	s.False(got.Downloaded)

	content, err := svc.OpenZip(context.Background(), alice, dl.ID)
	s.Require().NoError(err)
	s.Require().NoError(content.Content.Close())
	key := s.zipFiles()[0]
	s.Equal("Docs.zip", content.Name)
	s.Equal("application/zip", content.ContentType)
	s.Equal(`"`+key+`"`, content.ETag)
	got = s.download(alice, dl.ID)
	s.True(got.Downloaded)

	for _, id := range []int64{dl.ID, 999} {
		_, err = svc.OpenZip(context.Background(), bob, id)
		status, msg = apperr.Resolve(err)
		s.Equal(http.StatusNotFound, status, id)
		s.Equal("download not found", msg)
	}

	s.Require().NoError(os.Remove(filepath.Join(s.TC.UploadDir, utils.ZipDir, key)))
	_, err = svc.OpenZip(context.Background(), alice, dl.ID)
	status, msg = apperr.Resolve(err)
	s.Equal(http.StatusNotFound, status)
	s.Equal("file not found on disk", msg)
	s.Len(s.logs("a zip has no bytes on disk"), 1)
}

// The list holds the user's own downloads, newest first. A ready zip expires a day after it got ready, any other row
// a day after the request.
func (s *FileServiceTestSuite) TestListDownloads() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	docs := s.createFolder(alice, 0, "Docs")
	s.upload(alice, docs.ID, "a.txt", "alpha")
	ready, err := svc.RequestZip(context.Background(), alice, []int64{docs.ID})
	s.Require().NoError(err)
	s.workZipJobs()
	building, err := svc.RequestZip(context.Background(), alice, []int64{docs.ID})
	s.Require().NoError(err)
	s.ageDownload(ready.ID, 5, 3)
	s.ageDownload(building.ID, 2, 0)
	_, err = svc.RequestZip(context.Background(), bob, []int64{s.createFolder(bob, 0, "Bob").ID})
	s.Require().NoError(err)

	list, err := svc.ListDownloads(context.Background(), alice)

	s.Require().NoError(err)
	s.Require().Len(list, 2)
	s.Equal(building.ID, list[0].ID)
	s.Equal(file.DownloadBuilding, list[0].State)
	s.WithinDuration(list[0].CreatedAt.Add(24*time.Hour), list[0].ExpiresAt, 0)
	s.Equal(ready.ID, list[1].ID)
	s.Equal(file.DownloadReady, list[1].State)
	s.Equal("Docs.zip", list[1].Name)
	s.Positive(list[1].SizeBytes)
	var readyAt time.Time
	s.Require().NoError(s.TC.DB.QueryRowContext(context.Background(),
		`SELECT ready_at FROM downloads WHERE id = $1`, ready.ID).Scan(&readyAt))
	s.WithinDuration(readyAt.Add(24*time.Hour), list[1].ExpiresAt, 0)
	s.False(list[1].Downloaded)
}

// The list has no pages. It shows the 50 newest rows.
func (s *FileServiceTestSuite) TestListDownloads_Newest50() {
	userID := s.seedUser()
	_, err := s.TC.DB.ExecContext(context.Background(), `
		INSERT INTO downloads (user_id, name, item_ids, error)
		SELECT $1, 'Docs ' || i || '.zip', '{1}', 'failed' FROM generate_series(1, 55) i`, userID)
	s.Require().NoError(err)

	list, err := s.TC.Services.FileService.ListDownloads(context.Background(), userID)

	s.Require().NoError(err)
	s.Require().Len(list, 50)
	s.Equal("Docs 55.zip", list[0].Name)
	s.Equal("Docs 6.zip", list[49].Name)
}

// Remove deletes the row and its zip. A building row has no zip yet, and its place under the limit frees at once.
func (s *FileServiceTestSuite) TestRemoveDownload() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	docs := s.createFolder(alice, 0, "Docs")
	s.upload(alice, docs.ID, "a.txt", "alpha")
	request := func() int64 {
		dl, err := svc.RequestZip(context.Background(), alice, []int64{docs.ID})
		s.Require().NoError(err)
		return dl.ID
	}
	ready := request()
	s.workZipJobs()
	building := []int64{request(), request(), request()}

	for _, id := range []int64{ready, 999} {
		err := svc.RemoveDownload(context.Background(), bob, id)
		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusNotFound, status, id)
		s.Equal("download not found", msg)
	}
	s.Len(s.zipFiles(), 1)

	s.Require().NoError(svc.RemoveDownload(context.Background(), alice, ready))
	s.Require().NoError(svc.RemoveDownload(context.Background(), alice, building[0]))

	s.Equal(building[1:], s.downloadIDs())
	s.Empty(s.zipFiles())
	s.Equal("download not found", s.errorMessage(svc.RemoveDownload(context.Background(), alice, ready)))
	request()
}

// A zip that does not go is logged. Its row is gone, so the sweep removes the zip later.
func (s *FileServiceTestSuite) TestRemoveDownload_ZipThatDoesNotGo() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	s.upload(userID, docs.ID, "a.txt", "alpha")
	dl, err := svc.RequestZip(context.Background(), userID, []int64{docs.ID})
	s.Require().NoError(err)
	s.workZipJobs()
	// A dir that is not empty cannot go with a remove.
	path := filepath.Join(s.TC.UploadDir, utils.ZipDir, s.zipFiles()[0])
	s.Require().NoError(os.Remove(path))
	s.Require().NoError(os.Mkdir(path, 0o750))
	s.Require().NoError(os.WriteFile(filepath.Join(path, "x"), []byte("x"), 0o640))

	s.Require().NoError(svc.RemoveDownload(context.Background(), userID, dl.ID))

	s.Empty(s.downloadIDs())
	logs := s.logs("a removed download kept its zip")
	s.Require().Len(logs, 1)
	s.Equal(userID, logs[0]["user_id"])
	s.Equal(dl.ID, logs[0]["download_id"])
}

// The user removes a download while its zip builds. The tests check the row before each entry and each chunk, so the
// job stops at its next check and keeps no zip. A named pipe holds the job inside a file until the row is gone.
func (s *FileServiceTestSuite) TestBuildZip_StopsWhenRemoved() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	slow := s.upload(userID, docs.ID, "slow.bin", "x")
	pipe := filepath.Join(s.TC.UploadDir, s.storageKey(slow.ID))
	s.Require().NoError(os.Remove(pipe))
	s.Require().NoError(syscall.Mkfifo(pipe, 0o640))
	s.T().Cleanup(func() {
		_ = os.Remove(pipe)
		_ = os.RemoveAll(filepath.Join(s.TC.UploadDir, utils.ZipDir))
	})
	dl, err := svc.RequestZip(context.Background(), userID, []int64{docs.ID})
	s.Require().NoError(err)

	result := make(chan error, 1)
	go func() {
		result <- jobs.NewBuildZipWorker(svc).Work(context.Background(), &river.Job[jobs.BuildZipArgs]{
			JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 3},
			Args:   jobs.BuildZipArgs{UserID: userID, DownloadID: dl.ID},
		})
	}()
	// The open returns once the job opened the pipe to read it.
	writer, err := os.OpenFile(pipe, os.O_WRONLY, 0)
	s.Require().NoError(err)
	defer func() { _ = writer.Close() }()
	s.Require().NoError(svc.RemoveDownload(context.Background(), userID, dl.ID))
	// The job reads again after the write. If its check already saw the remove, it closed the pipe and the write fails.
	_, _ = writer.Write([]byte("x"))
	s.Require().NoError(writer.Close())

	s.Require().NoError(<-result)
	s.Empty(s.zipFiles())
	s.Equal([]map[string]any{{"user_id": userID, "download_id": dl.ID}}, s.logs("stopped a zip whose download was removed"))
	s.Empty(s.logs("built a zip"))
}

// A folder takes the rows under it along. Only the moved items get a new updated_at.
func (s *FileServiceTestSuite) TestMove_FilesAndFolders() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	photos := s.createFolder(userID, 0, "Photos")
	cars := s.createFolder(userID, photos.ID, "Cars")
	archive := s.createFolder(userID, 0, "Archive")
	fileID := s.seedFile(userID, "a.txt")
	_, err := s.TC.DB.ExecContext(context.Background(), `UPDATE files SET updated_at = $1 WHERE user_id = $2`, oldUpdatedAt, userID)
	s.Require().NoError(err)

	s.Require().NoError(svc.Move(context.Background(), userID, archive.ID, []int64{photos.ID, fileID, photos.ID}))

	result, err := svc.List(context.Background(), userID, newestFirst(0))
	s.Require().NoError(err)
	s.Equal([]string{"Archive"}, fileNames(result.Data))

	result, err = svc.List(context.Background(), userID, newestFirst(archive.ID))
	s.Require().NoError(err)
	s.Equal([]string{"Photos", "a.txt"}, fileNames(result.Data))

	path, err := svc.ResolveFolder(context.Background(), userID, []string{"Archive", "Photos", "Cars"})
	s.Require().NoError(err)
	s.Len(path.Path, 3)

	s.True(s.changedSince(photos.ID, oldUpdatedAt))
	s.True(s.changedSince(fileID, oldUpdatedAt))
	s.False(s.changedSince(cars.ID, oldUpdatedAt))
	s.False(s.changedSince(archive.ID, oldUpdatedAt))
}

func (s *FileServiceTestSuite) TestMove_ToRoot() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	sub := s.createFolder(userID, docs.ID, "Sub")
	_, err := s.finishUpload(userID, sub.ID, "x.txt", "x")
	s.Require().NoError(err)

	s.Require().NoError(svc.Move(context.Background(), userID, 0, []int64{sub.ID}))

	s.Zero(s.parentOf(sub.ID))
	result, err := svc.List(context.Background(), userID, newestFirst(0))
	s.Require().NoError(err)
	s.ElementsMatch([]string{"Docs", "Sub"}, fileNames(result.Data))

	result, err = svc.List(context.Background(), userID, newestFirst(sub.ID))
	s.Require().NoError(err)
	s.Equal([]string{"x.txt"}, fileNames(result.Data))
}

// Folders and files share one set of names. A move does not auto-rename: the message names the item, and nothing moves.
func (s *FileServiceTestSuite) TestMove_NameTaken() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	archive := s.createFolder(userID, 0, "Archive")
	_, err := s.finishUpload(userID, archive.ID, "a.txt", "a")
	s.Require().NoError(err)
	s.createFolder(userID, archive.ID, "Photos")
	fileA := s.seedFile(userID, "a.txt")
	fileB := s.seedFile(userID, "b.txt")
	photos := s.createFolder(userID, 0, "Photos")

	cases := []struct {
		ids     []int64
		message string
	}{
		{[]int64{fileA}, "a.txt already exists in this folder"},
		{[]int64{photos.ID}, "Photos already exists in this folder"},
		{[]int64{fileB, fileA}, "a.txt already exists in this folder"},
	}
	for _, tc := range cases {
		err := svc.Move(context.Background(), userID, archive.ID, tc.ids)

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusConflict, status, tc.message)
		s.Equal(tc.message, msg)
	}

	result, err := svc.List(context.Background(), userID, newestFirst(archive.ID))
	s.Require().NoError(err)
	s.ElementsMatch([]string{"Photos", "a.txt"}, fileNames(result.Data))
	s.Zero(s.parentOf(fileB))
}

// Two items with one name from two folders cannot both go into one folder.
func (s *FileServiceTestSuite) TestMove_TwoItemsWithOneName() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	notes := s.createFolder(userID, 0, "Notes")
	archive := s.createFolder(userID, 0, "Archive")
	inDocs, err := s.finishUpload(userID, docs.ID, "a.txt", "a")
	s.Require().NoError(err)
	inNotes, err := s.finishUpload(userID, notes.ID, "a.txt", "a")
	s.Require().NoError(err)

	err = svc.Move(context.Background(), userID, archive.ID, []int64{inDocs.ID, inNotes.ID})

	status, msg := apperr.Resolve(err)
	s.Equal(http.StatusConflict, status)
	s.Equal("an item with this name already exists", msg)
	s.Equal(docs.ID, s.parentOf(inDocs.ID))
	s.Equal(notes.ID, s.parentOf(inNotes.ID))
}

// A move into the folder an item is in already is refused, even with other items in the request. Nothing moves.
func (s *FileServiceTestSuite) TestMove_SameFolder() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	inDocs, err := s.finishUpload(userID, docs.ID, "a.txt", "a")
	s.Require().NoError(err)
	atRoot := s.seedFile(userID, "b.txt")

	cases := []struct {
		parentID int64
		ids      []int64
	}{
		{docs.ID, []int64{inDocs.ID}},
		{0, []int64{atRoot}},
		{docs.ID, []int64{atRoot, inDocs.ID}},
	}
	for _, tc := range cases {
		err := svc.Move(context.Background(), userID, tc.parentID, tc.ids)

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusBadRequest, status, tc.ids)
		s.Equal("the items are already in this folder", msg)
	}

	s.Zero(s.parentOf(atRoot))
	s.Equal(docs.ID, s.parentOf(inDocs.ID))
}

// A folder cannot go into itself or into a folder under it. The folders would form a loop that no path from the root reaches.
func (s *FileServiceTestSuite) TestMove_IntoItselfOrSubfolder() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	a := s.createFolder(userID, 0, "A")
	b := s.createFolder(userID, a.ID, "B")
	c := s.createFolder(userID, b.ID, "C")
	fileID := s.seedFile(userID, "x.txt")

	cases := []struct {
		parentID int64
		ids      []int64
	}{
		{a.ID, []int64{a.ID}},
		{b.ID, []int64{a.ID}},
		{c.ID, []int64{a.ID}},
		{c.ID, []int64{fileID, a.ID}},
		{c.ID, []int64{b.ID}},
	}
	for _, tc := range cases {
		err := svc.Move(context.Background(), userID, tc.parentID, tc.ids)

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusBadRequest, status, tc)
		s.Equal("a folder cannot move into itself or its own subfolder", msg)
	}

	path, err := svc.ResolveFolder(context.Background(), userID, []string{"A", "B", "C"})
	s.Require().NoError(err)
	s.Len(path.Path, 3)
	s.Zero(s.parentOf(fileID))
}

// Unknown ids, other users' items and items in the trash fail the same way. Nothing moves.
func (s *FileServiceTestSuite) TestMove_NotFound() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	archive := s.createFolder(alice, 0, "Archive")
	mine := s.seedFile(alice, "mine.txt")
	gone := s.seedFile(alice, "gone.txt")
	s.Require().NoError(svc.Trash(context.Background(), alice, []int64{gone}))
	bobFile := s.seedFile(bob, "bob.txt")

	for _, ids := range [][]int64{{999}, {mine, 999}, {mine, bobFile}, {mine, gone}} {
		err := svc.Move(context.Background(), alice, archive.ID, ids)

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusNotFound, status, ids)
		s.Equal("item not found", msg)
	}

	result, err := svc.List(context.Background(), alice, newestFirst(archive.ID))
	s.Require().NoError(err)
	s.Empty(result.Data)
	s.Zero(s.parentOf(bobFile))
}

// The target must be a live folder of the user.
func (s *FileServiceTestSuite) TestMove_UnknownFolder() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	mine := s.seedFile(alice, "a.txt")
	fileTarget := s.seedFile(alice, "b.txt")
	old := s.createFolder(alice, 0, "Old")
	s.Require().NoError(svc.Trash(context.Background(), alice, []int64{old.ID}))
	bobFolder := s.createFolder(bob, 0, "Bob")

	for _, target := range []int64{999, fileTarget, old.ID, bobFolder.ID} {
		err := svc.Move(context.Background(), alice, target, []int64{mine})

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusNotFound, status, target)
		s.Equal("folder not found", msg)
	}

	s.Zero(s.parentOf(mine))
}

// Two moves at once: A into Y (under B) and B into X (under A). Each sees no loop alone, and they lock different rows,
// so no row lock stops them. The tree lock runs them one at a time, so the second sees the first and fails.
func (s *FileServiceTestSuite) TestMove_ParallelMovesMakeNoLoop() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService

	const n = 20
	as, xs, bs, ys := make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n)
	for i := range n {
		as[i] = s.createFolder(userID, 0, fmt.Sprintf("A %d", i)).ID
		xs[i] = s.createFolder(userID, as[i], "X").ID
		bs[i] = s.createFolder(userID, 0, fmt.Sprintf("B %d", i)).ID
		ys[i] = s.createFolder(userID, bs[i], "Y").ID
	}

	errs := make([]error, 2*n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { errs[2*i] = svc.Move(context.Background(), userID, ys[i], []int64{as[i]}) })
		wg.Go(func() { errs[2*i+1] = svc.Move(context.Background(), userID, xs[i], []int64{bs[i]}) })
	}
	wg.Wait()

	for i := range n {
		aMoved, bMoved := errs[2*i] == nil, errs[2*i+1] == nil
		s.Require().NotEqual(aMoved, bMoved, "pair %d", i)

		moved, into, stayed, failed := as[i], ys[i], bs[i], errs[2*i+1]
		if bMoved {
			moved, into, stayed, failed = bs[i], xs[i], as[i], errs[2*i]
		}
		s.Equal("a folder cannot move into itself or its own subfolder", s.errorMessage(failed))
		s.Equal(into, s.parentOf(moved))
		s.Zero(s.parentOf(stayed))
	}
}

// The roots show up at once, marked as copying, and one job copies the rest. Each file gets new bytes with a fresh
// modified time, so the orphan sweep sees them as new. The sources stay as they were.
func (s *FileServiceTestSuite) TestCopy_FilesAndFolders() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	photos := s.createFolder(userID, 0, "Photos")
	cars := s.createFolder(userID, photos.ID, "Cars")
	s.createFolder(userID, photos.ID, "Empty")
	x := s.upload(userID, cars.ID, "x.txt", "x")
	s.upload(userID, photos.ID, "y.txt", "yy")
	a := s.upload(userID, 0, "a.txt", "aaa")
	archive := s.createFolder(userID, 0, "Archive")
	for _, id := range []int64{a.ID, x.ID} {
		s.ageStoredFile(s.storageKey(id), 25)
	}

	s.Require().NoError(svc.Copy(context.Background(), userID, archive.ID, []int64{photos.ID, a.ID, photos.ID}))

	copies, err := svc.List(context.Background(), userID, newestFirst(archive.ID))
	s.Require().NoError(err)
	s.Require().Equal([]string{"Photos", "a.txt"}, fileNames(copies.Data))
	photosCopy, aCopy := copies.Data[0], copies.Data[1]
	s.True(photosCopy.IsCopying)
	s.True(aCopy.IsCopying)
	rootIDs := slices.Sorted(slices.Values([]int64{photosCopy.ID, aCopy.ID}))
	s.Equal([]jobs.CopyItemsArgs{{UserID: userID, RootIDs: rootIDs}}, s.copyJobs())

	s.workCopyJobs()

	s.Equal([]string{"Cars/", "Cars/x.txt=x", "Empty/", "y.txt=yy"}, s.treeOf(photosCopy.ID))
	s.Equal(s.treeOf(photos.ID), s.treeOf(photosCopy.ID))
	s.Equal("aaa", s.storedContent(aCopy.ID))
	s.NotEqual(s.storageKey(a.ID), s.storageKey(aCopy.ID))
	xCopies := s.copiesOf(x.ID)
	s.Require().Len(xCopies, 1)
	for sourceID, copyID := range map[int64]int64{a.ID: aCopy.ID, x.ID: xCopies[0]} {
		s.True(s.storedModTime(copyID).After(s.storedModTime(sourceID)), copyID)
	}

	copies, err = svc.List(context.Background(), userID, newestFirst(archive.ID))
	s.Require().NoError(err)
	for _, f := range copies.Data {
		s.False(f.IsCopying, f.Name)
	}
	home, err := svc.List(context.Background(), userID, newestFirst(0))
	s.Require().NoError(err)
	s.Equal([]string{"Archive", "Photos", "a.txt"}, fileNames(home.Data))
	s.Equal([]map[string]any{{
		"user_id": userID, "root_ids": []any{rootIDs[0], rootIDs[1]},
		"rows": int64(4), "files": int64(3), "bytes": int64(6),
	}}, s.logs("copied items"))
}

func (s *FileServiceTestSuite) TestCopy_ToRoot() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	sub := s.createFolder(userID, docs.ID, "Sub")
	s.upload(userID, sub.ID, "x.txt", "x")

	s.Require().NoError(svc.Copy(context.Background(), userID, 0, []int64{sub.ID}))
	s.workCopyJobs()

	subCopies := s.copiesOf(sub.ID)
	s.Require().Len(subCopies, 1)
	s.Zero(s.parentOf(subCopies[0]))
	s.Equal([]string{"x.txt=x"}, s.treeOf(subCopies[0]))
	s.Equal([]string{"Sub/", "Sub/x.txt=x"}, s.treeOf(docs.ID))
}

// A copy does not auto-rename either: the message names the item, and nothing is copied.
func (s *FileServiceTestSuite) TestCopy_NameTaken() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	archive := s.createFolder(userID, 0, "Archive")
	s.upload(userID, archive.ID, "a.txt", "a")
	s.createFolder(userID, archive.ID, "Photos")
	fileA := s.seedFile(userID, "a.txt")
	fileB := s.seedFile(userID, "b.txt")
	photos := s.createFolder(userID, 0, "Photos")

	cases := []struct {
		ids     []int64
		message string
	}{
		{[]int64{fileA}, "a.txt already exists in this folder"},
		{[]int64{photos.ID}, "Photos already exists in this folder"},
		{[]int64{fileB, fileA}, "a.txt already exists in this folder"},
	}
	for _, tc := range cases {
		err := svc.Copy(context.Background(), userID, archive.ID, tc.ids)

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusConflict, status, tc.message)
		s.Equal(tc.message, msg)
	}

	result, err := svc.List(context.Background(), userID, newestFirst(archive.ID))
	s.Require().NoError(err)
	s.ElementsMatch([]string{"Photos", "a.txt"}, fileNames(result.Data))
	s.Empty(s.copyJobs())
}

// Two items with one name from two folders cannot both go into one folder.
func (s *FileServiceTestSuite) TestCopy_TwoItemsWithOneName() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	notes := s.createFolder(userID, 0, "Notes")
	archive := s.createFolder(userID, 0, "Archive")
	inDocs := s.upload(userID, docs.ID, "a.txt", "a")
	inNotes := s.upload(userID, notes.ID, "a.txt", "a")

	err := svc.Copy(context.Background(), userID, archive.ID, []int64{inDocs.ID, inNotes.ID})

	status, msg := apperr.Resolve(err)
	s.Equal(http.StatusConflict, status)
	s.Equal("an item with this name already exists", msg)
	s.Empty(s.treeOf(archive.ID))
	s.Empty(s.copyJobs())
}

// A copy into the folder an item is in already is refused, even with other items in the request.
func (s *FileServiceTestSuite) TestCopy_SameFolder() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	inDocs := s.upload(userID, docs.ID, "a.txt", "a")
	atRoot := s.seedFile(userID, "b.txt")

	cases := []struct {
		parentID int64
		ids      []int64
	}{
		{docs.ID, []int64{inDocs.ID}},
		{0, []int64{atRoot}},
		{docs.ID, []int64{atRoot, inDocs.ID}},
	}
	for _, tc := range cases {
		err := svc.Copy(context.Background(), userID, tc.parentID, tc.ids)

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusBadRequest, status, tc.ids)
		s.Equal("the items are already in this folder", msg)
	}

	s.Equal(3, s.fileRows())
	s.Empty(s.copyJobs())
}

// A copy works from what it finds, so it cannot loop, but A/B/A is almost never what the user wants.
func (s *FileServiceTestSuite) TestCopy_IntoItselfOrSubfolder() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	a := s.createFolder(userID, 0, "A")
	b := s.createFolder(userID, a.ID, "B")
	c := s.createFolder(userID, b.ID, "C")
	fileID := s.seedFile(userID, "x.txt")

	cases := []struct {
		parentID int64
		ids      []int64
	}{
		{a.ID, []int64{a.ID}},
		{b.ID, []int64{a.ID}},
		{c.ID, []int64{a.ID}},
		{c.ID, []int64{fileID, a.ID}},
		{c.ID, []int64{b.ID}},
	}
	for _, tc := range cases {
		err := svc.Copy(context.Background(), userID, tc.parentID, tc.ids)

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusBadRequest, status, tc)
		s.Equal("a folder cannot be copied into itself or its own subfolder", msg)
	}

	s.Equal(4, s.fileRows())
	s.Empty(s.copyJobs())
}

// Unknown ids, other users' items and items in the trash fail the same way. Nothing is copied.
func (s *FileServiceTestSuite) TestCopy_NotFound() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	archive := s.createFolder(alice, 0, "Archive")
	mine := s.seedFile(alice, "mine.txt")
	gone := s.seedFile(alice, "gone.txt")
	s.Require().NoError(svc.Trash(context.Background(), alice, []int64{gone}))
	bobFile := s.seedFile(bob, "bob.txt")

	for _, ids := range [][]int64{{999}, {mine, 999}, {mine, bobFile}, {mine, gone}} {
		err := svc.Copy(context.Background(), alice, archive.ID, ids)

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusNotFound, status, ids)
		s.Equal("item not found", msg)
	}

	s.Empty(s.treeOf(archive.ID))
	s.Empty(s.copyJobs())
}

// The target must be a live folder of the user that does not still copy.
func (s *FileServiceTestSuite) TestCopy_UnknownFolder() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	mine := s.seedFile(alice, "a.txt")
	fileTarget := s.seedFile(alice, "b.txt")
	old := s.createFolder(alice, 0, "Old")
	s.Require().NoError(svc.Trash(context.Background(), alice, []int64{old.ID}))
	bobFolder := s.createFolder(bob, 0, "Bob")
	docs := s.createFolder(alice, 0, "Docs")
	archive := s.createFolder(alice, 0, "Archive")
	s.Require().NoError(svc.Copy(context.Background(), alice, archive.ID, []int64{docs.ID}))
	copying := s.copiesOf(docs.ID)[0]

	for _, target := range []int64{999, fileTarget, old.ID, bobFolder.ID, copying} {
		err := svc.Copy(context.Background(), alice, target, []int64{mine})

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusNotFound, status, target)
		s.Equal("folder not found", msg)
	}

	s.Len(s.copyJobs(), 1)
}

// Until the job ends, nothing can go into a copy, and it cannot be deleted or copied. It can be renamed and moved.
func (s *FileServiceTestSuite) TestCopy_ItemStillCopying() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	s.upload(userID, docs.ID, "x.txt", "x")
	archive := s.createFolder(userID, 0, "Archive")
	empty := s.createFolder(userID, 0, "Empty")
	other := s.seedFile(userID, "other.txt")
	s.Require().NoError(svc.Copy(context.Background(), userID, archive.ID, []int64{docs.ID}))
	docsCopy := s.copiesOf(docs.ID)[0]

	for _, ids := range [][]int64{{docsCopy}, {other, docsCopy}} {
		s.Equal("Docs is still copying", s.errorMessage(svc.Copy(context.Background(), userID, empty.ID, ids)))
		s.Equal("Docs is still copying", s.errorMessage(svc.Trash(context.Background(), userID, ids)))
	}
	s.Zero(s.parentOf(other))
	s.Empty(s.treeOf(empty.ID))

	for _, into := range []func() error{
		func() error { _, err := svc.List(context.Background(), userID, newestFirst(docsCopy)); return err },
		func() error {
			_, err := s.finishUpload(userID, docsCopy, "new.txt", "n")
			return err
		},
		func() error { _, err := svc.CreateFolder(context.Background(), userID, docsCopy, "New"); return err },
		func() error { return svc.Move(context.Background(), userID, docsCopy, []int64{other}) },
		func() error { return svc.Copy(context.Background(), userID, docsCopy, []int64{other}) },
	} {
		s.Equal("folder not found", s.errorMessage(into()))
	}
	_, err := svc.ResolveFolder(context.Background(), userID, []string{"Archive", "Docs"})
	s.Equal("folder not found", s.errorMessage(err))

	_, err = svc.Rename(context.Background(), userID, docsCopy, "Docs copy")
	s.Require().NoError(err)
	s.Require().NoError(svc.Move(context.Background(), userID, 0, []int64{docsCopy}))

	s.workCopyJobs()

	s.Equal([]string{"x.txt=x"}, s.treeOf(docsCopy))
	_, err = svc.ResolveFolder(context.Background(), userID, []string{"Docs copy"})
	s.Require().NoError(err)
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{docsCopy}))
}

// Big holds 260 files and 251 folders, so its level takes several batches. Each folder holds a file, and that level
// has more copy folders than one query reads under. Sub 1 holds one more level.
func (s *FileServiceTestSuite) TestCopyItems_ManyBatchesAndLevels() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	big := s.createFolder(userID, 0, "Big")
	archive := s.createFolder(userID, 0, "Archive")
	for _, insert := range []string{
		`INSERT INTO files (user_id, parent_id, name, storage_key, content_type, size_bytes)
		SELECT $1, $2, 'f' || i || '.txt', md5(random()::text), 'text/plain', 1 FROM generate_series(1, 260) i`,
		`INSERT INTO files (user_id, parent_id, name, content_type, size_bytes)
		SELECT $1, $2, 'Sub ' || i, '', 0 FROM generate_series(1, 251) i`,
		`INSERT INTO files (user_id, parent_id, name, storage_key, content_type, size_bytes)
		SELECT user_id, id, 'in.txt', md5(random()::text), 'text/plain', 1 FROM files
		WHERE user_id = $1 AND parent_id = $2 AND storage_key IS NULL`,
	} {
		_, err := s.TC.DB.ExecContext(context.Background(), insert, userID, big.ID)
		s.Require().NoError(err)
	}
	s.writeSeededBytes(userID)
	path, err := svc.ResolveFolder(context.Background(), userID, []string{"Big", "Sub 1"})
	s.Require().NoError(err)
	deep := s.createFolder(userID, path.Path[1].ID, "Deep")
	s.upload(userID, deep.ID, "deep.txt", "d")

	s.Require().NoError(svc.Copy(context.Background(), userID, archive.ID, []int64{big.ID}))
	s.workCopyJobs()

	bigCopy := s.copiesOf(big.ID)[0]
	s.Len(s.treeOf(bigCopy), 260+251+251+2)
	s.Equal(s.treeOf(big.ID), s.treeOf(bigCopy))
	s.Equal(int64(260+251+251+2), s.logs("copied items")[0]["rows"])
}

// The first run copied a.txt and Sub, then failed. The next run adds only what is missing.
func (s *FileServiceTestSuite) TestCopyItems_RunAfterFailureFinishes() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	sub := s.createFolder(userID, docs.ID, "Sub")
	a := s.upload(userID, docs.ID, "a.txt", "a")
	s.upload(userID, docs.ID, "b.txt", "b")
	s.upload(userID, sub.ID, "c.txt", "c")
	archive := s.createFolder(userID, 0, "Archive")
	s.Require().NoError(svc.Copy(context.Background(), userID, archive.ID, []int64{docs.ID}))
	docsCopy := s.copiesOf(docs.ID)[0]
	s.addCopy(docsCopy, a.ID)
	s.addCopy(docsCopy, sub.ID)

	s.workCopyJobs()

	s.Equal([]string{"Sub/", "Sub/c.txt=c", "a.txt=a", "b.txt=b"}, s.treeOf(docsCopy))
	s.Equal(int64(2), s.logs("copied items")[0]["rows"])
}

// a.txt, 250 more files in Docs, and c.txt lost their bytes. Their copies are left out, and the copy root of c.txt
// goes. The rest is copied. The lost files fill a whole batch, and the job must not read them again and again.
func (s *FileServiceTestSuite) TestCopyItems_SkipsFilesWithNoBytes() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	a := s.upload(userID, docs.ID, "a.txt", "a")
	s.upload(userID, docs.ID, "b.txt", "b")
	c := s.upload(userID, 0, "c.txt", "c")
	archive := s.createFolder(userID, 0, "Archive")
	for _, id := range []int64{a.ID, c.ID} {
		s.Require().NoError(os.Remove(filepath.Join(s.TC.UploadDir, s.storageKey(id))))
	}
	_, err := s.TC.DB.ExecContext(context.Background(), `
		INSERT INTO files (user_id, parent_id, name, storage_key, content_type, size_bytes)
		SELECT $1, $2, 'lost ' || i || '.txt', md5(random()::text), 'text/plain', 1 FROM generate_series(1, 250) i`,
		userID, docs.ID)
	s.Require().NoError(err)
	s.Require().NoError(svc.Copy(context.Background(), userID, archive.ID, []int64{docs.ID, c.ID}))
	cCopy := s.copiesOf(c.ID)[0]

	s.workCopyJobs()

	s.Equal([]string{"Docs/", "Docs/b.txt=b"}, s.treeOf(archive.ID))
	skipped := s.logs("skipped a file with no bytes")
	s.Len(skipped, 251)
	s.Contains(skipped, map[string]any{"user_id": userID, "source_id": a.ID})
	s.Equal([]map[string]any{{"user_id": userID, "copy_id": cCopy, "source_id": c.ID}}, s.logs("removed a copy with no bytes"))
	copied := s.logs("copied items")
	s.Require().Len(copied, 1)
	s.Equal(int64(1), copied[0]["rows"])
	s.Equal(int64(1), copied[0]["files"])
}

// The first batch copied a.txt. Then the user renamed a.txt to z.txt, and b.txt to a.txt. The copy of b.txt takes
// a free name, so the job does not fail or loop.
func (s *FileServiceTestSuite) TestCopyItems_NameClashFromRenameInSource() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	a := s.upload(userID, docs.ID, "a.txt", "a")
	b := s.upload(userID, docs.ID, "b.txt", "b")
	archive := s.createFolder(userID, 0, "Archive")
	s.Require().NoError(svc.Copy(context.Background(), userID, archive.ID, []int64{docs.ID}))
	docsCopy := s.copiesOf(docs.ID)[0]
	s.addCopy(docsCopy, a.ID)
	_, err := svc.Rename(context.Background(), userID, a.ID, "z.txt")
	s.Require().NoError(err)
	_, err = svc.Rename(context.Background(), userID, b.ID, "a.txt")
	s.Require().NoError(err)

	s.workCopyJobs()

	s.Equal([]string{"a (1).txt=b", "a.txt=a"}, s.treeOf(docsCopy))
}

// x.txt went to the trash before the job got to Docs, and n.txt in Docs is a copy that is not done. Both are left out.
func (s *FileServiceTestSuite) TestCopyItems_LeavesOutDeletedAndUnfinishedItems() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	x := s.upload(userID, docs.ID, "x.txt", "x")
	s.upload(userID, docs.ID, "keep.txt", "k")
	notes := s.createFolder(userID, 0, "Notes")
	n := s.upload(userID, notes.ID, "n.txt", "n")
	archive := s.createFolder(userID, 0, "Archive")
	s.Require().NoError(svc.Copy(context.Background(), userID, docs.ID, []int64{n.ID}))
	s.Require().NoError(svc.Copy(context.Background(), userID, archive.ID, []int64{docs.ID}))
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{x.ID}))
	docsCopy := s.copiesOf(docs.ID)[0]

	s.Require().NoError(svc.CopyItems(context.Background(), userID, []int64{docsCopy}))

	s.Equal([]string{"keep.txt=k"}, s.treeOf(docsCopy))
}

// Archive, with the copy root in it, moved into Docs during the copy. The job copies Archive, finds its own copy
// root there, and leaves it out. So it does not copy what it makes.
func (s *FileServiceTestSuite) TestCopyItems_DoesNotCopyItsOwnCopy() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	s.upload(userID, docs.ID, "x.txt", "x")
	archive := s.createFolder(userID, 0, "Archive")
	s.Require().NoError(svc.Copy(context.Background(), userID, archive.ID, []int64{docs.ID}))
	s.Require().NoError(svc.Move(context.Background(), userID, docs.ID, []int64{archive.ID}))
	docsCopy := s.copiesOf(docs.ID)[0]

	s.workCopyJobs()

	s.Equal([]string{"Archive/", "x.txt=x"}, s.treeOf(docsCopy))
}

// The first run made Sub in the copy. Then Sub went to the trash. The job adds nothing under it and ends.
// Sub holds a whole batch, and the job must not read it again and again.
func (s *FileServiceTestSuite) TestCopyItems_CopyFolderInTrashGetsNothingMore() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	sub := s.createFolder(userID, docs.ID, "Sub")
	s.upload(userID, sub.ID, "x.txt", "x")
	_, err := s.TC.DB.ExecContext(context.Background(), `
		INSERT INTO files (user_id, parent_id, name, content_type, size_bytes)
		SELECT $1, $2, 'Sub ' || i, '', 0 FROM generate_series(1, 250) i`, userID, sub.ID)
	s.Require().NoError(err)
	s.upload(userID, docs.ID, "a.txt", "a")
	archive := s.createFolder(userID, 0, "Archive")
	s.Require().NoError(svc.Copy(context.Background(), userID, archive.ID, []int64{docs.ID}))
	docsCopy := s.copiesOf(docs.ID)[0]
	subCopy := s.addCopy(docsCopy, sub.ID)
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{subCopy}))

	s.workCopyJobs()

	s.Equal([]string{"a.txt=a"}, s.treeOf(docsCopy))
	s.Zero(s.liveRowsInTrash(userID))
	s.Equal(int64(1), s.logs("copied items")[0]["rows"])
}

// A job for an item that is not a copy root, or one that finished, changes nothing.
func (s *FileServiceTestSuite) TestCopyItems_SkipsRootsNotMarked() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	a := s.upload(userID, 0, "a.txt", "a")

	s.Require().NoError(svc.CopyItems(context.Background(), userID, []int64{a.ID}))

	s.Equal(1, s.fileRows())
	s.Empty(s.logs("copied items"))
}

// The sweep can add a copy job while the first one still runs, under other args. The runs skip each other's rows,
// and each byte copy has its own temp file. So nothing is copied twice, and no bytes are left without a row.
func (s *FileServiceTestSuite) TestCopyItems_RunsAtOnce() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	_, err := s.TC.DB.ExecContext(context.Background(), `
		INSERT INTO files (user_id, parent_id, name, storage_key, content_type, size_bytes)
		SELECT $1, $2, 'f' || i || '.txt', md5(random()::text), 'text/plain', 1 FROM generate_series(1, 150) i`,
		userID, docs.ID)
	s.Require().NoError(err)
	s.writeSeededBytes(userID)
	sub := s.createFolder(userID, docs.ID, "Sub")
	s.upload(userID, sub.ID, "x.txt", "x")
	big := s.seedFile(userID, "big.bin")
	bigPath := filepath.Join(s.TC.UploadDir, s.storageKey(big))
	s.Require().NoError(os.WriteFile(bigPath, bytes.Repeat([]byte("b"), 8<<20), 0o640))
	s.T().Cleanup(func() { _ = os.Remove(bigPath) })
	archive := s.createFolder(userID, 0, "Archive")
	s.Require().NoError(svc.Copy(context.Background(), userID, archive.ID, []int64{docs.ID, big}))
	rootIDs := s.copyJobs()[0].RootIDs
	before := s.dirEntries()

	errs := make([]error, 4)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() { errs[i] = svc.CopyItems(context.Background(), userID, rootIDs) })
	}
	wg.Wait()

	for _, err := range errs {
		s.Require().NoError(err)
	}
	s.Equal(s.treeOf(docs.ID), s.treeOf(s.copiesOf(docs.ID)[0]))
	s.Len(s.storedContent(s.copiesOf(big)[0]), 8<<20)
	var added []string
	for _, name := range s.dirEntries() {
		if !slices.Contains(before, name) {
			added = append(added, name)
		}
	}
	s.ElementsMatch(s.copyKeys(userID), added)
}

// A copy root marked more than a day ago gets its job again, unless River still holds a job with the same args.
func (s *FileServiceTestSuite) TestSweepCopies_AddsCopyJobAgainAfterRiverGivesUp() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	s.upload(userID, docs.ID, "x.txt", "x")
	a := s.upload(userID, 0, "a.txt", "a")
	archive := s.createFolder(userID, 0, "Archive")
	s.Require().NoError(svc.Copy(context.Background(), userID, archive.ID, []int64{docs.ID}))
	s.Require().NoError(svc.Copy(context.Background(), userID, archive.ID, []int64{a.ID}))
	docsCopy := s.copiesOf(docs.ID)[0]
	_, err := s.TC.DB.ExecContext(context.Background(),
		`UPDATE files SET copy_requested_at = copy_requested_at - INTERVAL '25 hours' WHERE id = $1`, docsCopy)
	s.Require().NoError(err)
	stuck := jobs.CopyItemsArgs{UserID: userID, RootIDs: []int64{docsCopy}}
	fresh := jobs.CopyItemsArgs{UserID: userID, RootIDs: []int64{s.copiesOf(a.ID)[0]}}

	s.Require().NoError(svc.SweepCopies(context.Background()))
	s.Equal([]jobs.CopyItemsArgs{stuck, fresh}, s.copyJobs())
	s.Empty(s.logs("added the copy job again"))

	_, err = s.TC.DB.ExecContext(context.Background(), `
		UPDATE river_job SET state = 'discarded', finalized_at = NOW()
		WHERE id = (SELECT MIN(id) FROM river_job WHERE kind = 'copy_items')`)
	s.Require().NoError(err)

	s.Require().NoError(svc.SweepCopies(context.Background()))
	s.Equal([]jobs.CopyItemsArgs{stuck, fresh, stuck}, s.copyJobs())
	s.Equal([]map[string]any{{"user_id": userID, "root_ids": []any{docsCopy}}}, s.logs("added the copy job again"))

	s.workCopyJobs()

	s.Equal([]string{"x.txt=x"}, s.treeOf(docsCopy))
	s.Require().NoError(svc.SweepCopies(context.Background()))
	s.Empty(s.copyJobs())
}

// Every row under the folder goes to the trash with it, so no path, list, upload or rename can reach them.
func (s *FileServiceTestSuite) TestTrash_HidesFolderAndEverythingUnder() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	photos := s.createFolder(userID, 0, "Photos")
	cars := s.createFolder(userID, photos.ID, "Cars")
	inCars, err := s.finishUpload(userID, cars.ID, "x.txt", "x")
	s.Require().NoError(err)
	inPhotos, err := s.finishUpload(userID, photos.ID, "y.txt", "y")
	s.Require().NoError(err)
	s.seedFile(userID, "a.txt")

	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{photos.ID}))
	s.workTrashJobs()

	result, err := svc.List(context.Background(), userID, newestFirst(0))
	s.Require().NoError(err)
	s.Equal([]string{"a.txt"}, fileNames(result.Data))

	for _, names := range [][]string{{"Photos"}, {"Photos", "Cars"}} {
		_, err := svc.ResolveFolder(context.Background(), userID, names)
		status, _ := apperr.Resolve(err)
		s.Equal(http.StatusNotFound, status, names)
	}

	for _, folderID := range []int64{photos.ID, cars.ID} {
		_, err := svc.List(context.Background(), userID, newestFirst(folderID))
		s.Equal("folder not found", s.errorMessage(err))

		_, err = s.finishUpload(userID, folderID, "new.txt", "n")
		s.Equal("folder not found", s.errorMessage(err))

		_, err = svc.CreateFolder(context.Background(), userID, folderID, "New")
		s.Equal("folder not found", s.errorMessage(err))
	}

	for _, id := range []int64{photos.ID, cars.ID, inCars.ID, inPhotos.ID} {
		_, err := svc.Rename(context.Background(), userID, id, "renamed")
		s.Equal("item not found", s.errorMessage(err))
		s.Equal(photos.ID, s.trashRoot(id))
	}
}

func (s *FileServiceTestSuite) TestTrash_Many() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	a := s.seedFile(userID, "a.txt")
	b := s.seedFile(userID, "b.txt")
	s.seedFile(userID, "c.txt")
	docs := s.createFolder(userID, 0, "Docs")

	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{a, docs.ID, b, a}))

	result, err := svc.List(context.Background(), userID, newestFirst(0))
	s.Require().NoError(err)
	s.Equal([]string{"c.txt"}, fileNames(result.Data))
	for _, id := range []int64{a, b, docs.ID} {
		s.Equal(id, s.trashRoot(id))
	}
}

// Names in the trash are free, for upload, new folder, auto-rename and rename.
func (s *FileServiceTestSuite) TestTrash_FreesNames() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	a := s.seedFile(userID, "a.txt")
	a1 := s.seedFile(userID, "a (1).txt")
	photos := s.createFolder(userID, 0, "Photos")
	b := s.seedFile(userID, "b.txt")

	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{a, a1, photos.ID}))

	for _, want := range []string{"a.txt", "a (1).txt"} {
		f, err := s.finishUpload(userID, 0, "a.txt", "new")
		s.Require().NoError(err)
		s.Equal(want, f.Name)
	}

	folder, err := svc.CreateFolder(context.Background(), userID, 0, "Photos")
	s.Require().NoError(err)
	s.Equal("Photos", folder.Name)

	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{folder.ID}))
	renamed, err := svc.Rename(context.Background(), userID, b, "Photos")
	s.Require().NoError(err)
	s.Equal("Photos", renamed.Name)
}

// One bad id undoes the rest, so nothing goes to the trash.
func (s *FileServiceTestSuite) TestTrash_AllOrNone() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	a := s.seedFile(alice, "a.txt")
	trashed := s.seedFile(alice, "old.txt")
	s.Require().NoError(svc.Trash(context.Background(), alice, []int64{trashed}))
	bobFile := s.seedFile(bob, "bob.txt")

	for _, bad := range []int64{999, bobFile, trashed} {
		err := svc.Trash(context.Background(), alice, []int64{a, bad})

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusNotFound, status, bad)
		s.Equal("item not found", msg)
	}

	result, err := svc.List(context.Background(), alice, newestFirst(0))
	s.Require().NoError(err)
	s.Equal([]string{"a.txt"}, fileNames(result.Data))

	result, err = svc.List(context.Background(), bob, newestFirst(0))
	s.Require().NoError(err)
	s.Equal([]string{"bob.txt"}, fileNames(result.Data))
}

// An item deleted on its own keeps its group when its folder follows, and so do the rows under it.
// An item listed next to its folder keeps its group too.
func (s *FileServiceTestSuite) TestTrash_EachItemKeepsItsGroup() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	old, err := s.finishUpload(userID, docs.ID, "old.txt", "o")
	s.Require().NoError(err)
	oldDir := s.createFolder(userID, docs.ID, "Old")
	inOldDir, err := s.finishUpload(userID, oldDir.ID, "in-old.txt", "i")
	s.Require().NoError(err)
	listed, err := s.finishUpload(userID, docs.ID, "listed.txt", "l")
	s.Require().NoError(err)
	rest, err := s.finishUpload(userID, docs.ID, "rest.txt", "r")
	s.Require().NoError(err)

	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{old.ID, oldDir.ID}))
	s.workTrashJobs()
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{listed.ID, docs.ID}))
	s.workTrashJobs()

	s.Equal(old.ID, s.trashRoot(old.ID))
	s.Equal(oldDir.ID, s.trashRoot(oldDir.ID))
	s.Equal(oldDir.ID, s.trashRoot(inOldDir.ID))
	s.Equal(listed.ID, s.trashRoot(listed.ID))
	s.Equal(docs.ID, s.trashRoot(docs.ID))
	s.Equal(docs.ID, s.trashRoot(rest.ID))
}

// Trash marks only the items. One job, for the folders only, marks the rows under them.
func (s *FileServiceTestSuite) TestTrash_JobMarksRowsUnderFolders() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	a := s.seedFile(userID, "a.txt")
	docs := s.createFolder(userID, 0, "Docs")
	empty := s.createFolder(userID, 0, "Empty")
	inDocs, err := s.finishUpload(userID, docs.ID, "x.txt", "x")
	s.Require().NoError(err)

	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{a}))
	s.Empty(s.trashJobs())

	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{empty.ID, docs.ID}))
	s.Equal([]jobs.TrashSubtreesArgs{{UserID: userID, RootIDs: []int64{docs.ID, empty.ID}}}, s.trashJobs())
	s.Equal(1, s.liveRowsInTrash(userID))

	s.workTrashJobs()

	s.Zero(s.liveRowsInTrash(userID))
	s.Equal(docs.ID, s.trashRoot(inDocs.ID))
	s.Equal([]map[string]any{
		{"user_id": userID, "root_ids": []any{docs.ID, empty.ID}, "rows": int64(1)},
	}, s.logs("trashed the rows under folders"))
}

// Big holds 1500 files and 1001 folders, so its level takes 3 batches. Each folder holds a file, and that level
// has more parents than one batch. Sub 1 holds one more level.
func (s *FileServiceTestSuite) TestTrashSubtrees_ManyBatchesAndLevels() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	big := s.createFolder(userID, 0, "Big")
	for _, insert := range []string{
		`INSERT INTO files (user_id, parent_id, name, storage_key, content_type, size_bytes)
		SELECT $1, $2, 'f' || i || '.txt', 'big-' || i, 'text/plain', 1 FROM generate_series(1, 1500) i`,
		`INSERT INTO files (user_id, parent_id, name, content_type, size_bytes)
		SELECT $1, $2, 'Sub ' || i, '', 0 FROM generate_series(1, 1001) i`,
		`INSERT INTO files (user_id, parent_id, name, storage_key, content_type, size_bytes)
		SELECT user_id, id, 'in.txt', 'sub-' || id, 'text/plain', 1 FROM files
		WHERE user_id = $1 AND parent_id = $2 AND storage_key IS NULL`,
	} {
		_, err := s.TC.DB.ExecContext(context.Background(), insert, userID, big.ID)
		s.Require().NoError(err)
	}
	path, err := svc.ResolveFolder(context.Background(), userID, []string{"Big", "Sub 1"})
	s.Require().NoError(err)
	deep := s.createFolder(userID, path.Path[1].ID, "Deep")
	inDeep, err := s.finishUpload(userID, deep.ID, "deep.txt", "d")
	s.Require().NoError(err)

	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{big.ID}))
	s.workTrashJobs()

	s.Zero(s.liveRowsInTrash(userID))
	s.Equal(1+1500+1001+1001+2, s.rowsInGroup(big.ID))
	s.Equal(big.ID, s.trashRoot(inDeep.ID))
}

// The job marked Sub, then the user restored Docs. The rest of the job finds nothing to mark.
func (s *FileServiceTestSuite) TestTrashSubtrees_RestoreMidJobStopsIt() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	sub := s.createFolder(userID, docs.ID, "Sub")
	_, err := s.finishUpload(userID, sub.ID, "x.txt", "x")
	s.Require().NoError(err)
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{docs.ID}))
	s.markInGroup(docs.ID, sub.ID)

	s.Require().NoError(svc.Restore(context.Background(), userID, []int64{docs.ID}))
	s.workTrashJobs()

	result, err := svc.List(context.Background(), userID, newestFirst(sub.ID))
	s.Require().NoError(err)
	s.Equal([]string{"x.txt"}, fileNames(result.Data))
	trash, err := svc.ListTrash(context.Background(), userID, file.TrashQuery{Sort: file.SortByDate})
	s.Require().NoError(err)
	s.Empty(trash.Data)
}

// The first run marked Sub and failed, so x.txt under it is still live. The next run walks through Sub and marks it.
func (s *FileServiceTestSuite) TestTrashSubtrees_RunAfterFailureFinishes() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	sub := s.createFolder(userID, docs.ID, "Sub")
	x, err := s.finishUpload(userID, sub.ID, "x.txt", "x")
	s.Require().NoError(err)
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{docs.ID}))
	s.markInGroup(docs.ID, sub.ID)
	s.Equal(1, s.liveRowsInTrash(userID))

	s.workTrashJobs()

	s.Zero(s.liveRowsInTrash(userID))
	s.Equal(docs.ID, s.trashRoot(x.ID))
}

// The trash lists the items the user deleted, folders first, newest delete first. The rows under them and other users' items stay out.
func (s *FileServiceTestSuite) TestListTrash_OnlyDeletedItems() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	photos := s.createFolder(alice, 0, "Photos")
	_, err := s.finishUpload(alice, photos.ID, "in-photos.txt", "p")
	s.Require().NoError(err)
	a := s.seedFile(alice, "a.txt")
	b := s.seedFile(alice, "b.txt")
	s.seedFile(alice, "live.txt")
	bobFile := s.seedFile(bob, "bob.txt")
	s.Require().NoError(svc.Trash(context.Background(), bob, []int64{bobFile}))
	s.Require().NoError(svc.Trash(context.Background(), alice, []int64{b, photos.ID}))
	s.workTrashJobs()
	s.Require().NoError(svc.Trash(context.Background(), alice, []int64{a}))
	s.moveTrashedAt(b, 2)
	s.moveTrashedAt(photos.ID, 2)

	result, err := svc.ListTrash(context.Background(), alice, file.TrashQuery{Sort: file.SortByDate, Desc: true})

	s.Require().NoError(err)
	s.Nil(result.NextCursor)
	s.Require().Len(result.Data, 3)
	s.Equal([]string{"Photos", "a.txt", "b.txt"}, []string{result.Data[0].Name, result.Data[1].Name, result.Data[2].Name})
	s.True(result.Data[0].IsFolder)
	s.True(result.Data[1].TrashedAt.After(result.Data[2].TrashedAt))
	s.False(result.Data[1].TrashedAt.IsZero())

	empty, err := svc.ListTrash(context.Background(), s.seedUserWithEmail("carol@example.com"), file.TrashQuery{Sort: file.SortByDate})
	s.Require().NoError(err)
	s.NotNil(empty.Data)
	s.Empty(empty.Data)
}

// The same walk as the file list: every page joins up to the full list, and desc reverses asc inside each part.
func (s *FileServiceTestSuite) TestListTrash_WalkEverySort() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	var ids []int64
	for _, name := range []string{"Beta", "alpha", "Gamma"} {
		ids = append(ids, s.createFolder(userID, 0, name).ID)
	}
	ids = append(ids,
		s.seedFileWith(userID, "a.txt", "text/plain", 10, 0),
		s.seedFileWith(userID, "b.png", "image/png", 10, 0),
		s.seedFileWith(userID, "c.pdf", "text/plain", 5, 0),
	)
	s.Require().NoError(svc.Trash(context.Background(), userID, ids))
	s.moveTrashedAt(ids[1], 1)
	s.moveTrashedAt(ids[5], 3)

	for _, sort := range []file.SortField{file.SortByName, file.SortBySize, file.SortByType, file.SortByDate} {
		var full [2][]string
		for i, desc := range []bool{false, true} {
			result, err := svc.ListTrash(context.Background(), userID, file.TrashQuery{PageSize: 100, Sort: sort, Desc: desc})
			s.Require().NoError(err)
			full[i] = trashNames(result.Data)

			pages := s.walkTrash(userID, file.TrashQuery{PageSize: 2, Sort: sort, Desc: desc})
			s.Equal(full[i], slices.Concat(pages...), "%s desc=%t", sort, desc)
		}

		s.ElementsMatch([]string{"Beta", "alpha", "Gamma"}, full[0][:3], sort)
		ascFolders, ascFiles := slices.Clone(full[0][:3]), slices.Clone(full[0][3:])
		slices.Reverse(ascFolders)
		slices.Reverse(ascFiles)
		s.Equal(slices.Concat(ascFolders, ascFiles), full[1], "%s desc must reverse asc", sort)
	}
}

// A folder in the trash counts the rows deleted with it, before and after the trash job marks them. A file deleted on
// its own before the folder is its own trash item, so the folder does not count it.
func (s *FileServiceTestSuite) TestListTrash_FolderSizes() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	photos := s.createFolder(userID, 0, "Photos")
	trip := s.createFolder(userID, photos.ID, "Trip")
	s.upload(userID, photos.ID, "a.txt", "aaa")
	s.upload(userID, trip.ID, "b.txt", "bb")
	early := s.upload(userID, trip.ID, "c.txt", "cccc")
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{early.ID}))
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{photos.ID}))

	for _, label := range []string{"before the trash job", "after the trash job"} {
		result, err := svc.ListTrash(context.Background(), userID, file.TrashQuery{Sort: file.SortByName})
		s.Require().NoError(err)
		files := make([]*file.File, len(result.Data))
		for i, item := range result.Data {
			files[i] = &item.File
		}
		s.Equal(map[string]int64{"Photos": 5, "c.txt": 4}, fileSizes(files), label)

		s.workTrashJobs()
	}
}

func (s *FileServiceTestSuite) TestListTrash_BadCursor() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	a := s.seedFile(userID, "a.txt")
	b := s.seedFile(userID, "b.txt")
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{a, b}))

	first, err := svc.ListTrash(context.Background(), userID, file.TrashQuery{PageSize: 1, Sort: file.SortByName})
	s.Require().NoError(err)
	s.Require().NotNil(first.NextCursor)

	for _, q := range []file.TrashQuery{
		{Cursor: *first.NextCursor, Sort: file.SortByDate},
		{Cursor: *first.NextCursor, Sort: file.SortByName, Desc: true},
		{Cursor: cursorOf(`{"s":"date","p":1,"v":["not a time","1"]}`), Sort: file.SortByDate},
	} {
		_, err := svc.ListTrash(context.Background(), userID, q)

		s.Equal("invalid cursor", s.errorMessage(err))
	}
}

// The folder and every row under it come back. Only the item the user deleted counts as changed.
func (s *FileServiceTestSuite) TestRestore_BringsBackFolderAndEverythingUnder() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	photos := s.createFolder(userID, 0, "Photos")
	cars := s.createFolder(userID, photos.ID, "Cars")
	_, err := s.finishUpload(userID, cars.ID, "x.txt", "x")
	s.Require().NoError(err)
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{photos.ID}))
	s.workTrashJobs()
	_, err = s.TC.DB.ExecContext(context.Background(), `UPDATE files SET updated_at = $1 WHERE user_id = $2`, oldUpdatedAt, userID)
	s.Require().NoError(err)

	s.Require().NoError(svc.Restore(context.Background(), userID, []int64{photos.ID, photos.ID}))

	path, err := svc.ResolveFolder(context.Background(), userID, []string{"Photos", "Cars"})
	s.Require().NoError(err)
	s.Equal(cars.ID, path.Path[1].ID)
	result, err := svc.List(context.Background(), userID, newestFirst(cars.ID))
	s.Require().NoError(err)
	s.Equal([]string{"x.txt"}, fileNames(result.Data))
	s.True(s.changedSince(photos.ID, oldUpdatedAt))
	s.False(s.changedSince(cars.ID, oldUpdatedAt))

	trash, err := svc.ListTrash(context.Background(), userID, file.TrashQuery{Sort: file.SortByDate})
	s.Require().NoError(err)
	s.Empty(trash.Data)
}

// A name taken since the delete gets a number, the same as an upload.
func (s *FileServiceTestSuite) TestRestore_RenamesOnClash() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	a := s.seedFile(userID, "a.txt")
	docs := s.createFolder(userID, 0, "Docs")
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{a, docs.ID}))
	_, err := s.finishUpload(userID, 0, "a.txt", "new")
	s.Require().NoError(err)
	s.createFolder(userID, 0, "Docs")

	s.Require().NoError(svc.Restore(context.Background(), userID, []int64{a, docs.ID}))

	names := s.namesByID(userID)
	s.Len(names, 4)
	s.Equal("a (1).txt", names[a])
	s.Equal("Docs (1)", names[docs.ID])
}

// Two deleted items with one name come back together. The older row keeps the name.
func (s *FileServiceTestSuite) TestRestore_TwoItemsWithOneName() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	first := s.seedFile(userID, "a.txt")
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{first}))
	second, err := s.finishUpload(userID, 0, "a.txt", "new")
	s.Require().NoError(err)
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{second.ID}))

	s.Require().NoError(svc.Restore(context.Background(), userID, []int64{second.ID, first}))

	names := s.namesByID(userID)
	s.Len(names, 2)
	s.Equal("a.txt", names[first])
	s.Equal("a (1).txt", names[second.ID])
}

// An item deleted before its folder stays in the trash when the folder comes back. Its own restore puts it back in the folder.
func (s *FileServiceTestSuite) TestRestore_LeavesItemDeletedEarlier() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	old, err := s.finishUpload(userID, docs.ID, "old.txt", "o")
	s.Require().NoError(err)
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{old.ID}))
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{docs.ID}))
	s.workTrashJobs()

	s.Require().NoError(svc.Restore(context.Background(), userID, []int64{docs.ID}))

	result, err := svc.List(context.Background(), userID, newestFirst(docs.ID))
	s.Require().NoError(err)
	s.Empty(result.Data)
	s.Equal(old.ID, s.trashRoot(old.ID))

	s.Require().NoError(svc.Restore(context.Background(), userID, []int64{old.ID}))

	result, err = svc.List(context.Background(), userID, newestFirst(docs.ID))
	s.Require().NoError(err)
	s.Equal([]string{"old.txt"}, fileNames(result.Data))
}

func (s *FileServiceTestSuite) TestRestore_FolderInTrashSendsItemHome() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	old, err := s.finishUpload(userID, docs.ID, "old.txt", "o")
	s.Require().NoError(err)
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{old.ID}))
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{docs.ID}))
	s.workTrashJobs()

	s.Require().NoError(svc.Restore(context.Background(), userID, []int64{old.ID}))

	result, err := svc.List(context.Background(), userID, newestFirst(0))
	s.Require().NoError(err)
	s.Require().Len(result.Data, 1)
	s.Equal("old.txt", result.Data[0].Name)
	s.Nil(result.Data[0].ParentID)
	s.Equal(docs.ID, s.trashRoot(docs.ID))
}

// An item deleted earlier goes back to its old folder when that folder comes back in the same restore.
func (s *FileServiceTestSuite) TestRestore_WithItsFolderGoesBackIn() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	sub := s.createFolder(userID, docs.ID, "Sub")
	old, err := s.finishUpload(userID, sub.ID, "old.txt", "o")
	s.Require().NoError(err)
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{old.ID}))
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{docs.ID}))
	s.workTrashJobs()

	s.Require().NoError(svc.Restore(context.Background(), userID, []int64{old.ID, docs.ID}))

	result, err := svc.List(context.Background(), userID, newestFirst(sub.ID))
	s.Require().NoError(err)
	s.Equal([]string{"old.txt"}, fileNames(result.Data))
}

// One bad id undoes the rest. A live item and a row under a deleted folder are bad ids too.
func (s *FileServiceTestSuite) TestRestore_AllOrNone() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	a := s.seedFile(alice, "a.txt")
	live := s.seedFile(alice, "live.txt")
	docs := s.createFolder(alice, 0, "Docs")
	under, err := s.finishUpload(alice, docs.ID, "under.txt", "u")
	s.Require().NoError(err)
	s.Require().NoError(svc.Trash(context.Background(), alice, []int64{a, docs.ID}))
	s.workTrashJobs()
	bobFile := s.seedFile(bob, "bob.txt")
	s.Require().NoError(svc.Trash(context.Background(), bob, []int64{bobFile}))

	for _, bad := range []int64{999, live, under.ID, bobFile} {
		err := svc.Restore(context.Background(), alice, []int64{a, bad})

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusNotFound, status, bad)
		s.Equal("item not found", msg)
	}

	s.Equal(a, s.trashRoot(a))
	s.Equal(docs.ID, s.trashRoot(under.ID))
	s.Equal(bobFile, s.trashRoot(bobFile))
}

// A delete of a folder and a restore into it run at once. Either order is fine, but the item must never stay live inside the trash.
func (s *FileServiceTestSuite) TestRestore_ParallelWithTrashOfItsFolder() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService

	const n = 20
	folders := make([]int64, n)
	items := make([]int64, n)
	for i := range n {
		folder := s.createFolder(userID, 0, fmt.Sprintf("Docs %d", i))
		item, err := s.finishUpload(userID, folder.ID, "old.txt", "o")
		s.Require().NoError(err)
		s.Require().NoError(svc.Trash(context.Background(), userID, []int64{item.ID}))
		folders[i], items[i] = folder.ID, item.ID
	}

	errs := make([]error, 2*n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { errs[2*i] = svc.Trash(context.Background(), userID, []int64{folders[i]}) })
		wg.Go(func() { errs[2*i+1] = svc.Restore(context.Background(), userID, []int64{items[i]}) })
	}
	wg.Wait()

	for _, err := range errs {
		s.Require().NoError(err)
	}
	s.workTrashJobs()
	s.Zero(s.liveRowsInTrash(userID))
}

// Purge hides the groups at once and adds one job. The job deletes every row of the groups and their bytes.
func (s *FileServiceTestSuite) TestPurge_DeletesRowsAndBytes() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	sub := s.createFolder(userID, docs.ID, "Sub")
	a, err := s.finishUpload(userID, docs.ID, "a.txt", "a")
	s.Require().NoError(err)
	b, err := s.finishUpload(userID, sub.ID, "b.txt", "b")
	s.Require().NoError(err)
	gone, err := s.finishUpload(userID, 0, "gone.txt", "g")
	s.Require().NoError(err)
	keep, err := s.finishUpload(userID, 0, "keep.txt", "k")
	s.Require().NoError(err)
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{docs.ID, gone.ID}))
	s.workTrashJobs()
	purgedKeys := []string{s.storageKey(a.ID), s.storageKey(b.ID), s.storageKey(gone.ID)}

	s.Require().NoError(svc.Purge(context.Background(), userID, []int64{gone.ID, docs.ID, gone.ID}))

	trash, err := svc.ListTrash(context.Background(), userID, file.TrashQuery{Sort: file.SortByDate})
	s.Require().NoError(err)
	s.Empty(trash.Data)
	s.Equal([]jobs.PurgeGroupsArgs{{UserID: userID, RootIDs: []int64{docs.ID, gone.ID}}}, s.purgeJobs())

	s.workPurgeJobs()

	s.Equal(1, s.fileRows())
	for _, key := range purgedKeys {
		s.False(s.storedFileExists(key), key)
	}
	s.True(s.storedFileExists(s.storageKey(keep.ID)))
	s.Equal([]map[string]any{
		{"user_id": userID, "root_ids": []any{docs.ID, gone.ID}, "rows": int64(5), "files": int64(3), "moved_home": int64(0)},
	}, s.logs("purged trash groups"))
}

// Purge removes the thumbnail of a picture with its bytes.
func (s *FileServiceTestSuite) TestPurge_RemovesThumbnail() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	picture := s.encodePNG(testPicture(4, 4, 255))
	gone := s.upload(userID, 0, "gone.png", picture)
	keep := s.upload(userID, 0, "keep.png", picture)
	s.thumbnail(userID, gone.ID)
	s.thumbnail(userID, keep.ID)
	goneKey, keepKey := s.storageKey(gone.ID), s.storageKey(keep.ID)
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{gone.ID}))
	s.workTrashJobs()

	s.Require().NoError(svc.Purge(context.Background(), userID, []int64{gone.ID}))
	s.workPurgeJobs()

	s.NoFileExists(filepath.Join(s.thumbDir(), goneKey))
	s.FileExists(filepath.Join(s.thumbDir(), keepKey))
}

// One bad id undoes the rest. A live item, a row under a deleted folder and an item already marked are bad ids too.
func (s *FileServiceTestSuite) TestPurge_AllOrNone() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	a := s.seedFile(alice, "a.txt")
	live := s.seedFile(alice, "live.txt")
	marked := s.seedFile(alice, "marked.txt")
	docs := s.createFolder(alice, 0, "Docs")
	under, err := s.finishUpload(alice, docs.ID, "under.txt", "u")
	s.Require().NoError(err)
	s.Require().NoError(svc.Trash(context.Background(), alice, []int64{a, marked, docs.ID}))
	s.workTrashJobs()
	s.Require().NoError(svc.Purge(context.Background(), alice, []int64{marked}))
	bobFile := s.seedFile(bob, "bob.txt")
	s.Require().NoError(svc.Trash(context.Background(), bob, []int64{bobFile}))

	for _, bad := range []int64{999, live, under.ID, marked, bobFile} {
		err := svc.Purge(context.Background(), alice, []int64{a, bad})

		status, msg := apperr.Resolve(err)
		s.Equal(http.StatusNotFound, status, bad)
		s.Equal("item not found", msg)
	}

	trash, err := svc.ListTrash(context.Background(), alice, file.TrashQuery{Sort: file.SortByName})
	s.Require().NoError(err)
	s.Equal([]string{"Docs", "a.txt"}, trashNames(trash.Data))
	s.Equal([]jobs.PurgeGroupsArgs{{UserID: alice, RootIDs: []int64{marked}}}, s.purgeJobs())
}

// A group marked for a permanent delete cannot come back, even before the job runs.
func (s *FileServiceTestSuite) TestPurge_BlocksRestore() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	a := s.seedFile(userID, "a.txt")
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{a}))
	s.Require().NoError(svc.Purge(context.Background(), userID, []int64{a}))

	err := svc.Restore(context.Background(), userID, []int64{a})

	s.Equal("item not found", s.errorMessage(err))
	s.Equal(a, s.trashRoot(a))
}

// old.txt was deleted before Docs. The purge of Docs moves it to Home, and it stays in the trash with its bytes.
func (s *FileServiceTestSuite) TestPurgeGroups_MovesItemDeletedEarlierHome() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	old, err := s.finishUpload(userID, docs.ID, "old.txt", "o")
	s.Require().NoError(err)
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{old.ID}))
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{docs.ID}))
	s.workTrashJobs()

	s.Require().NoError(svc.Purge(context.Background(), userID, []int64{docs.ID}))
	s.workPurgeJobs()

	s.Equal([]map[string]any{
		{"user_id": userID, "root_ids": []any{docs.ID}, "rows": int64(1), "files": int64(0), "moved_home": int64(1)},
	}, s.logs("purged trash groups"))
	trash, err := svc.ListTrash(context.Background(), userID, file.TrashQuery{Sort: file.SortByDate})
	s.Require().NoError(err)
	s.Require().Len(trash.Data, 1)
	s.Equal("old.txt", trash.Data[0].Name)
	s.Nil(trash.Data[0].ParentID)
	s.True(s.storedFileExists(s.storageKey(old.ID)))

	s.Require().NoError(svc.Restore(context.Background(), userID, []int64{old.ID}))

	s.Equal(map[int64]string{old.ID: "old.txt"}, s.namesByID(userID))
}

// The trash job has not run, so the rows under Docs are still live. The purge job marks them first, then deletes them.
func (s *FileServiceTestSuite) TestPurgeGroups_BeforeTrashJobEnds() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	sub := s.createFolder(userID, docs.ID, "Sub")
	x, err := s.finishUpload(userID, sub.ID, "x.txt", "x")
	s.Require().NoError(err)
	key := s.storageKey(x.ID)
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{docs.ID}))
	s.Require().NoError(svc.Purge(context.Background(), userID, []int64{docs.ID}))

	s.workPurgeJobs()

	s.Zero(s.fileRows())
	s.False(s.storedFileExists(key))
	s.workTrashJobs()
}

// Big holds 1500 files and 1001 folders with a file each, and Sub 1 holds one more level. So the leaves take
// several batches, and each folder level comes after the level under it. The seeded rows have no bytes on disk,
// which a missing file must not stop.
func (s *FileServiceTestSuite) TestPurgeGroups_ManyBatchesAndLevels() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	big := s.createFolder(userID, 0, "Big")
	for _, insert := range []string{
		`INSERT INTO files (user_id, parent_id, name, storage_key, content_type, size_bytes)
		SELECT $1, $2, 'f' || i || '.txt', 'big-' || i, 'text/plain', 1 FROM generate_series(1, 1500) i`,
		`INSERT INTO files (user_id, parent_id, name, content_type, size_bytes)
		SELECT $1, $2, 'Sub ' || i, '', 0 FROM generate_series(1, 1001) i`,
		`INSERT INTO files (user_id, parent_id, name, storage_key, content_type, size_bytes)
		SELECT user_id, id, 'in.txt', 'sub-' || id, 'text/plain', 1 FROM files
		WHERE user_id = $1 AND parent_id = $2 AND storage_key IS NULL`,
	} {
		_, err := s.TC.DB.ExecContext(context.Background(), insert, userID, big.ID)
		s.Require().NoError(err)
	}
	path, err := svc.ResolveFolder(context.Background(), userID, []string{"Big", "Sub 1"})
	s.Require().NoError(err)
	deep := s.createFolder(userID, path.Path[1].ID, "Deep")
	inDeep, err := s.finishUpload(userID, deep.ID, "deep.txt", "d")
	s.Require().NoError(err)
	key := s.storageKey(inDeep.ID)
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{big.ID}))
	s.workTrashJobs()

	s.Require().NoError(svc.Purge(context.Background(), userID, []int64{big.ID}))
	s.workPurgeJobs()

	s.Zero(s.fileRows())
	s.False(s.storedFileExists(key))
}

// The first run removed the bytes of x.txt and stopped before the delete. The next run finishes.
func (s *FileServiceTestSuite) TestPurgeGroups_RunAfterBytesGoneFinishes() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	x, err := s.finishUpload(userID, docs.ID, "x.txt", "x")
	s.Require().NoError(err)
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{docs.ID}))
	s.workTrashJobs()
	s.Require().NoError(svc.Purge(context.Background(), userID, []int64{docs.ID}))
	s.Require().NoError(os.Remove(filepath.Join(s.TC.UploadDir, s.storageKey(x.ID))))

	s.workPurgeJobs()

	s.Zero(s.fileRows())
}

// A job for a group the user did not purge deletes nothing.
func (s *FileServiceTestSuite) TestPurgeGroups_SkipsGroupsNotMarked() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	a, err := s.finishUpload(userID, 0, "a.txt", "a")
	s.Require().NoError(err)
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{a.ID}))

	s.Require().NoError(svc.PurgeGroups(context.Background(), userID, []int64{a.ID}))

	s.Equal(a.ID, s.trashRoot(a.ID))
	s.True(s.storedFileExists(s.storageKey(a.ID)))
}

// a.txt and Docs are in the trash longer than 30 days, b.txt is not. The sweep hides a.txt and Docs at once,
// and their purge job deletes them. Bob's old item gets a job of its own.
func (s *FileServiceTestSuite) TestSweepTrash_PurgesExpiredItems() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	a, err := s.finishUpload(alice, 0, "a.txt", "a")
	s.Require().NoError(err)
	b, err := s.finishUpload(alice, 0, "b.txt", "b")
	s.Require().NoError(err)
	docs := s.createFolder(alice, 0, "Docs")
	in, err := s.finishUpload(alice, docs.ID, "in.txt", "i")
	s.Require().NoError(err)
	old := s.seedFile(bob, "old.txt")
	s.Require().NoError(svc.Trash(context.Background(), alice, []int64{a.ID, b.ID, docs.ID}))
	s.Require().NoError(svc.Trash(context.Background(), bob, []int64{old}))
	s.workTrashJobs()
	for _, id := range []int64{a.ID, docs.ID, old} {
		s.moveTrashedAt(id, 31*24)
	}
	s.moveTrashedAt(b.ID, 29*24)
	purgedKeys := []string{s.storageKey(a.ID), s.storageKey(in.ID)}

	s.Require().NoError(svc.SweepTrash(context.Background()))

	trash, err := svc.ListTrash(context.Background(), alice, file.TrashQuery{Sort: file.SortByName})
	s.Require().NoError(err)
	s.Equal([]string{"b.txt"}, trashNames(trash.Data))
	s.Equal([]jobs.PurgeGroupsArgs{
		{UserID: alice, RootIDs: []int64{a.ID, docs.ID}},
		{UserID: bob, RootIDs: []int64{old}},
	}, s.purgeJobs())
	s.Equal([]map[string]any{
		{"user_id": alice, "root_ids": []any{a.ID, docs.ID}},
		{"user_id": bob, "root_ids": []any{old}},
	}, s.logs("marked expired trash for purge"))

	s.workPurgeJobs()

	s.Equal(1, s.fileRows())
	for _, key := range purgedKeys {
		s.False(s.storedFileExists(key), key)
	}
	s.True(s.storedFileExists(s.storageKey(b.ID)))
}

// 300 items are in the trash longer than 30 days, so the sweep marks them in two batches, with a job each.
func (s *FileServiceTestSuite) TestSweepTrash_ManyBatches() {
	userID := s.seedUser()
	for _, stmt := range []string{
		`INSERT INTO files (user_id, name, storage_key, content_type, size_bytes)
		SELECT $1, 'f' || i || '.txt', 'old-' || i, 'text/plain', 1 FROM generate_series(1, 300) i`,
		`UPDATE files SET trashed_at = NOW() - INTERVAL '31 days', trash_root_id = id WHERE user_id = $1`,
	} {
		_, err := s.TC.DB.ExecContext(context.Background(), stmt, userID)
		s.Require().NoError(err)
	}

	s.Require().NoError(s.TC.Services.FileService.SweepTrash(context.Background()))

	batches := s.purgeJobs()
	s.Require().Len(batches, 2)
	s.Len(batches[0].RootIDs, 250)
	s.Len(batches[1].RootIDs, 50)

	s.workPurgeJobs()

	s.Zero(s.fileRows())
}

// a.txt and b.txt were marked a day ago, and their job is still alive, so the sweep adds no copy. Once River gives
// up on that job, the sweep adds it again. c.txt was marked just now, so the sweep leaves it to its own job.
func (s *FileServiceTestSuite) TestSweepTrash_AddsPurgeJobAgainAfterRiverGivesUp() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	a := s.seedFile(userID, "a.txt")
	b := s.seedFile(userID, "b.txt")
	c := s.seedFile(userID, "c.txt")
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{a, b, c}))
	s.Require().NoError(svc.Purge(context.Background(), userID, []int64{a, b}))
	s.Require().NoError(svc.Purge(context.Background(), userID, []int64{c}))
	_, err := s.TC.DB.ExecContext(context.Background(),
		`UPDATE files SET purge_requested_at = purge_requested_at - INTERVAL '25 hours' WHERE id IN ($1, $2)`, a, b)
	s.Require().NoError(err)
	stuck := jobs.PurgeGroupsArgs{UserID: userID, RootIDs: []int64{a, b}}
	fresh := jobs.PurgeGroupsArgs{UserID: userID, RootIDs: []int64{c}}

	s.Require().NoError(svc.SweepTrash(context.Background()))
	s.Equal([]jobs.PurgeGroupsArgs{stuck, fresh}, s.purgeJobs())
	s.Empty(s.logs("added the purge job again"))

	_, err = s.TC.DB.ExecContext(context.Background(), `
		UPDATE river_job SET state = 'discarded', finalized_at = NOW()
		WHERE id = (SELECT MIN(id) FROM river_job WHERE kind = 'purge_groups')`)
	s.Require().NoError(err)

	s.Require().NoError(svc.SweepTrash(context.Background()))
	s.Equal([]jobs.PurgeGroupsArgs{stuck, fresh, stuck}, s.purgeJobs())
	s.Equal([]map[string]any{{"user_id": userID, "root_ids": []any{a, b}}}, s.logs("added the purge job again"))

	s.workPurgeJobs()

	s.Zero(s.fileRows())
}

// Only files older than a day that no row points to go: an orphan, and the temp file of an upload that never ended.
// A newer orphan can still get its row. A trashed file still has its row. A name not in key form, or a dir, is not ours.
func (s *FileServiceTestSuite) TestSweepOrphans_RemovesOldFilesWithNoRow() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	live, err := s.finishUpload(userID, 0, "live.txt", "l")
	s.Require().NoError(err)
	trashed, err := s.finishUpload(userID, 0, "trashed.txt", "t")
	s.Require().NoError(err)
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{trashed.ID}))
	kept := []string{s.storageKey(live.ID), s.storageKey(trashed.ID)}
	for _, key := range kept {
		s.ageStoredFile(key, 25)
	}
	orphan := s.writeStoredFile(fmt.Sprintf("%032x", 1), 25)
	temp := s.writeStoredFile(fmt.Sprintf(".upload-%032x", 2), 25)
	kept = append(kept,
		s.writeStoredFile(fmt.Sprintf("%032x", 3), 23),
		s.writeStoredFile(fmt.Sprintf(".upload-%032x", 4), 23),
		s.writeStoredFile("notes.txt", 25),
		s.writeStoredFile(strings.Repeat("A", 32), 25),
	)
	dir := fmt.Sprintf("%032x", 5)
	s.Require().NoError(os.Mkdir(filepath.Join(s.TC.UploadDir, dir), 0o750))
	s.ageStoredFile(dir, 25)
	kept = append(kept, dir)

	s.Require().NoError(svc.SweepOrphans(context.Background()))

	s.False(s.storedFileExists(orphan))
	s.False(s.storedFileExists(temp))
	for _, name := range kept {
		s.True(s.storedFileExists(name), name)
	}
	removed := map[string]any{}
	for _, fields := range s.logs("removed an orphan file") {
		removed[fields["name"].(string)] = fields["size_bytes"]
	}
	s.Equal(map[string]any{orphan: int64(len("orphan")), temp: int64(len("orphan"))}, removed)
}

// An upload is stale after 24 hours with no new bytes. A half left of one goes too, and a name the sweep does not
// know stays.
func (s *FileServiceTestSuite) TestSweepUploads_RemovesStaleUploads() {
	userID := s.seedUser()
	stale := s.startPartial(userID, 100, 40, 25)
	fresh := s.startPartial(userID, 100, 40, 23)
	loneData := s.startPartial(userID, 100, 1, 25)
	s.Require().NoError(os.Remove(s.partialPath(loneData + ".info")))
	loneInfo := s.startPartial(userID, 100, 0, 25)
	s.Require().NoError(os.Remove(s.partialPath(loneInfo)))
	s.Require().NoError(os.WriteFile(s.partialPath("notes.txt"), []byte("x"), 0o640))
	s.T().Cleanup(func() { _ = os.Remove(s.partialPath("notes.txt")) })
	s.agePartial("notes.txt", 25)
	before := s.partialEntries()

	s.Require().NoError(s.TC.Services.FileService.SweepUploads(context.Background()))

	var want []string
	for _, name := range before {
		if !strings.HasPrefix(name, stale) && !strings.HasPrefix(name, loneData) && !strings.HasPrefix(name, loneInfo) {
			want = append(want, name)
		}
	}
	s.Equal(want, s.partialEntries())
	s.Contains(want, fresh)
	s.Contains(want, fresh+".info")
	s.Contains(want, "notes.txt")

	removed := map[string][3]any{}
	for _, fields := range s.logs("removed a stale upload") {
		removed[fields["upload_id"].(string)] = [3]any{fields["user_id"], fields["size_bytes"], fields["length_bytes"]}
	}
	s.Equal(map[string][3]any{
		stale:    {userID, int64(40), int64(100)},
		loneData: {int64(0), int64(1), int64(0)},
		loneInfo: {userID, int64(0), int64(100)},
	}, removed)
}

// The orphan sweep reads only the top of the upload dir, so it never removes a partial upload.
func (s *FileServiceTestSuite) TestSweepOrphans_SkipsPartialDir() {
	userID := s.seedUser()
	s.upload(userID, 0, "live.txt", "l")
	id := s.startPartial(userID, 100, 40, 25)

	s.Require().NoError(s.TC.Services.FileService.SweepOrphans(context.Background()))

	s.FileExists(s.partialPath(id))
	s.Empty(s.logs("removed an orphan file"))
}

// The orphan sweep reads only the top of the upload dir, so it never removes a zip.
func (s *FileServiceTestSuite) TestSweepOrphans_SkipsZipDir() {
	userID := s.seedUser()
	s.upload(userID, 0, "live.txt", "l")
	stray := s.writeZipFile(fmt.Sprintf("%032x", 1), 25)

	s.Require().NoError(s.TC.Services.FileService.SweepOrphans(context.Background()))

	s.Equal([]string{stray}, s.zipFiles())
	s.Empty(s.logs("removed an orphan file"))
}

// The sweep removes an old thumbnail whose picture has no row. A live picture's thumbnail and a new one stay.
func (s *FileServiceTestSuite) TestSweepOrphans_RemovesOldThumbnails() {
	userID := s.seedUser()
	f := s.upload(userID, 0, "picture.png", s.encodePNG(testPicture(4, 4, 255)))
	s.thumbnail(userID, f.ID)
	live, orphan, fresh := s.storageKey(f.ID), fmt.Sprintf("%032x", 1), fmt.Sprintf("%032x", 2)
	for _, name := range []string{orphan, fresh} {
		s.Require().NoError(os.WriteFile(filepath.Join(s.thumbDir(), name), []byte("thumb"), 0o640))
	}
	for name, hoursAgo := range map[string]int{live: 25, orphan: 25, fresh: 23} {
		past := time.Now().Add(-time.Duration(hoursAgo) * time.Hour)
		s.Require().NoError(os.Chtimes(filepath.Join(s.thumbDir(), name), past, past))
	}

	s.Require().NoError(s.TC.Services.FileService.SweepOrphans(context.Background()))

	s.NoFileExists(filepath.Join(s.thumbDir(), orphan))
	s.FileExists(filepath.Join(s.thumbDir(), live))
	s.FileExists(filepath.Join(s.thumbDir(), fresh))
	removed := s.logs("removed an orphan thumbnail")
	s.Require().Len(removed, 1)
	s.Equal(orphan, removed[0]["name"])
}

// A ready zip goes 24 hours after it got ready. Any other download goes 24 hours after it was asked for: a failed one,
// and one still building, whose job is gone for good.
func (s *FileServiceTestSuite) TestSweepDownloads_RemovesExpired() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	s.upload(userID, docs.ID, "a.txt", "a")
	request := func() int64 {
		dl, err := svc.RequestZip(context.Background(), userID, []int64{docs.ID})
		s.Require().NoError(err)
		return dl.ID
	}
	old, recent := request(), request()
	s.workZipJobs()
	failed, stuck, building := request(), request(), request()
	s.Require().NoError(svc.FailZip(context.Background(), userID, failed))
	oldKey, recentKey := s.downloadZipKey(old), s.downloadZipKey(recent)
	s.ageDownload(old, 26, 25)
	s.ageDownload(recent, 30, 23)
	s.ageDownload(failed, 25, 0)
	s.ageDownload(stuck, 25, 0)

	s.Require().NoError(svc.SweepDownloads(context.Background()))

	s.Equal([]int64{recent, building}, s.downloadIDs())
	s.Equal([]string{recentKey}, s.zipFiles())
	s.NotEqual(oldKey, recentKey)
	logs := s.logs("removed expired downloads")
	s.Require().Len(logs, 1)
	s.ElementsMatch([]any{old, failed, stuck}, logs[0]["download_ids"])
	s.Equal(int64(1), logs[0]["zips"])
}

func (s *FileServiceTestSuite) TestSweepDownloads_ManyBatches() {
	userID := s.seedUser()
	_, err := s.TC.DB.ExecContext(context.Background(), `
		INSERT INTO downloads (user_id, name, item_ids, created_at)
		SELECT $1, 'Docs.zip', '{1}', NOW() - INTERVAL '25 hours' FROM generate_series(1, 2500)`, userID)
	s.Require().NoError(err)

	s.Require().NoError(s.TC.Services.FileService.SweepDownloads(context.Background()))

	s.Equal(0, s.downloadRows())
	s.Len(s.logs("removed expired downloads"), 3)
}

// A file in the zip dir with no row goes after 24 hours, and so does the temp file of a stopped build. A zip with a
// row, a newer file and a name not in key form stay.
func (s *FileServiceTestSuite) TestSweepDownloads_RemovesStrayFiles() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	s.upload(userID, docs.ID, "a.txt", "a")
	_, err := svc.RequestZip(context.Background(), userID, []int64{docs.ID})
	s.Require().NoError(err)
	s.workZipJobs()
	key := s.zipFiles()[0]
	s.ageZipFile(key, 25)
	stray := s.writeZipFile(fmt.Sprintf("%032x", 1), 25)
	temp := s.writeZipFile(fmt.Sprintf(".upload-%032x", 2), 25)
	kept := []string{key, s.writeZipFile(fmt.Sprintf("%032x", 3), 23), s.writeZipFile("notes.txt", 25)}

	s.Require().NoError(svc.SweepDownloads(context.Background()))

	s.ElementsMatch(kept, s.zipFiles())
	removed := map[string]any{}
	for _, fields := range s.logs("removed a stray zip file") {
		removed[fields["name"].(string)] = fields["size_bytes"]
	}
	s.Equal(map[string]any{stray: int64(len("stray")), temp: int64(len("stray"))}, removed)
}

// The zip dir comes with the first zip, so a sweep before it finds nothing to do.
func (s *FileServiceTestSuite) TestSweepDownloads_NoZipDir() {
	s.Require().NoError(os.RemoveAll(filepath.Join(s.TC.UploadDir, utils.ZipDir)))

	s.Require().NoError(s.TC.Services.FileService.SweepDownloads(context.Background()))
}

// No row has bytes, so the app may point at a new database by mistake. The sweep keeps every file.
func (s *FileServiceTestSuite) TestSweepOrphans_SkipsWhenNoRowHasBytes() {
	userID := s.seedUser()
	s.createFolder(userID, 0, "Docs")
	orphan := s.writeStoredFile(fmt.Sprintf("%032x", 6), 25)

	s.Require().NoError(s.TC.Services.FileService.SweepOrphans(context.Background()))

	s.True(s.storedFileExists(orphan))
	s.Len(s.logs("skipped the orphan sweep: no row has a stored file"), 1)
}

// 1500 orphans take 2 batches. The file with a row stays.
func (s *FileServiceTestSuite) TestSweepOrphans_ManyBatches() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	live, err := s.finishUpload(userID, 0, "live.txt", "l")
	s.Require().NoError(err)
	s.ageStoredFile(s.storageKey(live.ID), 25)
	var orphans []string
	for i := range 1500 {
		orphans = append(orphans, s.writeStoredFile(fmt.Sprintf("%032x", 1000+i), 25))
	}

	s.Require().NoError(svc.SweepOrphans(context.Background()))

	for _, name := range orphans {
		s.False(s.storedFileExists(name), name)
	}
	s.True(s.storedFileExists(s.storageKey(live.ID)))
	s.Len(s.logs("removed an orphan file"), 1500)
}

func (s *FileServiceTestSuite) createFolder(userID, parentID int64, name string) *file.File {
	f, err := s.TC.Services.FileService.CreateFolder(context.Background(), userID, parentID, name)
	s.Require().NoError(err)
	return f
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
	return s.seedFileWith(userID, name, "text/plain; charset=utf-8", 1, 0)
}

// seedFileWith moves created_at back by hoursAgo.
func (s *FileServiceTestSuite) seedFileWith(userID int64, name, contentType string, size int64, hoursAgo int) int64 {
	f, err := s.TC.Queries.CreateFile(context.Background(), database.CreateFileParams{
		UserID:      userID,
		Name:        name,
		StorageKey:  sql.NullString{String: fmt.Sprintf("%d-%s", userID, name), Valid: true},
		ContentType: contentType,
		SizeBytes:   size,
	})
	s.Require().NoError(err)

	s.moveCreatedAt(f.ID, hoursAgo)
	return f.ID
}

func (s *FileServiceTestSuite) moveCreatedAt(id int64, hoursAgo int) {
	_, err := s.TC.DB.ExecContext(context.Background(),
		`UPDATE files SET created_at = created_at - make_interval(hours => $1) WHERE id = $2`, hoursAgo, id)
	s.Require().NoError(err)
}

func newestFirst(parentID int64) file.ListQuery {
	return file.ListQuery{ParentID: parentID, Sort: file.SortByDate, Desc: true}
}

func (s *FileServiceTestSuite) walkPages(userID int64, q file.ListQuery) [][]string {
	var pages [][]string
	for range 50 {
		result, err := s.TC.Services.FileService.List(context.Background(), userID, q)
		s.Require().NoError(err)

		pages = append(pages, fileNames(result.Data))
		if result.NextCursor == nil {
			return pages
		}
		q.Cursor = *result.NextCursor
	}
	s.FailNow("the pages never end")
	return nil
}

func cursorOf(raw string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// fileSizes maps each item's name to its size.
func fileSizes(files []*file.File) map[string]int64 {
	sizes := make(map[string]int64, len(files))
	for _, f := range files {
		sizes[f.Name] = f.SizeBytes
	}
	return sizes
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

// trashRoot reads trash_root_id, and fails if the row is not in the trash.
func (s *FileServiceTestSuite) trashRoot(id int64) int64 {
	var root sql.NullInt64
	var trashed bool
	err := s.TC.DB.QueryRowContext(context.Background(),
		`SELECT trash_root_id, trashed_at IS NOT NULL FROM files WHERE id = $1`, id).Scan(&root, &trashed)
	s.Require().NoError(err)
	s.Require().True(root.Valid && trashed, "row %d is not in the trash", id)
	return root.Int64
}

func (s *FileServiceTestSuite) errorMessage(err error) string {
	s.Require().Error(err)
	_, msg := apperr.Resolve(err)
	return msg
}

func (s *FileServiceTestSuite) moveTrashedAt(rootID int64, hoursAgo int) {
	_, err := s.TC.DB.ExecContext(context.Background(),
		`UPDATE files SET trashed_at = trashed_at - make_interval(hours => $1) WHERE trash_root_id = $2`, hoursAgo, rootID)
	s.Require().NoError(err)
}

func (s *FileServiceTestSuite) walkTrash(userID int64, q file.TrashQuery) [][]string {
	var pages [][]string
	for range 50 {
		result, err := s.TC.Services.FileService.ListTrash(context.Background(), userID, q)
		s.Require().NoError(err)

		pages = append(pages, trashNames(result.Data))
		if result.NextCursor == nil {
			return pages
		}
		q.Cursor = *result.NextCursor
	}
	s.FailNow("the pages never end")
	return nil
}

func (s *FileServiceTestSuite) walkFavorites(userID int64, q file.FavoriteQuery) [][]string {
	var pages [][]string
	for range 50 {
		result, err := s.TC.Services.FileService.ListFavorites(context.Background(), userID, q)
		s.Require().NoError(err)

		pages = append(pages, favoriteNames(result.Data))
		if result.NextCursor == nil {
			return pages
		}
		q.Cursor = *result.NextCursor
	}
	s.FailNow("the pages never end")
	return nil
}

func favoriteNames(items []*file.Favorite) []string {
	names := make([]string, len(items))
	for i, item := range items {
		names[i] = item.Name
	}
	return names
}

func favoritePaths(items []*file.Favorite) map[string][]string {
	paths := map[string][]string{}
	for _, item := range items {
		paths[item.Name] = item.Path
	}
	return paths
}

// recentEntries names each recent file by its path, such as "Docs/report.txt".
func recentEntries(items []*file.RecentFile) []string {
	entries := make([]string, len(items))
	for i, item := range items {
		entries[i] = strings.Join(append(slices.Clone(item.Path), item.Name), "/")
	}
	return entries
}

func trashNames(items []*file.TrashedFile) []string {
	names := make([]string, len(items))
	for i, item := range items {
		names[i] = item.Name
	}
	return names
}

// liveRowsInTrash counts live rows whose folder is in the trash. Nobody can reach such a row.
func (s *FileServiceTestSuite) liveRowsInTrash(userID int64) int {
	var n int
	err := s.TC.DB.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM files c JOIN files p ON p.id = c.parent_id
		WHERE c.user_id = $1 AND c.trashed_at IS NULL AND p.trashed_at IS NOT NULL`, userID).Scan(&n)
	s.Require().NoError(err)
	return n
}

// logs reads the fields of the logs with msg, oldest first.
func (s *FileServiceTestSuite) logs(msg string) []map[string]any {
	var all []map[string]any
	for _, entry := range s.TC.Logs.FilterMessage(msg).All() {
		all = append(all, entry.ContextMap())
	}
	return all
}

// queuedJobs reads the jobs of one kind that wait in the queue.
func queuedJobs[T river.JobArgs](s *FileServiceTestSuite) []T {
	var kind T
	rows, err := s.TC.DB.QueryContext(context.Background(),
		`SELECT args FROM river_job WHERE kind = $1 ORDER BY id`, kind.Kind())
	s.Require().NoError(err)
	defer func() { _ = rows.Close() }()

	var all []T
	for rows.Next() {
		var raw []byte
		s.Require().NoError(rows.Scan(&raw))
		var args T
		s.Require().NoError(json.Unmarshal(raw, &args))
		all = append(all, args)
	}
	s.Require().NoError(rows.Err())
	return all
}

// workJobs runs the waiting jobs of one kind through the worker, the way River does, then removes them.
// Each run is a first attempt.
func workJobs[T river.JobArgs](s *FileServiceTestSuite, worker river.Worker[T]) {
	var kind T
	for _, args := range queuedJobs[T](s) {
		s.Require().NoError(worker.Work(context.Background(), &river.Job[T]{
			JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 3},
			Args:   args,
		}))
	}
	_, err := s.TC.DB.ExecContext(context.Background(), `DELETE FROM river_job WHERE kind = $1`, kind.Kind())
	s.Require().NoError(err)
}

func (s *FileServiceTestSuite) trashJobs() []jobs.TrashSubtreesArgs {
	return queuedJobs[jobs.TrashSubtreesArgs](s)
}

func (s *FileServiceTestSuite) purgeJobs() []jobs.PurgeGroupsArgs {
	return queuedJobs[jobs.PurgeGroupsArgs](s)
}

func (s *FileServiceTestSuite) workTrashJobs() {
	workJobs[jobs.TrashSubtreesArgs](s, jobs.NewTrashSubtreesWorker(s.TC.Services.FileService))
}

func (s *FileServiceTestSuite) workPurgeJobs() {
	workJobs[jobs.PurgeGroupsArgs](s, jobs.NewPurgeGroupsWorker(s.TC.Services.FileService))
}

func (s *FileServiceTestSuite) copyJobs() []jobs.CopyItemsArgs {
	return queuedJobs[jobs.CopyItemsArgs](s)
}

func (s *FileServiceTestSuite) workCopyJobs() {
	workJobs[jobs.CopyItemsArgs](s, jobs.NewCopyItemsWorker(s.TC.Services.FileService))
}

func (s *FileServiceTestSuite) zipJobs() []jobs.BuildZipArgs {
	return queuedJobs[jobs.BuildZipArgs](s)
}

// workZipJobs builds the waiting zips. The zip dir goes when the test ends, so later tests start with none.
func (s *FileServiceTestSuite) workZipJobs() {
	s.T().Cleanup(func() { _ = os.RemoveAll(filepath.Join(s.TC.UploadDir, utils.ZipDir)) })
	workJobs[jobs.BuildZipArgs](s, jobs.NewBuildZipWorker(s.TC.Services.FileService))
}

// zipEntries reads a ready zip: "path/" for a folder and "path=content" for a file, in byte order.
// Every entry must use Store.
func (s *FileServiceTestSuite) zipEntries(userID, downloadID int64) []string {
	content, err := s.TC.Services.FileService.OpenZip(context.Background(), userID, downloadID)
	s.Require().NoError(err)
	defer func() { _ = content.Content.Close() }()
	raw, err := io.ReadAll(content.Content)
	s.Require().NoError(err)
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	s.Require().NoError(err)

	entries := []string{}
	for _, f := range zr.File {
		s.Equal(zip.Store, f.Method, f.Name)
		if strings.HasSuffix(f.Name, "/") {
			entries = append(entries, f.Name)
			continue
		}
		rc, err := f.Open()
		s.Require().NoError(err)
		body, err := io.ReadAll(rc)
		s.Require().NoError(err)
		s.Require().NoError(rc.Close())
		entries = append(entries, f.Name+"="+string(body))
	}
	slices.Sort(entries)
	return entries
}

// zipFiles lists the files in the zip dir, temp files too.
func (s *FileServiceTestSuite) zipFiles() []string {
	entries, err := os.ReadDir(filepath.Join(s.TC.UploadDir, utils.ZipDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	s.Require().NoError(err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// endDownload marks a download failed, so it no longer counts toward the limit on building zips.
func (s *FileServiceTestSuite) endDownload(id int64) {
	_, err := s.TC.DB.ExecContext(context.Background(), `UPDATE downloads SET error = 'ended by the test' WHERE id = $1`, id)
	s.Require().NoError(err)
}

// writeZipFile writes a file into the zip dir and moves its modified time back by hoursAgo. The zip dir goes when the
// test ends.
func (s *FileServiceTestSuite) writeZipFile(name string, hoursAgo int) string {
	dir := filepath.Join(s.TC.UploadDir, utils.ZipDir)
	s.Require().NoError(os.MkdirAll(dir, 0o750))
	s.T().Cleanup(func() { _ = os.RemoveAll(dir) })
	s.Require().NoError(os.WriteFile(filepath.Join(dir, name), []byte("stray"), 0o640))
	s.ageZipFile(name, hoursAgo)
	return name
}

func (s *FileServiceTestSuite) ageZipFile(name string, hoursAgo int) {
	past := time.Now().Add(-time.Duration(hoursAgo) * time.Hour)
	s.Require().NoError(os.Chtimes(filepath.Join(s.TC.UploadDir, utils.ZipDir, name), past, past))
}

// ageDownload sets created_at and, on a ready download, ready_at to that many hours ago.
func (s *FileServiceTestSuite) ageDownload(id int64, createdHoursAgo, readyHoursAgo int) {
	_, err := s.TC.DB.ExecContext(context.Background(), `
		UPDATE downloads
		SET created_at = NOW() - make_interval(hours => $1),
		    ready_at = CASE WHEN ready_at IS NULL THEN NULL ELSE NOW() - make_interval(hours => $2) END
		WHERE id = $3`, createdHoursAgo, readyHoursAgo, id)
	s.Require().NoError(err)
}

func (s *FileServiceTestSuite) downloadZipKey(id int64) string {
	var key string
	s.Require().NoError(s.TC.DB.QueryRowContext(context.Background(),
		`SELECT storage_key FROM downloads WHERE id = $1`, id).Scan(&key))
	return key
}

// download finds one download in the user's list, the same way the client reads it.
func (s *FileServiceTestSuite) download(userID, id int64) *file.Download {
	list, err := s.TC.Services.FileService.ListDownloads(context.Background(), userID)
	s.Require().NoError(err)
	for _, dl := range list {
		if dl.ID == id {
			return dl
		}
	}
	s.FailNow("the download is not in the list", id)
	return nil
}

func (s *FileServiceTestSuite) downloadIDs() []int64 {
	rows, err := s.TC.DB.QueryContext(context.Background(), `SELECT id FROM downloads ORDER BY id`)
	s.Require().NoError(err)
	defer func() { _ = rows.Close() }()
	ids := []int64{}
	for rows.Next() {
		var id int64
		s.Require().NoError(rows.Scan(&id))
		ids = append(ids, id)
	}
	s.Require().NoError(rows.Err())
	return ids
}

// activity reads an item's whole audit log, newest first. Each entry is the metadata with the event added.
func (s *FileServiceTestSuite) activity(userID, id int64) []map[string]any {
	page, err := s.TC.Services.FileService.ListActivity(context.Background(), userID, id, file.ActivityQuery{PageSize: 100})
	s.Require().NoError(err)
	entries := []map[string]any{}
	for _, entry := range page.Data {
		meta := map[string]any{}
		s.Require().NoError(json.Unmarshal(entry.Metadata, &meta))
		meta["event"] = string(entry.Event)
		entries = append(entries, meta)
	}
	return entries
}

// childID finds a live item by name in a folder.
func (s *FileServiceTestSuite) childID(userID, parentID int64, name string) int64 {
	var id int64
	s.Require().NoError(s.TC.DB.QueryRowContext(context.Background(),
		`SELECT id FROM files WHERE user_id = $1 AND COALESCE(parent_id, 0) = $2 AND name = $3 AND trashed_at IS NULL`,
		userID, parentID, name).Scan(&id))
	return id
}

// favoriteMarks maps each item in one folder to its favorite mark.
func (s *FileServiceTestSuite) favoriteMarks(userID, parentID int64) map[string]bool {
	result, err := s.TC.Services.FileService.List(context.Background(), userID, newestFirst(parentID))
	s.Require().NoError(err)
	marks := map[string]bool{}
	for _, f := range result.Data {
		marks[f.Name] = f.IsFavorite
	}
	return marks
}

func hiddenMarks(files []*file.File) map[string]bool {
	marks := map[string]bool{}
	for _, f := range files {
		marks[f.Name] = f.IsHidden
	}
	return marks
}

func (s *FileServiceTestSuite) favoriteRows() int {
	var n int
	s.Require().NoError(s.TC.DB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM favorites`).Scan(&n))
	return n
}

func (s *FileServiceTestSuite) downloadRows() int {
	var n int
	s.Require().NoError(s.TC.DB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM downloads`).Scan(&n))
	return n
}

func (s *FileServiceTestSuite) upload(userID, parentID int64, name, content string) *file.File {
	f, err := s.finishUpload(userID, parentID, name, content)
	s.Require().NoError(err)
	return f
}

// finishUpload adds a file the way a chunked upload does: the bytes wait in the partial dir, then FinishUpload.
func (s *FileServiceTestSuite) finishUpload(userID, parentID int64, name, content string) (*file.File, error) {
	return s.TC.Services.FileService.FinishUpload(context.Background(), userID, parentID, name, s.writePartial(content))
}

// startPartial starts a chunked upload of length bytes through tusd's file store, writes the first written bytes, and
// moves both its files back by hoursAgo. It returns the upload ID.
func (s *FileServiceTestSuite) startPartial(userID, length, written int64, hoursAgo int) string {
	store := filestore.New(filepath.Join(s.TC.UploadDir, uploads.PartialDir))
	s.Require().NoError(os.MkdirAll(store.Path, 0o750))
	id, err := utils.NewStorageKey()
	s.Require().NoError(err)

	upload, err := store.NewUpload(context.Background(), tusd.FileInfo{
		ID:       id,
		Size:     length,
		MetaData: tusd.MetaData{"filename": "film.mp4", "user_id": strconv.FormatInt(userID, 10)},
	})
	s.Require().NoError(err)
	_, err = upload.WriteChunk(context.Background(), 0, bytes.NewReader(make([]byte, written)))
	s.Require().NoError(err)

	s.agePartial(id, hoursAgo)
	s.agePartial(id+".info", hoursAgo)
	s.T().Cleanup(func() {
		_ = os.Remove(s.partialPath(id))
		_ = os.Remove(s.partialPath(id + ".info"))
	})
	return id
}

func (s *FileServiceTestSuite) partialPath(name string) string {
	return filepath.Join(s.TC.UploadDir, uploads.PartialDir, name)
}

func (s *FileServiceTestSuite) agePartial(name string, hoursAgo int) {
	at := time.Now().Add(-time.Duration(hoursAgo) * time.Hour)
	s.Require().NoError(os.Chtimes(s.partialPath(name), at, at))
}

// partialEntries lists the partial upload dir, sorted by name.
func (s *FileServiceTestSuite) partialEntries() []string {
	entries, err := os.ReadDir(filepath.Join(s.TC.UploadDir, uploads.PartialDir))
	s.Require().NoError(err)
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}

// writePartial writes the bytes of a finished chunked upload where tusd keeps them, and returns their path.
func (s *FileServiceTestSuite) writePartial(content string) string {
	dir := filepath.Join(s.TC.UploadDir, uploads.PartialDir)
	s.Require().NoError(os.MkdirAll(dir, 0o750))
	key, err := utils.NewStorageKey()
	s.Require().NoError(err)
	path := filepath.Join(dir, key)
	s.Require().NoError(os.WriteFile(path, []byte(content), 0o640))
	s.T().Cleanup(func() { _ = os.Remove(path) })
	return path
}

// writeStoredFile writes a file into the upload dir and moves its modified time back by hoursAgo.
func (s *FileServiceTestSuite) writeStoredFile(name string, hoursAgo int) string {
	s.Require().NoError(os.WriteFile(filepath.Join(s.TC.UploadDir, name), []byte("orphan"), 0o640))
	s.ageStoredFile(name, hoursAgo)
	return name
}

// ageStoredFile moves the modified time of a file in the upload dir back by hoursAgo. The file goes when the test
// ends, so a later sweep test does not see it as an old orphan.
func (s *FileServiceTestSuite) ageStoredFile(name string, hoursAgo int) {
	path := filepath.Join(s.TC.UploadDir, name)
	past := time.Now().Add(-time.Duration(hoursAgo) * time.Hour)
	s.Require().NoError(os.Chtimes(path, past, past))
	s.T().Cleanup(func() { _ = os.Remove(path) })
}

func (s *FileServiceTestSuite) storageKey(id int64) string {
	var key string
	err := s.TC.DB.QueryRowContext(context.Background(), `SELECT storage_key FROM files WHERE id = $1`, id).Scan(&key)
	s.Require().NoError(err)
	return key
}

func (s *FileServiceTestSuite) storedFileExists(key string) bool {
	_, err := os.Stat(filepath.Join(s.TC.UploadDir, key))
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	s.Require().NoError(err)
	return true
}

func (s *FileServiceTestSuite) thumbDir() string {
	return filepath.Join(s.TC.UploadDir, utils.ThumbDir)
}

// thumbnail opens the thumbnail of id and decodes it. The thumbnail dir goes when the test ends, so the next test
// starts with none.
func (s *FileServiceTestSuite) thumbnail(userID, id int64) (image.Image, *file.FileContent) {
	s.T().Cleanup(func() { _ = os.RemoveAll(s.thumbDir()) })
	content, err := s.TC.Services.FileService.OpenThumbnail(context.Background(), userID, id)
	s.Require().NoError(err)
	defer func() { _ = content.Content.Close() }()
	img, _, err := image.Decode(content.Content)
	s.Require().NoError(err)
	return img, content
}

func (s *FileServiceTestSuite) encodePNG(img image.Image) string {
	var buf bytes.Buffer
	s.Require().NoError(png.Encode(&buf, img))
	return buf.String()
}

// encodeJPEG encodes img as a JPEG with an EXIF segment that holds only the orientation, the way a camera writes it.
func (s *FileServiceTestSuite) encodeJPEG(img image.Image, orientation uint16) string {
	var buf bytes.Buffer
	s.Require().NoError(jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}))
	// A big-endian TIFF header, then a directory with one entry: the orientation tag (0x0112) as a SHORT (3).
	tiff := []byte{'M', 'M', 0, 42, 0, 0, 0, 8, 0, 1, 0x01, 0x12, 0, 3, 0, 0, 0, 1}
	tiff = binary.BigEndian.AppendUint16(tiff, orientation)
	tiff = append(tiff, 0, 0, 0, 0, 0, 0)
	exif := append([]byte("Exif\x00\x00"), tiff...)
	segment := binary.BigEndian.AppendUint16([]byte{0xFF, 0xE1}, uint16(len(exif)+2))
	b := buf.Bytes()
	return string(b[:2]) + string(segment) + string(exif) + string(b[2:])
}

// hugePNG is a 1 by 1 PNG whose header says 10000 by 10000, so it is over the pixel cap before it decodes.
func (s *FileServiceTestSuite) hugePNG() string {
	b := []byte(s.encodePNG(testPicture(1, 1, 255)))
	// The IHDR chunk starts at byte 8: the length, the type, the width, the height, 5 more bytes, then the CRC of the
	// type and the data.
	binary.BigEndian.PutUint32(b[16:], 10000)
	binary.BigEndian.PutUint32(b[20:], 10000)
	binary.BigEndian.PutUint32(b[29:], crc32.ChecksumIEEE(b[12:29]))
	return string(b)
}

// testPicture is w by h with the given alpha: the left half red with a green top left quarter, and the right half blue.
func testPicture(w, h int, alpha uint8) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := color.NRGBA{B: 255, A: alpha}
			switch {
			case x < w/2 && y < h/2:
				c = color.NRGBA{G: 255, A: alpha}
			case x < w/2:
				c = color.NRGBA{R: 255, A: alpha}
			}
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

// colorName names the pure colors of testPicture, so a failed check shows which one it got.
func colorName(c color.Color) string {
	r, g, b, _ := c.RGBA()
	switch {
	case r > 0xC000 && g < 0x4000 && b < 0x4000:
		return "red"
	case g > 0xC000 && r < 0x4000 && b < 0x4000:
		return "green"
	case b > 0xC000 && r < 0x4000 && g < 0x4000:
		return "blue"
	}
	return fmt.Sprintf("other (%x, %x, %x)", r, g, b)
}

// markInGroup marks a row the way a batch of the trash job does. It stands for a job that stopped part way.
func (s *FileServiceTestSuite) markInGroup(rootID, id int64) {
	_, err := s.TC.DB.ExecContext(context.Background(), `
		UPDATE files SET (trashed_at, trash_root_id) = (SELECT trashed_at, id FROM files WHERE id = $1)
		WHERE id = $2`, rootID, id)
	s.Require().NoError(err)
}

func (s *FileServiceTestSuite) rowsInGroup(rootID int64) int {
	var n int
	err := s.TC.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM files WHERE trash_root_id = $1`, rootID).Scan(&n)
	s.Require().NoError(err)
	return n
}

// copiesOf reads the ids of the copies of a row, oldest first.
func (s *FileServiceTestSuite) copiesOf(sourceID int64) []int64 {
	rows, err := s.TC.DB.QueryContext(context.Background(),
		`SELECT id FROM files WHERE copied_from_id = $1 ORDER BY id`, sourceID)
	s.Require().NoError(err)
	defer func() { _ = rows.Close() }()

	var ids []int64
	for rows.Next() {
		var id int64
		s.Require().NoError(rows.Scan(&id))
		ids = append(ids, id)
	}
	s.Require().NoError(rows.Err())
	return ids
}

// copyKeys reads the storage keys of the user's copied files.
func (s *FileServiceTestSuite) copyKeys(userID int64) []string {
	rows, err := s.TC.DB.QueryContext(context.Background(),
		`SELECT storage_key FROM files WHERE user_id = $1 AND copied_from_id IS NOT NULL AND storage_key IS NOT NULL`, userID)
	s.Require().NoError(err)
	defer func() { _ = rows.Close() }()

	var keys []string
	for rows.Next() {
		var key string
		s.Require().NoError(rows.Scan(&key))
		keys = append(keys, key)
	}
	s.Require().NoError(rows.Err())
	return keys
}

// addCopy adds a copy of a row the way a batch of the copy job does. It stands for a job that stopped part way.
func (s *FileServiceTestSuite) addCopy(parentID, sourceID int64) int64 {
	var id int64
	var key sql.NullString
	err := s.TC.DB.QueryRowContext(context.Background(), `
		INSERT INTO files (user_id, parent_id, name, storage_key, content_type, size_bytes, copied_from_id)
		SELECT user_id, $1, name, CASE WHEN storage_key IS NOT NULL THEN md5(random()::text) END, content_type, size_bytes, id
		FROM files WHERE id = $2
		RETURNING id, storage_key`, parentID, sourceID).Scan(&id, &key)
	s.Require().NoError(err)
	if key.Valid {
		s.Require().NoError(os.WriteFile(filepath.Join(s.TC.UploadDir, key.String), []byte(s.storedContent(sourceID)), 0o640))
	}
	return id
}

// writeSeededBytes writes each of the user's seeded files to disk, with its name as its content.
func (s *FileServiceTestSuite) writeSeededBytes(userID int64) {
	rows, err := s.TC.DB.QueryContext(context.Background(),
		`SELECT storage_key, name FROM files WHERE user_id = $1 AND storage_key IS NOT NULL`, userID)
	s.Require().NoError(err)
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var key, name string
		s.Require().NoError(rows.Scan(&key, &name))
		s.Require().NoError(os.WriteFile(filepath.Join(s.TC.UploadDir, key), []byte(name), 0o640))
	}
	s.Require().NoError(rows.Err())
}

// treeOf lists the live rows under a folder by path, in byte order. A folder ends in "/", and a file shows its content.
func (s *FileServiceTestSuite) treeOf(folderID int64) []string {
	rows, err := s.TC.DB.QueryContext(context.Background(), `
		WITH RECURSIVE tree AS (
			SELECT id, name::text AS path, storage_key FROM files WHERE parent_id = $1 AND trashed_at IS NULL
			UNION ALL
			SELECT f.id, tree.path || '/' || f.name, f.storage_key
			FROM files f JOIN tree ON f.parent_id = tree.id
			WHERE f.trashed_at IS NULL
		)
		SELECT path, storage_key FROM tree`, folderID)
	s.Require().NoError(err)
	defer func() { _ = rows.Close() }()

	var paths []string
	for rows.Next() {
		var path string
		var key sql.NullString
		s.Require().NoError(rows.Scan(&path, &key))
		if !key.Valid {
			paths = append(paths, path+"/")
			continue
		}
		content, err := os.ReadFile(filepath.Join(s.TC.UploadDir, key.String))
		s.Require().NoError(err, path)
		paths = append(paths, path+"="+string(content))
	}
	s.Require().NoError(rows.Err())
	slices.Sort(paths)
	return paths
}

func (s *FileServiceTestSuite) storedContent(id int64) string {
	content, err := os.ReadFile(filepath.Join(s.TC.UploadDir, s.storageKey(id)))
	s.Require().NoError(err)
	return string(content)
}

func (s *FileServiceTestSuite) storedModTime(id int64) time.Time {
	info, err := os.Stat(filepath.Join(s.TC.UploadDir, s.storageKey(id)))
	s.Require().NoError(err)
	return info.ModTime()
}

// parentOf reads parent_id. The root is 0.
func (s *FileServiceTestSuite) parentOf(id int64) int64 {
	var parent sql.NullInt64
	err := s.TC.DB.QueryRowContext(context.Background(), `SELECT parent_id FROM files WHERE id = $1`, id).Scan(&parent)
	s.Require().NoError(err)
	return parent.Int64
}

func (s *FileServiceTestSuite) changedSince(id int64, since time.Time) bool {
	var changed bool
	err := s.TC.DB.QueryRowContext(context.Background(),
		`SELECT updated_at > $1 FROM files WHERE id = $2`, since, id).Scan(&changed)
	s.Require().NoError(err)
	return changed
}

// namesByID reads the items in Home.
func (s *FileServiceTestSuite) namesByID(userID int64) map[int64]string {
	result, err := s.TC.Services.FileService.List(context.Background(), userID, newestFirst(0))
	s.Require().NoError(err)
	names := make(map[int64]string, len(result.Data))
	for _, f := range result.Data {
		names[f.ID] = f.Name
	}
	return names
}
