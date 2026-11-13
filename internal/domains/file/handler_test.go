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
	suite.handler = file.NewFileHandler(suite.mockService, cfg, apperr.NewResponder(zap.NewNop()))

	suite.router = chi.NewRouter()
	suite.router.Get("/files", suite.handler.List)
	suite.router.Post("/files", suite.handler.Upload)
	suite.router.Post("/folders", suite.handler.CreateFolder)
	suite.router.Get("/folders/resolve", suite.handler.ResolveFolder)
	suite.router.Patch("/files/{id}", suite.handler.Rename)
	suite.router.Get("/trash", suite.handler.ListTrash)
	suite.router.Post("/trash", suite.handler.Trash)
	suite.router.Post("/trash/restore", suite.handler.Restore)
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
