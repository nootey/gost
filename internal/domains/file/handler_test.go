package file_test

import (
	"bytes"
	"context"
	"encoding/json"
	"gost/config"
	"gost/internal/apperr"
	"gost/internal/domains/file"
	"gost/internal/middleware"
	"gost/internal/utils"
	"gost/mocks"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
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
}

func (suite *FileHandlerTestSuite) SetupTest() {
	suite.mockService = mocks.NewMockFileServiceInterface(suite.T())
	cfg := config.UploadConfig{Dir: suite.T().TempDir(), MaxSizeMB: 1, TimeoutSeconds: 60}
	suite.handler = file.NewFileHandler(suite.mockService, cfg, config.DownloadConfig{TimeoutSeconds: 60}, apperr.NewResponder(zap.NewNop()))

	suite.router = chi.NewRouter()
	suite.router.Get("/files", suite.handler.List)
	suite.router.Post("/files", suite.handler.Upload)
	suite.router.Post("/folders", suite.handler.CreateFolder)
	suite.router.Post("/favorites", suite.handler.AddFavorites)
	suite.router.Post("/favorites/remove", suite.handler.RemoveFavorites)
	suite.router.Get("/folders/resolve", suite.handler.ResolveFolder)
	suite.router.Patch("/files/{id}", suite.handler.Rename)
	suite.router.Get("/files/{id}/download", suite.handler.Download)
	suite.router.Post("/files/move", suite.handler.Move)
	suite.router.Post("/files/copy", suite.handler.Copy)
	suite.router.Get("/trash", suite.handler.ListTrash)
	suite.router.Post("/trash", suite.handler.Trash)
	suite.router.Post("/trash/restore", suite.handler.Restore)
	suite.router.Post("/trash/purge", suite.handler.Purge)
	suite.router.Post("/downloads", suite.handler.RequestZip)
	suite.router.Get("/downloads", suite.handler.ListDownloads)
	suite.router.Get("/downloads/{id}", suite.handler.GetDownload)
	suite.router.Delete("/downloads/{id}", suite.handler.RemoveDownload)
	suite.router.Get("/downloads/{id}/file", suite.handler.DownloadZip)
}

func TestFileHandlerTestSuite(t *testing.T) {
	suite.Run(t, new(FileHandlerTestSuite))
}

// multipartBody builds a form with optional text fields first, then the file.
func (suite *FileHandlerTestSuite) multipartBody(fields map[string]string, filename, content string) (*bytes.Buffer, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		suite.Require().NoError(mw.WriteField(k, v))
	}
	if filename != "" {
		fw, err := mw.CreateFormFile("file", filename)
		suite.Require().NoError(err)
		_, err = fw.Write([]byte(content))
		suite.Require().NoError(err)
	}
	suite.Require().NoError(mw.Close())
	return &buf, mw.FormDataContentType()
}

func (suite *FileHandlerTestSuite) upload(query string, body io.Reader, contentType string, authenticated bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/files"+query, body)
	req.Header.Set("Content-Type", contentType)
	if authenticated {
		req = req.WithContext(middleware.WithUserID(req.Context(), testUserID))
	}
	w := httptest.NewRecorder()
	suite.router.ServeHTTP(w, req)
	return w
}

func (suite *FileHandlerTestSuite) errorMessage(w *httptest.ResponseRecorder) string {
	var resp apperr.ErrorResponse
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	return resp.Message
}

func (suite *FileHandlerTestSuite) TestUpload_Success() {
	suite.mockService.EXPECT().
		Upload(mock.Anything, testUserID, int64(0), "notes.txt", mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ int64, name string, src io.Reader) (*file.File, error) {
			content, err := io.ReadAll(src)
			suite.Require().NoError(err)
			suite.Equal("hello", string(content))
			return &file.File{ID: 1, Name: name, ContentType: "text/plain; charset=utf-8", SizeBytes: 5}, nil
		}).
		Once()

	body, ct := suite.multipartBody(nil, "notes.txt", "hello")
	w := suite.upload("", body, ct, true)

	suite.Equal(http.StatusCreated, w.Code)
	var resp file.UploadResponse
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	suite.Equal("notes.txt uploaded.", resp.Message)
	suite.Require().NotNil(resp.File)
	suite.Equal(int64(5), resp.File.SizeBytes)
}

func (suite *FileHandlerTestSuite) TestUpload_SkipsFieldsBeforeFile() {
	suite.mockService.EXPECT().
		Upload(mock.Anything, testUserID, int64(0), "a.txt", mock.Anything).
		Return(&file.File{ID: 1, Name: "a.txt"}, nil).
		Once()

	body, ct := suite.multipartBody(map[string]string{"note": "ignored"}, "a.txt", "x")
	w := suite.upload("", body, ct, true)

	suite.Equal(http.StatusCreated, w.Code)
}

func (suite *FileHandlerTestSuite) TestUpload_IntoFolder() {
	suite.mockService.EXPECT().
		Upload(mock.Anything, testUserID, int64(12), "a.txt", mock.Anything).
		Return(&file.File{ID: 1, Name: "a.txt"}, nil).
		Once()

	body, ct := suite.multipartBody(nil, "a.txt", "x")
	w := suite.upload("?parent_id=12", body, ct, true)

	suite.Equal(http.StatusCreated, w.Code)
}

func (suite *FileHandlerTestSuite) TestUpload_InvalidParentID() {
	body, ct := suite.multipartBody(nil, "a.txt", "x")
	w := suite.upload("?parent_id=abc", body, ct, true)

	suite.Equal(http.StatusBadRequest, w.Code)
	suite.Equal("invalid parent_id", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestUpload_Unauthenticated() {
	body, ct := suite.multipartBody(nil, "notes.txt", "hello")
	w := suite.upload("", body, ct, false)

	suite.Equal(http.StatusUnauthorized, w.Code)
}

func (suite *FileHandlerTestSuite) TestUpload_NotMultipart() {
	w := suite.upload("", strings.NewReader(`{"file":"x"}`), "application/json", true)

	suite.Equal(http.StatusBadRequest, w.Code)
	suite.Equal("expected a multipart form", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestUpload_NoFile() {
	body, ct := suite.multipartBody(map[string]string{"note": "no file here"}, "", "")
	w := suite.upload("", body, ct, true)

	suite.Equal(http.StatusBadRequest, w.Code)
	suite.Equal("no file was sent", suite.errorMessage(w))
}

// A field bigger than the whole body limit must stop at the limit, not be read in full.
func (suite *FileHandlerTestSuite) TestUpload_BodyTooLarge() {
	huge := strings.Repeat("a", 3<<20)
	body, ct := suite.multipartBody(map[string]string{"padding": huge}, "a.txt", "x")
	w := suite.upload("", body, ct, true)

	suite.Equal(http.StatusRequestEntityTooLarge, w.Code)
	suite.Equal("request body is too large", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestUpload_ServiceError() {
	suite.mockService.EXPECT().
		Upload(mock.Anything, testUserID, int64(0), "big.bin", mock.Anything).
		Return(nil, apperr.New(apperr.KindTooLarge, "file is larger than the 1 MB limit")).
		Once()

	body, ct := suite.multipartBody(nil, "big.bin", "x")
	w := suite.upload("", body, ct, true)

	suite.Equal(http.StatusRequestEntityTooLarge, w.Code)
	suite.Equal("file is larger than the 1 MB limit", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestUpload_InternalError() {
	suite.mockService.EXPECT().
		Upload(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, assert.AnError).
		Once()

	body, ct := suite.multipartBody(nil, "a.txt", "x")
	w := suite.upload("", body, ct, true)

	suite.Equal(http.StatusInternalServerError, w.Code)
	suite.Equal("something went wrong", suite.errorMessage(w))
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
		{"?order=asc", file.ListQuery{Sort: file.SortByDate}},
		{"?search=%20Report%25_", file.ListQuery{Search: " Report%_", Sort: file.SortByDate, Desc: true}},
		{"?search=" + atNameLimit, file.ListQuery{Search: atNameLimit, Sort: file.SortByDate, Desc: true}},
	}
	for _, tc := range cases {
		suite.mockService.EXPECT().
			List(mock.Anything, testUserID, tc.want).
			Return(&utils.Page[*file.File]{Data: []*file.File{}}, nil).
			Once()

		suite.Equal(http.StatusOK, suite.list(tc.query, true).Code, tc.query)
	}
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

func (suite *FileHandlerTestSuite) TestGetDownload_Success() {
	suite.mockService.EXPECT().
		GetDownload(mock.Anything, testUserID, int64(9)).
		Return(&file.Download{ID: 9, Name: "Docs.zip", State: file.DownloadFailed, Error: "the zip could not be built"}, nil).
		Once()

	w := suite.getPath("/downloads/9", true)

	suite.Equal(http.StatusOK, w.Code)
	var resp file.Download
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	suite.Equal(file.DownloadFailed, resp.State)
	suite.Equal("the zip could not be built", resp.Error)
}

func (suite *FileHandlerTestSuite) TestGetDownload_InvalidID() {
	w := suite.getPath("/downloads/abc", true)

	suite.Equal(http.StatusBadRequest, w.Code)
	suite.Equal("invalid id", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestGetDownload_Unauthenticated() {
	w := suite.getPath("/downloads/9", false)

	suite.Equal(http.StatusUnauthorized, w.Code)
}

func (suite *FileHandlerTestSuite) TestGetDownload_ServiceError() {
	suite.mockService.EXPECT().
		GetDownload(mock.Anything, testUserID, int64(9)).
		Return(nil, apperr.New(apperr.KindNotFound, "download not found")).
		Once()

	w := suite.getPath("/downloads/9", true)

	suite.Equal(http.StatusNotFound, w.Code)
	suite.Equal("download not found", suite.errorMessage(w))
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
