//go:build integration

package file_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"gost/internal/apperr"
	"gost/internal/database"
	"gost/internal/domains/file"
	"gost/internal/jobs"
	"gost/tests/integration"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/riverqueue/river"
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

// A delete of a folder and creates into it run at once. A create may fail, but it must never leave a live row inside the trash.
func (s *FileServiceTestSuite) TestUploadAndCreateFolder_ParallelWithTrashOfParent() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService

	const n = 20
	folders := make([]int64, n)
	for i := range n {
		folders[i] = s.createFolder(userID, 0, fmt.Sprintf("Docs %d", i)).ID
	}

	trashErrs := make([]error, n)
	createErrs := make([]error, 2*n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { trashErrs[i] = svc.Trash(context.Background(), userID, []int64{folders[i]}) })
		wg.Go(func() {
			_, createErrs[2*i] = svc.Upload(context.Background(), userID, folders[i], "new.txt", strings.NewReader("n"))
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
	_, err := svc.Upload(context.Background(), userID, sub.ID, "x.txt", strings.NewReader("x"))
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
	_, err := svc.Upload(context.Background(), userID, archive.ID, "a.txt", strings.NewReader("a"))
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
	inDocs, err := svc.Upload(context.Background(), userID, docs.ID, "a.txt", strings.NewReader("a"))
	s.Require().NoError(err)
	inNotes, err := svc.Upload(context.Background(), userID, notes.ID, "a.txt", strings.NewReader("a"))
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
	inDocs, err := svc.Upload(context.Background(), userID, docs.ID, "a.txt", strings.NewReader("a"))
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
			_, err := svc.Upload(context.Background(), userID, docsCopy, "new.txt", strings.NewReader("n"))
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
	inCars, err := svc.Upload(context.Background(), userID, cars.ID, "x.txt", strings.NewReader("x"))
	s.Require().NoError(err)
	inPhotos, err := svc.Upload(context.Background(), userID, photos.ID, "y.txt", strings.NewReader("y"))
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

// An item deleted on its own keeps its group when its folder follows, and so do the rows under it.
// An item listed next to its folder keeps its group too.
func (s *FileServiceTestSuite) TestTrash_EachItemKeepsItsGroup() {
	userID := s.seedUser()
	svc := s.TC.Services.FileService
	docs := s.createFolder(userID, 0, "Docs")
	old, err := svc.Upload(context.Background(), userID, docs.ID, "old.txt", strings.NewReader("o"))
	s.Require().NoError(err)
	oldDir := s.createFolder(userID, docs.ID, "Old")
	inOldDir, err := svc.Upload(context.Background(), userID, oldDir.ID, "in-old.txt", strings.NewReader("i"))
	s.Require().NoError(err)
	listed, err := svc.Upload(context.Background(), userID, docs.ID, "listed.txt", strings.NewReader("l"))
	s.Require().NoError(err)
	rest, err := svc.Upload(context.Background(), userID, docs.ID, "rest.txt", strings.NewReader("r"))
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
	inDocs, err := svc.Upload(context.Background(), userID, docs.ID, "x.txt", strings.NewReader("x"))
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
	inDeep, err := svc.Upload(context.Background(), userID, deep.ID, "deep.txt", strings.NewReader("d"))
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
	_, err := svc.Upload(context.Background(), userID, sub.ID, "x.txt", strings.NewReader("x"))
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
	x, err := svc.Upload(context.Background(), userID, sub.ID, "x.txt", strings.NewReader("x"))
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
	_, err := svc.Upload(context.Background(), alice, photos.ID, "in-photos.txt", strings.NewReader("p"))
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
	old, err := svc.Upload(context.Background(), userID, docs.ID, "old.txt", strings.NewReader("o"))
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
	old, err := svc.Upload(context.Background(), userID, sub.ID, "old.txt", strings.NewReader("o"))
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
	under, err := svc.Upload(context.Background(), alice, docs.ID, "under.txt", strings.NewReader("u"))
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
		item, err := svc.Upload(context.Background(), userID, folder.ID, "old.txt", strings.NewReader("o"))
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
	a, err := svc.Upload(context.Background(), userID, docs.ID, "a.txt", strings.NewReader("a"))
	s.Require().NoError(err)
	b, err := svc.Upload(context.Background(), userID, sub.ID, "b.txt", strings.NewReader("b"))
	s.Require().NoError(err)
	gone, err := svc.Upload(context.Background(), userID, 0, "gone.txt", strings.NewReader("g"))
	s.Require().NoError(err)
	keep, err := svc.Upload(context.Background(), userID, 0, "keep.txt", strings.NewReader("k"))
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

// One bad id undoes the rest. A live item, a row under a deleted folder and an item already marked are bad ids too.
func (s *FileServiceTestSuite) TestPurge_AllOrNone() {
	alice := s.seedUserWithEmail("alice@example.com")
	bob := s.seedUserWithEmail("bob@example.com")
	svc := s.TC.Services.FileService
	a := s.seedFile(alice, "a.txt")
	live := s.seedFile(alice, "live.txt")
	marked := s.seedFile(alice, "marked.txt")
	docs := s.createFolder(alice, 0, "Docs")
	under, err := svc.Upload(context.Background(), alice, docs.ID, "under.txt", strings.NewReader("u"))
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
	old, err := svc.Upload(context.Background(), userID, docs.ID, "old.txt", strings.NewReader("o"))
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
	x, err := svc.Upload(context.Background(), userID, sub.ID, "x.txt", strings.NewReader("x"))
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
	inDeep, err := svc.Upload(context.Background(), userID, deep.ID, "deep.txt", strings.NewReader("d"))
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
	x, err := svc.Upload(context.Background(), userID, docs.ID, "x.txt", strings.NewReader("x"))
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
	a, err := svc.Upload(context.Background(), userID, 0, "a.txt", strings.NewReader("a"))
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
	a, err := svc.Upload(context.Background(), alice, 0, "a.txt", strings.NewReader("a"))
	s.Require().NoError(err)
	b, err := svc.Upload(context.Background(), alice, 0, "b.txt", strings.NewReader("b"))
	s.Require().NoError(err)
	docs := s.createFolder(alice, 0, "Docs")
	in, err := svc.Upload(context.Background(), alice, docs.ID, "in.txt", strings.NewReader("i"))
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
	live, err := svc.Upload(context.Background(), userID, 0, "live.txt", strings.NewReader("l"))
	s.Require().NoError(err)
	trashed, err := svc.Upload(context.Background(), userID, 0, "trashed.txt", strings.NewReader("t"))
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
	live, err := svc.Upload(context.Background(), userID, 0, "live.txt", strings.NewReader("l"))
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
func workJobs[T river.JobArgs](s *FileServiceTestSuite, worker river.Worker[T]) {
	var kind T
	for _, args := range queuedJobs[T](s) {
		s.Require().NoError(worker.Work(context.Background(), &river.Job[T]{Args: args}))
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

func (s *FileServiceTestSuite) upload(userID, parentID int64, name, content string) *file.File {
	f, err := s.TC.Services.FileService.Upload(context.Background(), userID, parentID, name, strings.NewReader(content))
	s.Require().NoError(err)
	return f
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
