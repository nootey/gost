package file_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"gost/config"
	"gost/internal/apperr"
	"gost/internal/domains/file"
	"gost/internal/middleware"
	"gost/internal/uploads"
	"gost/internal/utils"
	"gost/mocks"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
)

const testUserID int64 = 7

var atNameLimit = strings.Repeat("a", utils.MaxNameLength)

type FileHandlerTestSuite struct {
	suite.Suite
	router      *chi.Mux
	mockService *mocks.MockFileServiceInterface
	handler     *file.FileHandler
	uploadDir   string
}

func (suite *FileHandlerTestSuite) SetupTest() {
	suite.mockService = mocks.NewMockFileServiceInterface(suite.T())
	suite.uploadDir = suite.T().TempDir()
	resp := apperr.NewResponder(zap.NewNop())
	suite.handler = file.NewFileHandler(suite.mockService, config.DownloadConfig{TimeoutSeconds: 60}, resp)
	uploadHandler, err := uploads.NewHandler(suite.uploadDir, "/uploads/", suite.handler.UploadHooks(), resp, zap.NewNop())
	suite.Require().NoError(err)

	suite.router = chi.NewRouter()
	suite.router.Mount("/uploads", uploadHandler)
	suite.router.Get("/files", suite.handler.List)
	suite.router.Post("/folders", suite.handler.CreateFolder)
	suite.router.Post("/folders/tree", suite.handler.CreateFolderTree)
	suite.router.Get("/favorites", suite.handler.ListFavorites)
	suite.router.Post("/favorites", suite.handler.AddFavorites)
	suite.router.Post("/favorites/remove", suite.handler.RemoveFavorites)
	suite.router.Get("/folders/resolve", suite.handler.ResolveFolder)
	suite.router.Get("/files/{id}", suite.handler.GetItem)
	suite.router.Get("/files/{id}/activity", suite.handler.ListActivity)
	suite.router.Patch("/files/{id}", suite.handler.Rename)
	suite.router.Get("/files/{id}/download", suite.handler.Download)
	suite.router.Post("/files/move", suite.handler.Move)
	suite.router.Post("/files/copy", suite.handler.Copy)
	suite.router.Post("/files/hide", suite.handler.Hide)
	suite.router.Post("/files/unhide", suite.handler.Unhide)
	suite.router.Get("/trash", suite.handler.ListTrash)
	suite.router.Post("/trash", suite.handler.Trash)
	suite.router.Post("/trash/restore", suite.handler.Restore)
	suite.router.Post("/trash/purge", suite.handler.Purge)
	suite.router.Post("/downloads", suite.handler.RequestZip)
	suite.router.Get("/downloads", suite.handler.ListDownloads)
	suite.router.Delete("/downloads/{id}", suite.handler.RemoveDownload)
	suite.router.Get("/downloads/{id}/file", suite.handler.DownloadZip)
}

func TestFileHandlerTestSuite(t *testing.T) {
	suite.Run(t, new(FileHandlerTestSuite))
}

func (suite *FileHandlerTestSuite) errorMessage(w *httptest.ResponseRecorder) string {
	var resp apperr.ErrorResponse
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	return resp.Message
}

// tusMeta builds an Upload-Metadata header from key and value pairs.
func tusMeta(pairs ...string) string {
	parts := make([]string, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		parts = append(parts, pairs[i]+" "+base64.StdEncoding.EncodeToString([]byte(pairs[i+1])))
	}
	return strings.Join(parts, ",")
}

// tus sends one tus request as userID. A userID of 0 sends it without a user.
func (suite *FileHandlerTestSuite) tus(method, path string, userID int64, header map[string]string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Tus-Resumable", "1.0.0")
	if body != "" {
		req.Header.Set("Content-Type", "application/offset+octet-stream")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	if userID != 0 {
		req = req.WithContext(middleware.WithUserID(req.Context(), userID))
	}
	w := httptest.NewRecorder()
	suite.router.ServeHTTP(w, req)
	return w
}

// createUpload starts an upload of notes.txt in folder 5 and returns its URL.
func (suite *FileHandlerTestSuite) createUpload(size int64, extraMeta ...string) string {
	suite.mockService.EXPECT().CheckUpload(mock.Anything, testUserID, int64(5), "notes.txt", size).Return(nil).Once()

	meta := tusMeta(append([]string{"filename", "notes.txt", "parent_id", "5"}, extraMeta...)...)
	w := suite.tus(http.MethodPost, "/uploads/", testUserID, map[string]string{
		"Upload-Length":   strconv.FormatInt(size, 10),
		"Upload-Metadata": meta,
	}, "")
	suite.Require().Equal(http.StatusCreated, w.Code, w.Body.String())
	return w.Header().Get("Location")
}

// partialFiles lists what the uploads left in the partial dir.
func (suite *FileHandlerTestSuite) partialFiles() []string {
	entries, err := os.ReadDir(filepath.Join(suite.uploadDir, uploads.PartialDir))
	suite.Require().NoError(err)
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}

func (suite *FileHandlerTestSuite) TestChunkedUpload_Success() {
	loc := suite.createUpload(11)
	suite.Regexp(`^/uploads/[0-9a-f]{32}$`, loc, "the Location is a path, so the chunks go through the same proxy")

	w := suite.tus(http.MethodPatch, loc, testUserID, map[string]string{"Upload-Offset": "0"}, "hello")
	suite.Require().Equal(http.StatusNoContent, w.Code, w.Body.String())
	suite.Equal("5", w.Header().Get("Upload-Offset"))

	w = suite.tus(http.MethodHead, loc, testUserID, nil, "")
	suite.Equal(http.StatusOK, w.Code)
	suite.Equal("5", w.Header().Get("Upload-Offset"))

	suite.mockService.EXPECT().
		FinishUpload(mock.Anything, testUserID, int64(5), "notes.txt", mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ int64, _, path string) (*file.File, error) {
			suite.Equal(filepath.Join(suite.uploadDir, uploads.PartialDir), filepath.Dir(path))
			content, err := os.ReadFile(path)
			suite.Require().NoError(err)
			suite.Equal("hello world", string(content))
			// The service moves the bytes into the store.
			suite.Require().NoError(os.Remove(path))
			return &file.File{ID: 1, Name: "notes.txt", SizeBytes: 11}, nil
		}).
		Once()

	w = suite.tus(http.MethodPatch, loc, testUserID, map[string]string{"Upload-Offset": "5"}, " world")

	suite.Require().Equal(http.StatusOK, w.Code, w.Body.String())
	var resp file.UploadResponse
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	suite.Equal("notes.txt uploaded.", resp.Message)
	suite.Equal(int64(1), resp.File.ID)
	suite.Empty(suite.partialFiles())
	suite.Equal(http.StatusNotFound, suite.tus(http.MethodHead, loc, testUserID, nil, "").Code)
}

func (suite *FileHandlerTestSuite) TestChunkedUpload_AtRoot() {
	suite.mockService.EXPECT().CheckUpload(mock.Anything, testUserID, int64(0), "notes.txt", int64(1)).Return(nil).Once()
	suite.mockService.EXPECT().
		FinishUpload(mock.Anything, testUserID, int64(0), "notes.txt", mock.Anything).
		Return(&file.File{ID: 1, Name: "notes.txt"}, nil).
		Once()

	w := suite.tus(http.MethodPost, "/uploads/", testUserID, map[string]string{
		"Upload-Length":   "1",
		"Upload-Metadata": tusMeta("filename", "notes.txt"),
	}, "")
	suite.Require().Equal(http.StatusCreated, w.Code, w.Body.String())
	w = suite.tus(http.MethodPatch, w.Header().Get("Location"), testUserID, map[string]string{"Upload-Offset": "0"}, "x")

	suite.Equal(http.StatusOK, w.Code, w.Body.String())
}

// Another user's upload answers 404, as if it did not exist.
func (suite *FileHandlerTestSuite) TestChunkedUpload_OnlyOwner() {
	// The client cannot set the owner through the metadata.
	loc := suite.createUpload(5, "user_id", "8")

	for _, method := range []string{http.MethodHead, http.MethodPatch, http.MethodDelete} {
		w := suite.tus(method, loc, 8, map[string]string{"Upload-Offset": "0"}, "x")
		suite.Equal(http.StatusNotFound, w.Code, method)
	}
	suite.Equal(http.StatusOK, suite.tus(http.MethodHead, loc, testUserID, nil, "").Code)
}

func (suite *FileHandlerTestSuite) TestChunkedUpload_UnknownIDs() {
	for _, path := range []string{"/uploads/abc", "/uploads/../notes.txt", "/uploads/0123456789abcdef0123456789abcdef"} {
		w := suite.tus(http.MethodHead, path, testUserID, nil, "")
		suite.Equal(http.StatusNotFound, w.Code, path)
	}
}

func (suite *FileHandlerTestSuite) TestChunkedUpload_Unauthenticated() {
	w := suite.tus(http.MethodPost, "/uploads/", 0, map[string]string{"Upload-Length": "1"}, "")

	suite.Equal(http.StatusUnauthorized, w.Code)
}

func (suite *FileHandlerTestSuite) TestChunkedUpload_CreateRejects() {
	suite.mockService.EXPECT().
		CheckUpload(mock.Anything, testUserID, int64(0), "notes.txt", int64(1)).
		Return(apperr.New(apperr.KindNotFound, "folder not found")).
		Once()

	cases := []struct {
		header  map[string]string
		status  int
		message string
	}{
		{map[string]string{"Upload-Length": "1", "Upload-Metadata": tusMeta("filename", "notes.txt")}, http.StatusNotFound, "folder not found"},
		{map[string]string{"Upload-Length": "1", "Upload-Metadata": tusMeta("parent_id", "abc")}, http.StatusBadRequest, "invalid parent_id"},
		{map[string]string{"Upload-Defer-Length": "1"}, http.StatusBadRequest, "the upload length is required"},
	}
	for _, tc := range cases {
		w := suite.tus(http.MethodPost, "/uploads/", testUserID, tc.header, "")

		suite.Equal(tc.status, w.Code, tc.message)
		suite.Equal(tc.message, suite.errorMessage(w))
	}
	suite.Empty(suite.partialFiles())
}

// A failed finish removes the partial upload: the server has all its bytes, so the client cannot finish it again.
func (suite *FileHandlerTestSuite) TestChunkedUpload_FinishFails() {
	cases := []struct {
		err     error
		status  int
		message string
	}{
		{apperr.New(apperr.KindNotFound, "folder not found"), http.StatusNotFound, "folder not found"},
		{assert.AnError, http.StatusInternalServerError, "something went wrong"},
	}
	for _, tc := range cases {
		loc := suite.createUpload(1)
		suite.mockService.EXPECT().
			FinishUpload(mock.Anything, testUserID, int64(5), "notes.txt", mock.Anything).
			Return(nil, tc.err).
			Once()

		w := suite.tus(http.MethodPatch, loc, testUserID, map[string]string{"Upload-Offset": "0"}, "x")

		suite.Equal(tc.status, w.Code, tc.message)
		suite.Equal(tc.message, suite.errorMessage(w))
		suite.Empty(suite.partialFiles())
	}
}

func (suite *FileHandlerTestSuite) TestChunkedUpload_Cancel() {
	loc := suite.createUpload(5)

	w := suite.tus(http.MethodDelete, loc, testUserID, nil, "")

	suite.Equal(http.StatusNoContent, w.Code)
	suite.Empty(suite.partialFiles())
}

func (suite *FileHandlerTestSuite) list(query string, authenticated bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/files"+query, nil)
	if authenticated {
		req = req.WithContext(middleware.WithUserID(req.Context(), testUserID))
	}
	w := httptest.NewRecorder()
	suite.router.ServeHTTP(w, req)
	return w
}

func (suite *FileHandlerTestSuite) TestList_Success() {
	next := "next-page"
	suite.mockService.EXPECT().
		List(mock.Anything, testUserID, file.ListQuery{Cursor: "this-page", PageSize: 5, Sort: file.SortByDate, Desc: true}).
		Return(&utils.Page[*file.File]{
			Data:       []*file.File{{ID: 3, Name: "c.txt"}, {ID: 2, Name: "b.txt"}},
			NextCursor: &next,
		}, nil).
		Once()

	w := suite.list("?cursor=this-page&page_size=5", true)

	suite.Equal(http.StatusOK, w.Code)
	var resp utils.Page[*file.File]
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	suite.Require().Len(resp.Data, 2)
	suite.Equal("c.txt", resp.Data[0].Name)
	suite.Require().NotNil(resp.NextCursor)
	suite.Equal(next, *resp.NextCursor)
}

// A missing or bad page size reaches the service as zero, so the service picks the default. The sort is newest first.
// The old page param is ignored. The last page has a null next_cursor, so the client knows to stop.
func (suite *FileHandlerTestSuite) TestList_DefaultPagination() {
	suite.mockService.EXPECT().
		List(mock.Anything, testUserID, file.ListQuery{Sort: file.SortByDate, Desc: true}).
		Return(&utils.Page[*file.File]{Data: []*file.File{}}, nil).
		Twice()

	suite.Equal(http.StatusOK, suite.list("", true).Code)
	w := suite.list("?page=2&page_size=-x", true)

	suite.Equal(http.StatusOK, w.Code)
	suite.JSONEq(`{"data":[],"next_cursor":null}`, w.Body.String())
}

func (suite *FileHandlerTestSuite) TestList_InFolder() {
	suite.mockService.EXPECT().
		List(mock.Anything, testUserID, file.ListQuery{ParentID: 12, PageSize: 10, Sort: file.SortByDate, Desc: true}).
		Return(&utils.Page[*file.File]{Data: []*file.File{}}, nil).
		Once()

	w := suite.list("?parent_id=12&page_size=10", true)

	suite.Equal(http.StatusOK, w.Code)
}

// Unlike page values, a bad folder id is an error: falling back to the root would show the wrong folder.
func (suite *FileHandlerTestSuite) TestList_InvalidParentID() {
	for _, query := range []string{"?parent_id=abc", "?parent_id=0", "?parent_id=-4"} {
		w := suite.list(query, true)

		suite.Equal(http.StatusBadRequest, w.Code, query)
		suite.Equal("invalid parent_id", suite.errorMessage(w), query)
	}
}

func (suite *FileHandlerTestSuite) TestList_SortAndSearch() {
	cases := []struct {
		query string
		want  file.ListQuery
	}{
		{"?sort=name&order=asc", file.ListQuery{Sort: file.SortByName}},
		{"?sort=size&order=desc", file.ListQuery{Sort: file.SortBySize, Desc: true}},
		{"?sort=type", file.ListQuery{Sort: file.SortByType, Desc: true}},
		{"?sort=modified&order=asc", file.ListQuery{Sort: file.SortByModified}},
		{"?order=asc", file.ListQuery{Sort: file.SortByDate}},
		{"?search=%20Report%25_", file.ListQuery{Search: " Report%_", Sort: file.SortByDate, Desc: true}},
		{"?search=" + atNameLimit, file.ListQuery{Search: atNameLimit, Sort: file.SortByDate, Desc: true}},
		{"?show_hidden=true", file.ListQuery{Sort: file.SortByDate, Desc: true, ShowHidden: true}},
		{"?show_hidden=false", file.ListQuery{Sort: file.SortByDate, Desc: true}},
	}
	for _, tc := range cases {
		suite.mockService.EXPECT().
			List(mock.Anything, testUserID, tc.want).
			Return(&utils.Page[*file.File]{Data: []*file.File{}}, nil).
			Once()

		suite.Equal(http.StatusOK, suite.list(tc.query, true).Code, tc.query)
	}
}

func (suite *FileHandlerTestSuite) TestList_InvalidShowHidden() {
	w := suite.list("?show_hidden=yes", true)

	suite.Equal(http.StatusBadRequest, w.Code)
	suite.Equal("invalid show_hidden", suite.errorMessage(w))
}

// Sort values are case-sensitive.
func (suite *FileHandlerTestSuite) TestList_InvalidSort() {
	cases := []struct {
		query   string
		message string
	}{
		{"?sort=created_at", "invalid sort"},
		{"?sort=Name", "invalid sort"},
		{"?order=up", "invalid order"},
		{"?order=DESC", "invalid order"},
		{"?search=" + atNameLimit + "a", "invalid search"},
	}
	for _, tc := range cases {
		w := suite.list(tc.query, true)

		suite.Equal(http.StatusBadRequest, w.Code, tc.query)
		suite.Equal(tc.message, suite.errorMessage(w), tc.query)
	}
}

func (suite *FileHandlerTestSuite) TestList_Unauthenticated() {
	w := suite.list("", false)

	suite.Equal(http.StatusUnauthorized, w.Code)
}

func (suite *FileHandlerTestSuite) TestList_ServiceError() {
	suite.mockService.EXPECT().
		List(mock.Anything, mock.Anything, mock.Anything).
		Return(nil, assert.AnError).
		Once()

	w := suite.list("", true)

	suite.Equal(http.StatusInternalServerError, w.Code)
	suite.Equal("something went wrong", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) createFolder(body string, authenticated bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/folders", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authenticated {
		req = req.WithContext(middleware.WithUserID(req.Context(), testUserID))
	}
	w := httptest.NewRecorder()
	suite.router.ServeHTTP(w, req)
	return w
}

func (suite *FileHandlerTestSuite) TestCreateFolder_Success() {
	parentID := int64(12)
	suite.mockService.EXPECT().
		CreateFolder(mock.Anything, testUserID, int64(12), "Photos").
		Return(&file.File{ID: 4, ParentID: &parentID, IsFolder: true, Name: "Photos"}, nil).
		Once()

	w := suite.createFolder(`{"name":"Photos","parent_id":12}`, true)

	suite.Equal(http.StatusCreated, w.Code)
	var resp file.FolderResponse
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	suite.Equal("Photos created.", resp.Message)
	suite.Require().NotNil(resp.Folder)
	suite.True(resp.Folder.IsFolder)
	suite.Equal(&parentID, resp.Folder.ParentID)
}

// A missing or null parent_id reaches the service as 0, the root.
func (suite *FileHandlerTestSuite) TestCreateFolder_AtRoot() {
	suite.mockService.EXPECT().
		CreateFolder(mock.Anything, testUserID, int64(0), "Photos").
		Return(&file.File{ID: 4, IsFolder: true, Name: "Photos"}, nil).
		Twice()

	suite.Equal(http.StatusCreated, suite.createFolder(`{"name":"Photos"}`, true).Code)
	suite.Equal(http.StatusCreated, suite.createFolder(`{"name":"Photos","parent_id":null}`, true).Code)
}

func (suite *FileHandlerTestSuite) TestCreateFolder_InvalidBody() {
	for _, body := range []string{`not json`, `{"name":"Photos","parent_id":"12"}`} {
		w := suite.createFolder(body, true)

		suite.Equal(http.StatusBadRequest, w.Code, body)
		suite.Equal("invalid request body", suite.errorMessage(w), body)
	}
}

func (suite *FileHandlerTestSuite) TestCreateFolder_InvalidParentID() {
	for _, body := range []string{`{"name":"Photos","parent_id":0}`, `{"name":"Photos","parent_id":-4}`} {
		w := suite.createFolder(body, true)

		suite.Equal(http.StatusBadRequest, w.Code, body)
		suite.Equal("invalid parent_id", suite.errorMessage(w), body)
	}
}

func (suite *FileHandlerTestSuite) TestCreateFolder_Unauthenticated() {
	w := suite.createFolder(`{"name":"Photos"}`, false)

	suite.Equal(http.StatusUnauthorized, w.Code)
}

func (suite *FileHandlerTestSuite) TestCreateFolder_ServiceError() {
	suite.mockService.EXPECT().
		CreateFolder(mock.Anything, testUserID, int64(99), "Photos").
		Return(nil, apperr.New(apperr.KindNotFound, "folder not found")).
		Once()

	w := suite.createFolder(`{"name":"Photos","parent_id":99}`, true)

	suite.Equal(http.StatusNotFound, w.Code)
	suite.Equal("folder not found", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestCreateFolderTree_Success() {
	folders := map[string]int64{"Docs": 3, "Photos": 1, "Photos/2024": 2}
	suite.mockService.EXPECT().
		CreateFolderTree(mock.Anything, testUserID, int64(12), []string{"Photos/2024", "Docs"}).
		Return(folders, nil).
		Once()

	w := suite.postItems("/folders/tree", `{"parent_id":12,"paths":["Photos/2024","Docs"]}`, true)

	suite.Equal(http.StatusCreated, w.Code)
	var resp file.FolderTreeResponse
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	suite.Equal("3 folders created.", resp.Message)
	suite.Equal(folders, resp.Folders)
}

func (suite *FileHandlerTestSuite) TestCreateFolderTree_OneFolderAtRoot() {
	suite.mockService.EXPECT().
		CreateFolderTree(mock.Anything, testUserID, int64(0), []string{"Docs"}).
		Return(map[string]int64{"Docs": 3}, nil).
		Once()

	w := suite.postItems("/folders/tree", `{"paths":["Docs"]}`, true)

	suite.Equal(http.StatusCreated, w.Code)
	var resp file.FolderTreeResponse
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	suite.Equal("1 folder created.", resp.Message)
}

func (suite *FileHandlerTestSuite) TestCreateFolderTree_BadRequests() {
	cases := []struct {
		body    string
		message string
	}{
		{`not json`, "invalid request body"},
		{`{"paths":["` + strings.Repeat("a", 16<<20) + `"]}`, "invalid request body"},
		{`{"parent_id":0,"paths":["Docs"]}`, "invalid parent_id"},
	}
	for _, tc := range cases {
		w := suite.postItems("/folders/tree", tc.body, true)

		suite.Equal(http.StatusBadRequest, w.Code, tc.message)
		suite.Equal(tc.message, suite.errorMessage(w))
	}
}

func (suite *FileHandlerTestSuite) TestCreateFolderTree_Unauthenticated() {
	w := suite.postItems("/folders/tree", `{"paths":["Docs"]}`, false)

	suite.Equal(http.StatusUnauthorized, w.Code)
}

func (suite *FileHandlerTestSuite) TestCreateFolderTree_ServiceError() {
	suite.mockService.EXPECT().
		CreateFolderTree(mock.Anything, testUserID, int64(99), []string{"Docs"}).
		Return(nil, apperr.New(apperr.KindNotFound, "folder not found")).
		Once()

	w := suite.postItems("/folders/tree", `{"parent_id":99,"paths":["Docs"]}`, true)

	suite.Equal(http.StatusNotFound, w.Code)
	suite.Equal("folder not found", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) resolveFolder(dir string, authenticated bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/folders/resolve?"+url.Values{"dir": {dir}}.Encode(), nil)
	if authenticated {
		req = req.WithContext(middleware.WithUserID(req.Context(), testUserID))
	}
	w := httptest.NewRecorder()
	suite.router.ServeHTTP(w, req)
	return w
}

// The names reach the service decoded, spaces and all. Slashes at either end are optional.
func (suite *FileHandlerTestSuite) TestResolveFolder_Success() {
	suite.mockService.EXPECT().
		ResolveFolder(mock.Anything, testUserID, []string{"My Photos", "Cars & Bikes"}).
		Return(&file.FolderPath{Path: []*file.PathEntry{{ID: 3, Name: "My Photos"}, {ID: 7, Name: "Cars & Bikes"}}}, nil).
		Twice()

	for _, dir := range []string{"/My Photos/Cars & Bikes", "My Photos/Cars & Bikes/"} {
		w := suite.resolveFolder(dir, true)

		suite.Equal(http.StatusOK, w.Code, dir)
		var resp file.FolderPath
		suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp), dir)
		suite.Require().Len(resp.Path, 2, dir)
		suite.Equal("My Photos", resp.Path[0].Name, dir)
		suite.Equal(int64(7), resp.Path[1].ID, dir)
	}
}

func (suite *FileHandlerTestSuite) TestResolveFolder_Root() {
	suite.mockService.EXPECT().
		ResolveFolder(mock.Anything, testUserID, []string(nil)).
		Return(&file.FolderPath{Path: []*file.PathEntry{}}, nil).
		Twice()

	for _, dir := range []string{"", "/"} {
		suite.Equal(http.StatusOK, suite.resolveFolder(dir, true).Code, dir)
	}
}

func (suite *FileHandlerTestSuite) TestResolveFolder_InvalidDir() {
	for _, dir := range []string{"/Photos//Cars", "/Photos/./Cars", "/..", "/ ", "/" + strings.Repeat("a", 256)} {
		w := suite.resolveFolder(dir, true)

		suite.Equal(http.StatusBadRequest, w.Code, dir)
		suite.Equal("invalid dir", suite.errorMessage(w), dir)
	}
}

func (suite *FileHandlerTestSuite) TestResolveFolder_Unauthenticated() {
	w := suite.resolveFolder("/Photos", false)

	suite.Equal(http.StatusUnauthorized, w.Code)
}

func (suite *FileHandlerTestSuite) TestResolveFolder_ServiceError() {
	suite.mockService.EXPECT().
		ResolveFolder(mock.Anything, testUserID, []string{"Nope"}).
		Return(nil, apperr.New(apperr.KindNotFound, "folder not found")).
		Once()

	w := suite.resolveFolder("/Nope", true)

	suite.Equal(http.StatusNotFound, w.Code)
	suite.Equal("folder not found", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) rename(id, body string, authenticated bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPatch, "/files/"+id, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authenticated {
		req = req.WithContext(middleware.WithUserID(req.Context(), testUserID))
	}
	w := httptest.NewRecorder()
	suite.router.ServeHTTP(w, req)
	return w
}

func (suite *FileHandlerTestSuite) TestRename_Success() {
	suite.mockService.EXPECT().
		Rename(mock.Anything, testUserID, int64(5), "report.md").
		Return(&file.File{ID: 5, Name: "report.md"}, nil).
		Once()

	w := suite.rename("5", `{"name":"report.md"}`, true)

	suite.Equal(http.StatusOK, w.Code)
	var resp file.RenameResponse
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	suite.Equal("Renamed to report.md.", resp.Message)
	suite.Require().NotNil(resp.File)
	suite.Equal(int64(5), resp.File.ID)
}

func (suite *FileHandlerTestSuite) TestRename_InvalidID() {
	for _, id := range []string{"abc", "0", "-4"} {
		w := suite.rename(id, `{"name":"a.txt"}`, true)

		suite.Equal(http.StatusBadRequest, w.Code, id)
		suite.Equal("invalid id", suite.errorMessage(w), id)
	}
}

func (suite *FileHandlerTestSuite) TestRename_InvalidBody() {
	for _, body := range []string{`not json`, `{"name":5}`} {
		w := suite.rename("5", body, true)

		suite.Equal(http.StatusBadRequest, w.Code, body)
		suite.Equal("invalid request body", suite.errorMessage(w), body)
	}
}

func (suite *FileHandlerTestSuite) TestRename_Unauthenticated() {
	w := suite.rename("5", `{"name":"a.txt"}`, false)

	suite.Equal(http.StatusUnauthorized, w.Code)
}

func (suite *FileHandlerTestSuite) TestRename_ServiceError() {
	suite.mockService.EXPECT().
		Rename(mock.Anything, testUserID, int64(5), "b.txt").
		Return(nil, apperr.New(apperr.KindConflict, "an item with this name already exists")).
		Once()

	w := suite.rename("5", `{"name":"b.txt"}`, true)

	suite.Equal(http.StatusConflict, w.Code)
	suite.Equal("an item with this name already exists", suite.errorMessage(w))
}

// trackedContent is file bytes that remember a Close.
type trackedContent struct {
	*bytes.Reader
	closed bool
}

func (c *trackedContent) Close() error {
	c.closed = true
	return nil
}

func (suite *FileHandlerTestSuite) download(id string, header http.Header, authenticated bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/files/"+id+"/download", nil)
	for k, v := range header {
		req.Header[k] = v
	}
	if authenticated {
		req = req.WithContext(middleware.WithUserID(req.Context(), testUserID))
	}
	w := httptest.NewRecorder()
	suite.router.ServeHTTP(w, req)
	return w
}

// expectOpen makes OpenFile return name with "hello world" as its bytes, once.
func (suite *FileHandlerTestSuite) expectOpen(name string) *trackedContent {
	content := &trackedContent{Reader: bytes.NewReader([]byte("hello world"))}
	suite.mockService.EXPECT().
		OpenFile(mock.Anything, testUserID, int64(5)).
		Return(&file.FileContent{
			Name:        name,
			ContentType: "text/plain; charset=utf-8",
			ModTime:     time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC),
			ETag:        `"abc123"`,
			Content:     content,
		}, nil).
		Once()
	return content
}

func (suite *FileHandlerTestSuite) TestDownload_Success() {
	content := suite.expectOpen("notes.txt")

	w := suite.download("5", nil, true)

	suite.Equal(http.StatusOK, w.Code)
	suite.Equal("hello world", w.Body.String())
	suite.Equal("text/plain; charset=utf-8", w.Header().Get("Content-Type"))
	suite.Equal("attachment; filename=notes.txt", w.Header().Get("Content-Disposition"))
	suite.Equal("nosniff", w.Header().Get("X-Content-Type-Options"))
	suite.Equal(`"abc123"`, w.Header().Get("ETag"))
	suite.Equal("private, no-cache", w.Header().Get("Cache-Control"))
	suite.Equal("bytes", w.Header().Get("Accept-Ranges"))
	suite.Equal("11", w.Header().Get("Content-Length"))
	suite.True(content.closed)
}

// A name that is not ASCII goes in the RFC 2231 form, which browsers read as UTF-8.
func (suite *FileHandlerTestSuite) TestDownload_NameNotASCII() {
	suite.expectOpen("poročilo 1.txt")

	w := suite.download("5", nil, true)

	suite.Equal(http.StatusOK, w.Code)
	suite.Equal("attachment; filename*=utf-8''poro%C4%8Dilo%201.txt", w.Header().Get("Content-Disposition"))
}

func (suite *FileHandlerTestSuite) TestDownload_Range() {
	suite.expectOpen("notes.txt")

	w := suite.download("5", http.Header{"Range": {"bytes=6-10"}}, true)

	suite.Equal(http.StatusPartialContent, w.Code)
	suite.Equal("world", w.Body.String())
	suite.Equal("bytes 6-10/11", w.Header().Get("Content-Range"))
}

func (suite *FileHandlerTestSuite) TestDownload_NotModified() {
	suite.expectOpen("notes.txt")

	w := suite.download("5", http.Header{"If-None-Match": {`"abc123"`}}, true)

	suite.Equal(http.StatusNotModified, w.Code)
	suite.Empty(w.Body.String())
}

func (suite *FileHandlerTestSuite) TestDownload_InvalidID() {
	for _, id := range []string{"abc", "0", "-4"} {
		w := suite.download(id, nil, true)

		suite.Equal(http.StatusBadRequest, w.Code, id)
		suite.Equal("invalid id", suite.errorMessage(w), id)
	}
}

func (suite *FileHandlerTestSuite) TestDownload_Unauthenticated() {
	w := suite.download("5", nil, false)

	suite.Equal(http.StatusUnauthorized, w.Code)
}

func (suite *FileHandlerTestSuite) TestDownload_ServiceError() {
	suite.mockService.EXPECT().
		OpenFile(mock.Anything, testUserID, int64(5)).
		Return(nil, apperr.New(apperr.KindValidation, "a folder downloads as a zip")).
		Once()

	w := suite.download("5", nil, true)

	suite.Equal(http.StatusBadRequest, w.Code)
	suite.Equal("a folder downloads as a zip", suite.errorMessage(w))
	suite.Empty(w.Header().Get("Content-Disposition"))
}

func (suite *FileHandlerTestSuite) getPath(path string, authenticated bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if authenticated {
		req = req.WithContext(middleware.WithUserID(req.Context(), testUserID))
	}
	w := httptest.NewRecorder()
	suite.router.ServeHTTP(w, req)
	return w
}

func (suite *FileHandlerTestSuite) TestRequestZip_Success() {
	suite.mockService.EXPECT().
		RequestZip(mock.Anything, testUserID, []int64{5, 3}).
		Return(&file.Download{ID: 9, Name: "Docs.zip", State: file.DownloadBuilding}, nil).
		Once()

	w := suite.postItems("/downloads", `{"ids":[5,3]}`, true)

	suite.Equal(http.StatusAccepted, w.Code)
	var resp file.DownloadResponse
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	suite.Equal("Preparing Docs.zip.", resp.Message)
	suite.Require().NotNil(resp.Download)
	suite.Equal(int64(9), resp.Download.ID)
	suite.Equal(file.DownloadBuilding, resp.Download.State)
}

func (suite *FileHandlerTestSuite) TestRequestZip_InvalidIDs() {
	for _, body := range []string{`{"ids":[]}`, `{}`, `{"ids":[0]}`, `{"ids":[-1]}`} {
		w := suite.postItems("/downloads", body, true)

		suite.Equal(http.StatusBadRequest, w.Code, body)
		suite.Equal("invalid ids", suite.errorMessage(w), body)
	}
}

func (suite *FileHandlerTestSuite) TestRequestZip_InvalidBody() {
	w := suite.postItems("/downloads", `not json`, true)

	suite.Equal(http.StatusBadRequest, w.Code)
	suite.Equal("invalid request body", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestRequestZip_Unauthenticated() {
	w := suite.postItems("/downloads", `{"ids":[5]}`, false)

	suite.Equal(http.StatusUnauthorized, w.Code)
}

func (suite *FileHandlerTestSuite) TestRequestZip_ServiceError() {
	suite.mockService.EXPECT().
		RequestZip(mock.Anything, testUserID, []int64{5}).
		Return(nil, apperr.New(apperr.KindConflict, "wait for your other downloads to finish")).
		Once()

	w := suite.postItems("/downloads", `{"ids":[5]}`, true)

	suite.Equal(http.StatusConflict, w.Code)
	suite.Equal("wait for your other downloads to finish", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestListDownloads_Success() {
	expires := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	suite.mockService.EXPECT().
		ListDownloads(mock.Anything, testUserID).
		Return([]*file.Download{
			{ID: 9, Name: "Docs.zip", State: file.DownloadReady, SizeBytes: 120, ExpiresAt: expires, Downloaded: true},
			{ID: 8, Name: "Home.zip", State: file.DownloadBuilding, ExpiresAt: expires},
		}, nil).
		Once()

	w := suite.getPath("/downloads", true)

	suite.Equal(http.StatusOK, w.Code)
	var resp file.DownloadList
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	suite.Require().Len(resp.Downloads, 2)
	suite.Equal(int64(9), resp.Downloads[0].ID)
	suite.Equal(file.DownloadReady, resp.Downloads[0].State)
	suite.True(resp.Downloads[0].Downloaded)
	suite.Equal(expires, resp.Downloads[0].ExpiresAt)
	suite.Equal(file.DownloadBuilding, resp.Downloads[1].State)
}

// An empty list is an empty array, not null, so the client needs no check.
func (suite *FileHandlerTestSuite) TestListDownloads_Empty() {
	suite.mockService.EXPECT().ListDownloads(mock.Anything, testUserID).Return([]*file.Download{}, nil).Once()

	w := suite.getPath("/downloads", true)

	suite.Equal(http.StatusOK, w.Code)
	suite.JSONEq(`{"downloads":[]}`, w.Body.String())
}

func (suite *FileHandlerTestSuite) TestListDownloads_Unauthenticated() {
	w := suite.getPath("/downloads", false)

	suite.Equal(http.StatusUnauthorized, w.Code)
}

func (suite *FileHandlerTestSuite) TestListDownloads_ServiceError() {
	suite.mockService.EXPECT().ListDownloads(mock.Anything, testUserID).Return(nil, assert.AnError).Once()

	w := suite.getPath("/downloads", true)

	suite.Equal(http.StatusInternalServerError, w.Code)
}

func (suite *FileHandlerTestSuite) deletePath(path string, authenticated bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	if authenticated {
		req = req.WithContext(middleware.WithUserID(req.Context(), testUserID))
	}
	w := httptest.NewRecorder()
	suite.router.ServeHTTP(w, req)
	return w
}

func (suite *FileHandlerTestSuite) TestRemoveDownload_Success() {
	suite.mockService.EXPECT().RemoveDownload(mock.Anything, testUserID, int64(9)).Return(nil).Once()

	w := suite.deletePath("/downloads/9", true)

	suite.Equal(http.StatusOK, w.Code)
	var resp apperr.SuccessResponse
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	suite.Equal("Removed.", resp.Message)
}

func (suite *FileHandlerTestSuite) TestRemoveDownload_InvalidID() {
	w := suite.deletePath("/downloads/abc", true)

	suite.Equal(http.StatusBadRequest, w.Code)
	suite.Equal("invalid id", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestRemoveDownload_Unauthenticated() {
	w := suite.deletePath("/downloads/9", false)

	suite.Equal(http.StatusUnauthorized, w.Code)
}

func (suite *FileHandlerTestSuite) TestRemoveDownload_ServiceError() {
	suite.mockService.EXPECT().
		RemoveDownload(mock.Anything, testUserID, int64(9)).
		Return(apperr.New(apperr.KindNotFound, "download not found")).
		Once()

	w := suite.deletePath("/downloads/9", true)

	suite.Equal(http.StatusNotFound, w.Code)
	suite.Equal("download not found", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestDownloadZip_Success() {
	content := &trackedContent{Reader: bytes.NewReader([]byte("PK zip"))}
	suite.mockService.EXPECT().
		OpenZip(mock.Anything, testUserID, int64(9)).
		Return(&file.FileContent{
			Name:        "Docs.zip",
			ContentType: "application/zip",
			ModTime:     time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC),
			ETag:        `"zip123"`,
			Content:     content,
		}, nil).
		Once()

	w := suite.getPath("/downloads/9/file", true)

	suite.Equal(http.StatusOK, w.Code)
	suite.Equal("PK zip", w.Body.String())
	suite.Equal("application/zip", w.Header().Get("Content-Type"))
	suite.Equal("attachment; filename=Docs.zip", w.Header().Get("Content-Disposition"))
	suite.Equal("nosniff", w.Header().Get("X-Content-Type-Options"))
	suite.Equal(`"zip123"`, w.Header().Get("ETag"))
	suite.True(content.closed)
}

func (suite *FileHandlerTestSuite) TestDownloadZip_NotReady() {
	suite.mockService.EXPECT().
		OpenZip(mock.Anything, testUserID, int64(9)).
		Return(nil, apperr.New(apperr.KindConflict, "the zip is not ready yet")).
		Once()

	w := suite.getPath("/downloads/9/file", true)

	suite.Equal(http.StatusConflict, w.Code)
	suite.Equal("the zip is not ready yet", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestDownloadZip_Unauthenticated() {
	w := suite.getPath("/downloads/9/file", false)

	suite.Equal(http.StatusUnauthorized, w.Code)
}

func (suite *FileHandlerTestSuite) postItems(path, body string, authenticated bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authenticated {
		req = req.WithContext(middleware.WithUserID(req.Context(), testUserID))
	}
	w := httptest.NewRecorder()
	suite.router.ServeHTTP(w, req)
	return w
}

func (suite *FileHandlerTestSuite) TestMove_Success() {
	suite.mockService.EXPECT().
		Move(mock.Anything, testUserID, int64(9), []int64{5, 3}).
		Return(nil).
		Once()

	w := suite.postItems("/files/move", `{"ids":[5,3],"parent_id":9}`, true)

	suite.Equal(http.StatusOK, w.Code)
	var resp apperr.SuccessResponse
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	suite.Equal("Moved.", resp.Message)
}

// A missing or null parent_id is the root.
func (suite *FileHandlerTestSuite) TestMove_ToRoot() {
	suite.mockService.EXPECT().
		Move(mock.Anything, testUserID, int64(0), []int64{5}).
		Return(nil).
		Twice()

	for _, body := range []string{`{"ids":[5]}`, `{"ids":[5],"parent_id":null}`} {
		w := suite.postItems("/files/move", body, true)

		suite.Equal(http.StatusOK, w.Code, body)
	}
}

func (suite *FileHandlerTestSuite) TestMove_InvalidIDs() {
	tooMany := strings.TrimSuffix(strings.Repeat("1,", 101), ",")
	for _, ids := range []string{`[]`, `null`, `[0]`, `[4,-1]`, `[` + tooMany + `]`} {
		body := `{"ids":` + ids + `,"parent_id":9}`
		w := suite.postItems("/files/move", body, true)

		suite.Equal(http.StatusBadRequest, w.Code, body)
		suite.Equal("invalid ids", suite.errorMessage(w), body)
	}
}

func (suite *FileHandlerTestSuite) TestMove_InvalidParentID() {
	for _, body := range []string{`{"ids":[5],"parent_id":0}`, `{"ids":[5],"parent_id":-1}`} {
		w := suite.postItems("/files/move", body, true)

		suite.Equal(http.StatusBadRequest, w.Code, body)
		suite.Equal("invalid parent_id", suite.errorMessage(w), body)
	}
}

func (suite *FileHandlerTestSuite) TestMove_InvalidBody() {
	for _, body := range []string{`not json`, `{"ids":"5"}`, `{"ids":[5],"parent_id":"9"}`} {
		w := suite.postItems("/files/move", body, true)

		suite.Equal(http.StatusBadRequest, w.Code, body)
		suite.Equal("invalid request body", suite.errorMessage(w), body)
	}
}

func (suite *FileHandlerTestSuite) TestMove_Unauthenticated() {
	w := suite.postItems("/files/move", `{"ids":[5]}`, false)

	suite.Equal(http.StatusUnauthorized, w.Code)
}

func (suite *FileHandlerTestSuite) TestMove_ServiceError() {
	suite.mockService.EXPECT().
		Move(mock.Anything, testUserID, int64(9), []int64{5}).
		Return(apperr.New(apperr.KindConflict, "a.txt already exists in this folder")).
		Once()

	w := suite.postItems("/files/move", `{"ids":[5],"parent_id":9}`, true)

	suite.Equal(http.StatusConflict, w.Code)
	suite.Equal("a.txt already exists in this folder", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestCopy_Success() {
	suite.mockService.EXPECT().
		Copy(mock.Anything, testUserID, int64(9), []int64{5, 3}).
		Return(nil).
		Once()

	w := suite.postItems("/files/copy", `{"ids":[5,3],"parent_id":9}`, true)

	suite.Equal(http.StatusOK, w.Code)
	var resp apperr.SuccessResponse
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	suite.Equal("Copying.", resp.Message)
}

// A missing or null parent_id is the root.
func (suite *FileHandlerTestSuite) TestCopy_ToRoot() {
	suite.mockService.EXPECT().
		Copy(mock.Anything, testUserID, int64(0), []int64{5}).
		Return(nil).
		Twice()

	for _, body := range []string{`{"ids":[5]}`, `{"ids":[5],"parent_id":null}`} {
		w := suite.postItems("/files/copy", body, true)

		suite.Equal(http.StatusOK, w.Code, body)
	}
}

func (suite *FileHandlerTestSuite) TestCopy_InvalidIDs() {
	tooMany := strings.TrimSuffix(strings.Repeat("1,", 101), ",")
	for _, ids := range []string{`[]`, `null`, `[0]`, `[4,-1]`, `[` + tooMany + `]`} {
		body := `{"ids":` + ids + `,"parent_id":9}`
		w := suite.postItems("/files/copy", body, true)

		suite.Equal(http.StatusBadRequest, w.Code, body)
		suite.Equal("invalid ids", suite.errorMessage(w), body)
	}
}

func (suite *FileHandlerTestSuite) TestCopy_InvalidParentID() {
	for _, body := range []string{`{"ids":[5],"parent_id":0}`, `{"ids":[5],"parent_id":-1}`} {
		w := suite.postItems("/files/copy", body, true)

		suite.Equal(http.StatusBadRequest, w.Code, body)
		suite.Equal("invalid parent_id", suite.errorMessage(w), body)
	}
}

func (suite *FileHandlerTestSuite) TestCopy_InvalidBody() {
	for _, body := range []string{`not json`, `{"ids":"5"}`, `{"ids":[5],"parent_id":"9"}`} {
		w := suite.postItems("/files/copy", body, true)

		suite.Equal(http.StatusBadRequest, w.Code, body)
		suite.Equal("invalid request body", suite.errorMessage(w), body)
	}
}

func (suite *FileHandlerTestSuite) TestCopy_Unauthenticated() {
	w := suite.postItems("/files/copy", `{"ids":[5]}`, false)

	suite.Equal(http.StatusUnauthorized, w.Code)
}

func (suite *FileHandlerTestSuite) TestCopy_ServiceError() {
	suite.mockService.EXPECT().
		Copy(mock.Anything, testUserID, int64(9), []int64{5}).
		Return(apperr.New(apperr.KindConflict, "Docs is still copying")).
		Once()

	w := suite.postItems("/files/copy", `{"ids":[5],"parent_id":9}`, true)

	suite.Equal(http.StatusConflict, w.Code)
	suite.Equal("Docs is still copying", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestGetItem_Success() {
	suite.mockService.EXPECT().
		GetItem(mock.Anything, testUserID, int64(9)).
		Return(&file.File{ID: 9, Name: "a.txt", SizeBytes: 5, IsFavorite: true}, nil).
		Once()

	w := suite.getPath("/files/9", true)

	suite.Equal(http.StatusOK, w.Code)
	var resp file.File
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	suite.Equal("a.txt", resp.Name)
	suite.True(resp.IsFavorite)
}

func (suite *FileHandlerTestSuite) TestGetItem_Errors() {
	w := suite.getPath("/files/abc", true)
	suite.Equal(http.StatusBadRequest, w.Code)
	suite.Equal("invalid id", suite.errorMessage(w))

	w = suite.getPath("/files/9", false)
	suite.Equal(http.StatusUnauthorized, w.Code)

	suite.mockService.EXPECT().
		GetItem(mock.Anything, testUserID, int64(9)).
		Return(nil, apperr.New(apperr.KindNotFound, "item not found")).
		Once()
	w = suite.getPath("/files/9", true)
	suite.Equal(http.StatusNotFound, w.Code)
	suite.Equal("item not found", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestListActivity_Success() {
	next := "next"
	suite.mockService.EXPECT().
		ListActivity(mock.Anything, testUserID, int64(9), file.ActivityQuery{Cursor: "abc", PageSize: 10}).
		Return(&utils.Page[*file.Activity]{
			Data:       []*file.Activity{{ID: 3, Event: file.EventRenamed, Metadata: []byte(`{"name":"b.txt","old_name":"a.txt"}`)}},
			NextCursor: &next,
		}, nil).
		Once()

	w := suite.getPath("/files/9/activity?cursor=abc&page_size=10", true)

	suite.Equal(http.StatusOK, w.Code)
	var resp utils.Page[*file.Activity]
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	suite.Require().Len(resp.Data, 1)
	suite.Equal(file.EventRenamed, resp.Data[0].Event)
	suite.JSONEq(`{"name":"b.txt","old_name":"a.txt"}`, string(resp.Data[0].Metadata))
	suite.Equal(&next, resp.NextCursor)
}

func (suite *FileHandlerTestSuite) TestListActivity_Errors() {
	w := suite.getPath("/files/abc/activity", true)
	suite.Equal(http.StatusBadRequest, w.Code)
	suite.Equal("invalid id", suite.errorMessage(w))

	w = suite.getPath("/files/9/activity", false)
	suite.Equal(http.StatusUnauthorized, w.Code)

	suite.mockService.EXPECT().
		ListActivity(mock.Anything, testUserID, int64(9), file.ActivityQuery{Cursor: "bad"}).
		Return(nil, apperr.New(apperr.KindInvalid, "invalid cursor")).
		Once()
	w = suite.getPath("/files/9/activity?cursor=bad", true)
	suite.Equal(http.StatusBadRequest, w.Code)
	suite.Equal("invalid cursor", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestAddFavorites_Success() {
	suite.mockService.EXPECT().AddFavorites(mock.Anything, testUserID, []int64{5, 3}).Return(nil).Once()

	w := suite.postItems("/favorites", `{"ids":[5,3]}`, true)

	suite.Equal(http.StatusOK, w.Code)
	var resp apperr.SuccessResponse
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	suite.Equal("Added to favorites.", resp.Message)
}

func (suite *FileHandlerTestSuite) TestRemoveFavorites_Success() {
	suite.mockService.EXPECT().RemoveFavorites(mock.Anything, testUserID, []int64{5}).Return(nil).Once()

	w := suite.postItems("/favorites/remove", `{"ids":[5]}`, true)

	suite.Equal(http.StatusOK, w.Code)
	var resp apperr.SuccessResponse
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	suite.Equal("Removed from favorites.", resp.Message)
}

func (suite *FileHandlerTestSuite) TestFavorites_InvalidInput() {
	for _, path := range []string{"/favorites", "/favorites/remove"} {
		for _, body := range []string{`{"ids":[]}`, `{}`, `{"ids":[0]}`} {
			w := suite.postItems(path, body, true)

			suite.Equal(http.StatusBadRequest, w.Code, path+" "+body)
			suite.Equal("invalid ids", suite.errorMessage(w), path+" "+body)
		}

		w := suite.postItems(path, `not json`, true)
		suite.Equal(http.StatusBadRequest, w.Code, path)
		suite.Equal("invalid request body", suite.errorMessage(w), path)

		w = suite.postItems(path, `{"ids":[5]}`, false)
		suite.Equal(http.StatusUnauthorized, w.Code, path)
	}
}

func (suite *FileHandlerTestSuite) TestAddFavorites_ServiceError() {
	suite.mockService.EXPECT().
		AddFavorites(mock.Anything, testUserID, []int64{5}).
		Return(apperr.New(apperr.KindNotFound, "item not found")).
		Once()

	w := suite.postItems("/favorites", `{"ids":[5]}`, true)

	suite.Equal(http.StatusNotFound, w.Code)
	suite.Equal("item not found", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestRemoveFavorites_ServiceError() {
	suite.mockService.EXPECT().RemoveFavorites(mock.Anything, testUserID, []int64{5}).Return(assert.AnError).Once()

	w := suite.postItems("/favorites/remove", `{"ids":[5]}`, true)

	suite.Equal(http.StatusInternalServerError, w.Code)
}

func (suite *FileHandlerTestSuite) TestListFavorites_Success() {
	next := "next-page"
	suite.mockService.EXPECT().
		ListFavorites(mock.Anything, testUserID, file.FavoriteQuery{Cursor: "this-page", PageSize: 5, Sort: file.SortByName}).
		Return(&utils.Page[*file.Favorite]{
			Data: []*file.Favorite{
				{File: file.File{ID: 3, Name: "c.txt", IsFavorite: true}, Path: []string{"Docs", "Work"}},
				{File: file.File{ID: 4, Name: "d.txt", IsFavorite: true}, Path: []string{}},
			},
			NextCursor: &next,
		}, nil).
		Once()

	w := suite.getPath("/favorites?cursor=this-page&page_size=5", true)

	suite.Equal(http.StatusOK, w.Code)
	var resp struct {
		Data []struct {
			Name       string   `json:"name"`
			IsFavorite bool     `json:"is_favorite"`
			Path       []string `json:"path"`
		} `json:"data"`
		NextCursor *string `json:"next_cursor"`
	}
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	suite.Require().Len(resp.Data, 2)
	suite.Equal("c.txt", resp.Data[0].Name)
	suite.True(resp.Data[0].IsFavorite)
	suite.Equal([]string{"Docs", "Work"}, resp.Data[0].Path)
	suite.Equal([]string{}, resp.Data[1].Path)
	suite.Require().NotNil(resp.NextCursor)
	suite.Equal(next, *resp.NextCursor)
}

// The list sorts by name, A to Z, unless the query asks for another sort. A bad page size reaches the service as zero.
func (suite *FileHandlerTestSuite) TestListFavorites_Query() {
	cases := []struct {
		query string
		want  file.FavoriteQuery
	}{
		{"", file.FavoriteQuery{Sort: file.SortByName}},
		{"?page_size=-x", file.FavoriteQuery{Sort: file.SortByName}},
		{"?sort=date&order=desc", file.FavoriteQuery{Sort: file.SortByDate, Desc: true}},
		{"?sort=size", file.FavoriteQuery{Sort: file.SortBySize}},
		{"?sort=type&order=asc", file.FavoriteQuery{Sort: file.SortByType}},
		{"?show_hidden=true", file.FavoriteQuery{Sort: file.SortByName, ShowHidden: true}},
		{"?show_hidden=false", file.FavoriteQuery{Sort: file.SortByName}},
	}
	for _, tc := range cases {
		suite.mockService.EXPECT().
			ListFavorites(mock.Anything, testUserID, tc.want).
			Return(&utils.Page[*file.Favorite]{Data: []*file.Favorite{}}, nil).
			Once()

		w := suite.getPath("/favorites"+tc.query, true)

		suite.Equal(http.StatusOK, w.Code, tc.query)
		suite.JSONEq(`{"data":[],"next_cursor":null}`, w.Body.String(), tc.query)
	}
}

func (suite *FileHandlerTestSuite) TestListFavorites_InvalidQuery() {
	cases := []struct {
		query   string
		message string
	}{
		{"?sort=created_at", "invalid sort"},
		{"?sort=modified", "invalid sort"},
		{"?order=DESC", "invalid order"},
		{"?show_hidden=yes", "invalid show_hidden"},
	}
	for _, tc := range cases {
		w := suite.getPath("/favorites"+tc.query, true)

		suite.Equal(http.StatusBadRequest, w.Code, tc.query)
		suite.Equal(tc.message, suite.errorMessage(w), tc.query)
	}
}

func (suite *FileHandlerTestSuite) TestListFavorites_Unauthenticated() {
	w := suite.getPath("/favorites", false)

	suite.Equal(http.StatusUnauthorized, w.Code)
}

func (suite *FileHandlerTestSuite) TestListFavorites_ServiceError() {
	suite.mockService.EXPECT().
		ListFavorites(mock.Anything, mock.Anything, mock.Anything).
		Return(nil, apperr.New(apperr.KindInvalid, "invalid cursor")).
		Once()

	w := suite.getPath("/favorites?cursor=bad", true)

	suite.Equal(http.StatusBadRequest, w.Code)
	suite.Equal("invalid cursor", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestHide_Success() {
	cases := []struct {
		path    string
		hidden  bool
		message string
	}{
		{"/files/hide", true, "Hidden."},
		{"/files/unhide", false, "Unhidden."},
	}
	for _, tc := range cases {
		suite.mockService.EXPECT().SetHidden(mock.Anything, testUserID, []int64{5, 3}, tc.hidden).Return(nil).Once()

		w := suite.postItems(tc.path, `{"ids":[5,3]}`, true)

		suite.Equal(http.StatusOK, w.Code, tc.path)
		var resp apperr.SuccessResponse
		suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
		suite.Equal(tc.message, resp.Message, tc.path)
	}
}

func (suite *FileHandlerTestSuite) TestHide_InvalidInput() {
	for _, path := range []string{"/files/hide", "/files/unhide"} {
		for _, body := range []string{`{"ids":[]}`, `{}`, `{"ids":[-1]}`} {
			w := suite.postItems(path, body, true)

			suite.Equal(http.StatusBadRequest, w.Code, path+" "+body)
			suite.Equal("invalid ids", suite.errorMessage(w), path+" "+body)
		}

		w := suite.postItems(path, `not json`, true)
		suite.Equal(http.StatusBadRequest, w.Code, path)
		suite.Equal("invalid request body", suite.errorMessage(w), path)

		w = suite.postItems(path, `{"ids":[5]}`, false)
		suite.Equal(http.StatusUnauthorized, w.Code, path)
	}
}

func (suite *FileHandlerTestSuite) TestHide_ServiceError() {
	suite.mockService.EXPECT().
		SetHidden(mock.Anything, testUserID, []int64{5}, true).
		Return(apperr.New(apperr.KindNotFound, "item not found")).
		Once()

	w := suite.postItems("/files/hide", `{"ids":[5]}`, true)

	suite.Equal(http.StatusNotFound, w.Code)
	suite.Equal("item not found", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestTrash_Success() {
	suite.mockService.EXPECT().
		Trash(mock.Anything, testUserID, []int64{5, 3, 5}).
		Return(nil).
		Once()

	w := suite.postItems("/trash", `{"ids":[5,3,5]}`, true)

	suite.Equal(http.StatusOK, w.Code)
	var resp apperr.SuccessResponse
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	suite.Equal("Moved to the trash.", resp.Message)
}

func (suite *FileHandlerTestSuite) TestTrash_AtLimit() {
	ids := make([]string, 100)
	for i := range ids {
		ids[i] = strconv.Itoa(i + 1)
	}
	suite.mockService.EXPECT().
		Trash(mock.Anything, testUserID, mock.MatchedBy(func(got []int64) bool { return len(got) == 100 })).
		Return(nil).
		Once()

	w := suite.postItems("/trash", `{"ids":[`+strings.Join(ids, ",")+`]}`, true)

	suite.Equal(http.StatusOK, w.Code)
}

func (suite *FileHandlerTestSuite) TestTrash_InvalidIDs() {
	tooMany := strings.TrimSuffix(strings.Repeat("1,", 101), ",")
	for _, body := range []string{`{}`, `{"ids":[]}`, `{"ids":null}`, `{"ids":[0]}`, `{"ids":[4,-1]}`, `{"ids":[` + tooMany + `]}`} {
		w := suite.postItems("/trash", body, true)

		suite.Equal(http.StatusBadRequest, w.Code, body)
		suite.Equal("invalid ids", suite.errorMessage(w), body)
	}
}

func (suite *FileHandlerTestSuite) TestTrash_InvalidBody() {
	for _, body := range []string{`not json`, `{"ids":"5"}`, `{"ids":["a"]}`} {
		w := suite.postItems("/trash", body, true)

		suite.Equal(http.StatusBadRequest, w.Code, body)
		suite.Equal("invalid request body", suite.errorMessage(w), body)
	}
}

func (suite *FileHandlerTestSuite) TestTrash_Unauthenticated() {
	w := suite.postItems("/trash", `{"ids":[5]}`, false)

	suite.Equal(http.StatusUnauthorized, w.Code)
}

func (suite *FileHandlerTestSuite) TestTrash_ServiceError() {
	suite.mockService.EXPECT().
		Trash(mock.Anything, testUserID, []int64{5}).
		Return(apperr.New(apperr.KindNotFound, "item not found")).
		Once()

	w := suite.postItems("/trash", `{"ids":[5]}`, true)

	suite.Equal(http.StatusNotFound, w.Code)
	suite.Equal("item not found", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestRestore_Success() {
	suite.mockService.EXPECT().
		Restore(mock.Anything, testUserID, []int64{5, 3, 5}).
		Return(nil).
		Once()

	w := suite.postItems("/trash/restore", `{"ids":[5,3,5]}`, true)

	suite.Equal(http.StatusOK, w.Code)
	var resp apperr.SuccessResponse
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	suite.Equal("Restored from the trash.", resp.Message)
}

func (suite *FileHandlerTestSuite) TestRestore_InvalidIDs() {
	tooMany := strings.TrimSuffix(strings.Repeat("1,", 101), ",")
	for _, body := range []string{`{}`, `{"ids":[]}`, `{"ids":[0]}`, `{"ids":[` + tooMany + `]}`} {
		w := suite.postItems("/trash/restore", body, true)

		suite.Equal(http.StatusBadRequest, w.Code, body)
		suite.Equal("invalid ids", suite.errorMessage(w), body)
	}
}

func (suite *FileHandlerTestSuite) TestRestore_InvalidBody() {
	w := suite.postItems("/trash/restore", `not json`, true)

	suite.Equal(http.StatusBadRequest, w.Code)
	suite.Equal("invalid request body", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestRestore_Unauthenticated() {
	w := suite.postItems("/trash/restore", `{"ids":[5]}`, false)

	suite.Equal(http.StatusUnauthorized, w.Code)
}

func (suite *FileHandlerTestSuite) TestRestore_ServiceError() {
	suite.mockService.EXPECT().
		Restore(mock.Anything, testUserID, []int64{5}).
		Return(apperr.New(apperr.KindNotFound, "item not found")).
		Once()

	w := suite.postItems("/trash/restore", `{"ids":[5]}`, true)

	suite.Equal(http.StatusNotFound, w.Code)
	suite.Equal("item not found", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestPurge_Success() {
	suite.mockService.EXPECT().
		Purge(mock.Anything, testUserID, []int64{5, 3, 5}).
		Return(nil).
		Once()

	w := suite.postItems("/trash/purge", `{"ids":[5,3,5]}`, true)

	suite.Equal(http.StatusOK, w.Code)
	var resp apperr.SuccessResponse
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	suite.Equal("Deleted forever.", resp.Message)
}

func (suite *FileHandlerTestSuite) TestPurge_InvalidIDs() {
	tooMany := strings.TrimSuffix(strings.Repeat("1,", 101), ",")
	for _, body := range []string{`{}`, `{"ids":[]}`, `{"ids":[0]}`, `{"ids":[` + tooMany + `]}`} {
		w := suite.postItems("/trash/purge", body, true)

		suite.Equal(http.StatusBadRequest, w.Code, body)
		suite.Equal("invalid ids", suite.errorMessage(w), body)
	}
}

func (suite *FileHandlerTestSuite) TestPurge_InvalidBody() {
	w := suite.postItems("/trash/purge", `not json`, true)

	suite.Equal(http.StatusBadRequest, w.Code)
	suite.Equal("invalid request body", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestPurge_Unauthenticated() {
	w := suite.postItems("/trash/purge", `{"ids":[5]}`, false)

	suite.Equal(http.StatusUnauthorized, w.Code)
}

func (suite *FileHandlerTestSuite) TestPurge_ServiceError() {
	suite.mockService.EXPECT().
		Purge(mock.Anything, testUserID, []int64{5}).
		Return(apperr.New(apperr.KindNotFound, "item not found")).
		Once()

	w := suite.postItems("/trash/purge", `{"ids":[5]}`, true)

	suite.Equal(http.StatusNotFound, w.Code)
	suite.Equal("item not found", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) listTrash(query string, authenticated bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/trash"+query, nil)
	if authenticated {
		req = req.WithContext(middleware.WithUserID(req.Context(), testUserID))
	}
	w := httptest.NewRecorder()
	suite.router.ServeHTTP(w, req)
	return w
}

func (suite *FileHandlerTestSuite) TestListTrash_Success() {
	next := "next-page"
	trashedAt := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	suite.mockService.EXPECT().
		ListTrash(mock.Anything, testUserID, file.TrashQuery{Cursor: "this-page", PageSize: 5, Sort: file.SortByDate, Desc: true}).
		Return(&utils.Page[*file.TrashedFile]{
			Data:       []*file.TrashedFile{{File: file.File{ID: 3, Name: "c.txt"}, TrashedAt: trashedAt}},
			NextCursor: &next,
		}, nil).
		Once()

	w := suite.listTrash("?cursor=this-page&page_size=5", true)

	suite.Equal(http.StatusOK, w.Code)
	var resp struct {
		Data []struct {
			ID        int64     `json:"id"`
			Name      string    `json:"name"`
			TrashedAt time.Time `json:"trashed_at"`
		} `json:"data"`
		NextCursor *string `json:"next_cursor"`
	}
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	suite.Require().Len(resp.Data, 1)
	suite.Equal("c.txt", resp.Data[0].Name)
	suite.True(trashedAt.Equal(resp.Data[0].TrashedAt))
	suite.Require().NotNil(resp.NextCursor)
	suite.Equal(next, *resp.NextCursor)
}

func (suite *FileHandlerTestSuite) TestListTrash_Sort() {
	cases := []struct {
		query string
		want  file.TrashQuery
	}{
		{"", file.TrashQuery{Sort: file.SortByDate, Desc: true}},
		{"?page_size=-x", file.TrashQuery{Sort: file.SortByDate, Desc: true}},
		{"?sort=name&order=asc", file.TrashQuery{Sort: file.SortByName}},
		{"?sort=size", file.TrashQuery{Sort: file.SortBySize, Desc: true}},
		{"?sort=type&order=desc", file.TrashQuery{Sort: file.SortByType, Desc: true}},
	}
	for _, tc := range cases {
		suite.mockService.EXPECT().
			ListTrash(mock.Anything, testUserID, tc.want).
			Return(&utils.Page[*file.TrashedFile]{Data: []*file.TrashedFile{}}, nil).
			Once()

		w := suite.listTrash(tc.query, true)

		suite.Equal(http.StatusOK, w.Code, tc.query)
		suite.JSONEq(`{"data":[],"next_cursor":null}`, w.Body.String(), tc.query)
	}
}

func (suite *FileHandlerTestSuite) TestListTrash_InvalidSort() {
	cases := []struct {
		query   string
		message string
	}{
		{"?sort=trashed_at", "invalid sort"},
		{"?sort=modified", "invalid sort"},
		{"?order=DESC", "invalid order"},
	}
	for _, tc := range cases {
		w := suite.listTrash(tc.query, true)

		suite.Equal(http.StatusBadRequest, w.Code, tc.query)
		suite.Equal(tc.message, suite.errorMessage(w), tc.query)
	}
}

func (suite *FileHandlerTestSuite) TestListTrash_Unauthenticated() {
	w := suite.listTrash("", false)

	suite.Equal(http.StatusUnauthorized, w.Code)
}

func (suite *FileHandlerTestSuite) TestListTrash_ServiceError() {
	suite.mockService.EXPECT().
		ListTrash(mock.Anything, mock.Anything, mock.Anything).
		Return(nil, apperr.New(apperr.KindInvalid, "invalid cursor")).
		Once()

	w := suite.listTrash("?cursor=bad", true)

	suite.Equal(http.StatusBadRequest, w.Code)
	suite.Equal("invalid cursor", suite.errorMessage(w))
}
