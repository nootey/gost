//go:build integration

package file_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"gost/internal/apperr"
	"gost/internal/database"
	"gost/internal/domains/file"
	"gost/tests/integration"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
)

// oldUpdatedAt is a fixed past time. A test sets updated_at to it, then checks which rows an action changed.
var oldUpdatedAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

type FileServiceTestSuite struct {
	integration.ServiceIntegrationSuite
}

func TestIntegrationFileServiceTestSuite(t *testing.T) {
	suite.Run(t, new(FileServiceTestSuite))
}

func (s *FileServiceTestSuite) TestUpload_StoresFileAndRecord() {
	userID := s.seedUser()

	f, err := s.TC.Services.FileService.Upload(context.Background(), userID, 0, "notes.txt", strings.NewReader("hello world"))

	s.Require().NoError(err)
	s.Equal("notes.txt", f.Name)
	s.False(f.IsFolder)
	s.Nil(f.ParentID)
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

	f, err := s.TC.Services.FileService.Upload(context.Background(), userID, 0, "exact.bin", bytes.NewReader(make([]byte, 1<<20)))

	s.Require().NoError(err)
	s.Equal(int64(1<<20), f.SizeBytes)
}

func (s *FileServiceTestSuite) TestUpload_TooLargeLeavesNothingBehind() {
	userID := s.seedUser()
	before := s.dirEntries()

	_, err := s.TC.Services.FileService.Upload(context.Background(), userID, 0, "big.bin", bytes.NewReader(make([]byte, 1<<20+1)))

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
		{"   ", "x", "file name is required"},
		{"bad\xffname.txt", "x", "file name has invalid characters"},
		{"nul\x00.txt", "x", "file name has invalid characters"},
		{"a/b.txt", "x", "file name has invalid characters"},
		{"..", "x", "file name has invalid characters"},
		{strings.Repeat("a", 256), "x", "file name is too long"},
		{"empty.txt", "", "file is empty"},
	}
	for _, tc := range cases {
		_, err := svc.Upload(context.Background(), userID, 0, tc.name, strings.NewReader(tc.content))

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

	for _, sort := range []file.SortField{file.SortByName, file.SortBySize, file.SortByType, file.SortByDate} {
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
	uploaded, err := svc.Upload(context.Background(), userID, folder.ID, "notes.txt", strings.NewReader("hello"))
	s.Require().NoError(err)

	inFolder, err := svc.List(context.Background(), userID, newestFirst(folder.ID))
	s.Require().NoError(err)
	atRoot, err := svc.List(context.Background(), userID, newestFirst(0))
	s.Require().NoError(err)

	s.Equal([]*file.File{uploaded}, inFolder.Data)
	s.Equal([]*file.File{folder}, atRoot.Data)
}

func (s *FileServiceTestSuite) TestUpload_IntoFolder() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	folder := s.createFolder(userID, 0, "Docs")

	f, err := svc.Upload(context.Background(), userID, folder.ID, "notes.txt", strings.NewReader("hello"))

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

func (s *FileServiceTestSuite) TestUpload_UnknownFolderLeavesNothingBehind() {
	userID := s.seedUser()
	before := s.dirEntries()

	_, err := s.TC.Services.FileService.Upload(context.Background(), userID, 999, "a.txt", strings.NewReader("x"))

	status, msg := apperr.Resolve(err)
	s.Equal(http.StatusNotFound, status)
	s.Equal("folder not found", msg)
	s.Equal(before, s.dirEntries())
	s.Equal(0, s.fileRows())
}

func (s *FileServiceTestSuite) TestUpload_RenamesOnClash() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService

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
		f, err := svc.Upload(context.Background(), userID, 0, tc.name, strings.NewReader("x"))

		s.Require().NoError(err)
		s.Equal(tc.want, f.Name)
	}
}

// The lowest free number wins, so a gap left by a missing copy is filled first.
func (s *FileServiceTestSuite) TestUpload_RenameFillsGaps() {
	userID := s.seedUser()
	s.seedFile(userID, "b.txt")
	s.seedFile(userID, "b (2).txt")

	f, err := s.TC.Services.FileService.Upload(context.Background(), userID, 0, "b.txt", strings.NewReader("x"))

	s.Require().NoError(err)
	s.Equal("b (1).txt", f.Name)
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
	svc := s.TC.Services.FileService
	s.seedFile(userID, "a.txt")

	s.Equal("Photos", s.createFolder(userID, 0, "Photos").Name)
	s.Equal("Photos (1)", s.createFolder(userID, 0, "Photos").Name)
	s.Equal("a.txt (1)", s.createFolder(userID, 0, "a.txt").Name)

	f, err := svc.Upload(context.Background(), userID, 0, "Photos", strings.NewReader("x"))
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
	_, err := svc.Upload(context.Background(), userID, folder.ID, "report-2.txt", strings.NewReader("x"))
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

// Every row under the folder goes to the trash with it, so no path, list, upload or rename can reach them.
func (s *FileServiceTestSuite) TestTrash_HidesFolderAndEverythingUnder() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	photos := s.createFolder(userID, 0, "Photos")
	cars := s.createFolder(userID, photos.ID, "Cars")
	inCars, err := svc.Upload(context.Background(), userID, cars.ID, "x.txt", strings.NewReader("x"))
	s.Require().NoError(err)
	inPhotos, err := svc.Upload(context.Background(), userID, photos.ID, "y.txt", strings.NewReader("y"))
	s.Require().NoError(err)
	s.seedFile(userID, "a.txt")

	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{photos.ID}))

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

		_, err = svc.Upload(context.Background(), userID, folderID, "new.txt", strings.NewReader("n"))
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
		f, err := svc.Upload(context.Background(), userID, 0, "a.txt", strings.NewReader("new"))
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

// An item deleted on its own keeps its group when its folder follows, and so does an item listed next to its folder.
func (s *FileServiceTestSuite) TestTrash_EachItemKeepsItsGroup() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	old, err := svc.Upload(context.Background(), userID, docs.ID, "old.txt", strings.NewReader("o"))
	s.Require().NoError(err)
	listed, err := svc.Upload(context.Background(), userID, docs.ID, "listed.txt", strings.NewReader("l"))
	s.Require().NoError(err)
	rest, err := svc.Upload(context.Background(), userID, docs.ID, "rest.txt", strings.NewReader("r"))
	s.Require().NoError(err)

	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{old.ID}))
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{listed.ID, docs.ID}))

	s.Equal(old.ID, s.trashRoot(old.ID))
	s.Equal(listed.ID, s.trashRoot(listed.ID))
	s.Equal(docs.ID, s.trashRoot(docs.ID))
	s.Equal(docs.ID, s.trashRoot(rest.ID))
}

// The trash lists the items the user deleted, folders first, newest delete first. The rows under them and other users' items stay out.
func (s *FileServiceTestSuite) TestListTrash_OnlyDeletedItems() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	photos := s.createFolder(alice, 0, "Photos")
	_, err := svc.Upload(context.Background(), alice, photos.ID, "in-photos.txt", strings.NewReader("p"))
	s.Require().NoError(err)
	a := s.seedFile(alice, "a.txt")
	b := s.seedFile(alice, "b.txt")
	s.seedFile(alice, "live.txt")
	bobFile := s.seedFile(bob, "bob.txt")
	s.Require().NoError(svc.Trash(context.Background(), bob, []int64{bobFile}))
	s.Require().NoError(svc.Trash(context.Background(), alice, []int64{b, photos.ID}))
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
	_, err := svc.Upload(context.Background(), userID, cars.ID, "x.txt", strings.NewReader("x"))
	s.Require().NoError(err)
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{photos.ID}))
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
	_, err := svc.Upload(context.Background(), userID, 0, "a.txt", strings.NewReader("new"))
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
	second, err := svc.Upload(context.Background(), userID, 0, "a.txt", strings.NewReader("new"))
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
	old, err := svc.Upload(context.Background(), userID, docs.ID, "old.txt", strings.NewReader("o"))
	s.Require().NoError(err)
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{old.ID}))
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{docs.ID}))

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
	old, err := svc.Upload(context.Background(), userID, docs.ID, "old.txt", strings.NewReader("o"))
	s.Require().NoError(err)
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{old.ID}))
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{docs.ID}))

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
	old, err := svc.Upload(context.Background(), userID, sub.ID, "old.txt", strings.NewReader("o"))
	s.Require().NoError(err)
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{old.ID}))
	s.Require().NoError(svc.Trash(context.Background(), userID, []int64{docs.ID}))

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
	under, err := svc.Upload(context.Background(), alice, docs.ID, "under.txt", strings.NewReader("u"))
	s.Require().NoError(err)
	s.Require().NoError(svc.Trash(context.Background(), alice, []int64{a, docs.ID}))
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

func trashNames(items []*file.TrashedFile) []string {
	names := make([]string, len(items))
	for i, item := range items {
		names[i] = item.Name
	}
	return names
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
