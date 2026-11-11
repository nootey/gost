package file_test

import (
	"bytes"
	"context"
	"encoding/json"
	"gost/config"
	"gost/internal/apperr"
	"gost/internal/domains/file"
	"gost/internal/middleware"
	"gost/mocks"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
)

const testUserID int64 = 7

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

func (suite *FileHandlerTestSuite) upload(body io.Reader, contentType string, authenticated bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/files", body)
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
		Upload(mock.Anything, testUserID, "notes.txt", mock.Anything).
		RunAndReturn(func(_ context.Context, _ int64, name string, src io.Reader) (*file.File, error) {
			content, err := io.ReadAll(src)
			suite.Require().NoError(err)
			suite.Equal("hello", string(content))
			return &file.File{ID: 1, Name: name, ContentType: "text/plain; charset=utf-8", SizeBytes: 5}, nil
		}).
		Once()

	body, ct := suite.multipartBody(nil, "notes.txt", "hello")
	w := suite.upload(body, ct, true)

	suite.Equal(http.StatusCreated, w.Code)
	var resp file.UploadResponse
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	suite.Equal("notes.txt uploaded.", resp.Message)
	suite.Require().NotNil(resp.File)
	suite.Equal(int64(5), resp.File.SizeBytes)
}

func (suite *FileHandlerTestSuite) TestUpload_SkipsFieldsBeforeFile() {
	suite.mockService.EXPECT().
		Upload(mock.Anything, testUserID, "a.txt", mock.Anything).
		Return(&file.File{ID: 1, Name: "a.txt"}, nil).
		Once()

	body, ct := suite.multipartBody(map[string]string{"note": "ignored"}, "a.txt", "x")
	w := suite.upload(body, ct, true)

	suite.Equal(http.StatusCreated, w.Code)
}

func (suite *FileHandlerTestSuite) TestUpload_Unauthenticated() {
	body, ct := suite.multipartBody(nil, "notes.txt", "hello")
	w := suite.upload(body, ct, false)

	suite.Equal(http.StatusUnauthorized, w.Code)
}

func (suite *FileHandlerTestSuite) TestUpload_NotMultipart() {
	w := suite.upload(strings.NewReader(`{"file":"x"}`), "application/json", true)

	suite.Equal(http.StatusBadRequest, w.Code)
	suite.Equal("expected a multipart form", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestUpload_NoFile() {
	body, ct := suite.multipartBody(map[string]string{"note": "no file here"}, "", "")
	w := suite.upload(body, ct, true)

	suite.Equal(http.StatusBadRequest, w.Code)
	suite.Equal("no file was sent", suite.errorMessage(w))
}

// A field bigger than the whole body limit must stop at the limit, not be read in full.
func (suite *FileHandlerTestSuite) TestUpload_BodyTooLarge() {
	huge := strings.Repeat("a", 3<<20)
	body, ct := suite.multipartBody(map[string]string{"padding": huge}, "a.txt", "x")
	w := suite.upload(body, ct, true)

	suite.Equal(http.StatusRequestEntityTooLarge, w.Code)
	suite.Equal("request body is too large", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestUpload_ServiceError() {
	suite.mockService.EXPECT().
		Upload(mock.Anything, testUserID, "big.bin", mock.Anything).
		Return(nil, apperr.New(apperr.KindTooLarge, "file is larger than the 1 MB limit")).
		Once()

	body, ct := suite.multipartBody(nil, "big.bin", "x")
	w := suite.upload(body, ct, true)

	suite.Equal(http.StatusRequestEntityTooLarge, w.Code)
	suite.Equal("file is larger than the 1 MB limit", suite.errorMessage(w))
}

func (suite *FileHandlerTestSuite) TestUpload_InternalError() {
	suite.mockService.EXPECT().
		Upload(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, assert.AnError).
		Once()

	body, ct := suite.multipartBody(nil, "a.txt", "x")
	w := suite.upload(body, ct, true)

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
	suite.mockService.EXPECT().
		List(mock.Anything, testUserID, 2, 5).
		Return(&file.PaginatedFiles{
			Data:       []*file.File{{ID: 3, Name: "c.txt"}, {ID: 2, Name: "b.txt"}},
			Page:       2,
			PageSize:   5,
			Total:      7,
			TotalPages: 2,
		}, nil).
		Once()

	w := suite.list("?page=2&page_size=5", true)

	suite.Equal(http.StatusOK, w.Code)
	var resp file.PaginatedFiles
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	suite.Require().Len(resp.Data, 2)
	suite.Equal("c.txt", resp.Data[0].Name)
	suite.Equal(2, resp.Page)
	suite.Equal(7, resp.Total)
}

// Missing or bad values reach the service as zero, so the service picks the defaults.
func (suite *FileHandlerTestSuite) TestList_DefaultPagination() {
	suite.mockService.EXPECT().
		List(mock.Anything, testUserID, 0, 0).
		Return(&file.PaginatedFiles{Data: []*file.File{}, Page: 1, PageSize: 20}, nil).
		Twice()

	suite.Equal(http.StatusOK, suite.list("", true).Code)
	suite.Equal(http.StatusOK, suite.list("?page=abc&page_size=-x", true).Code)
}

func (suite *FileHandlerTestSuite) TestList_Unauthenticated() {
	w := suite.list("", false)

	suite.Equal(http.StatusUnauthorized, w.Code)
}

func (suite *FileHandlerTestSuite) TestList_ServiceError() {
	suite.mockService.EXPECT().
		List(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, assert.AnError).
		Once()

	w := suite.list("", true)

	suite.Equal(http.StatusInternalServerError, w.Code)
	suite.Equal("something went wrong", suite.errorMessage(w))
}
