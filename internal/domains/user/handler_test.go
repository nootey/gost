package user_test

import (
	"encoding/json"
	"gost/internal/apperr"
	"gost/internal/domains/user"
	"gost/internal/middleware"
	"gost/internal/utils"
	"gost/mocks"
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

type UserHandlerTestSuite struct {
	suite.Suite
	router      *chi.Mux
	mockService *mocks.MockUserServiceInterface
	handler     *user.UserHandler
}

func (suite *UserHandlerTestSuite) SetupTest() {
	suite.mockService = mocks.NewMockUserServiceInterface(suite.T())
	suite.handler = user.NewUserHandler(suite.mockService, apperr.NewResponder(zap.NewNop()))

	// Setup router
	suite.router = chi.NewRouter()
	suite.router.Mount("/users", suite.handler.Routes())
	suite.router.Get("/settings", suite.handler.GetSettings)
	suite.router.Put("/settings", suite.handler.SaveSettings)
}

const testUserID int64 = 7

func (suite *UserHandlerTestSuite) settingsRequest(method, body string, authenticated bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authenticated {
		req = req.WithContext(middleware.WithUserID(req.Context(), testUserID))
	}
	w := httptest.NewRecorder()
	suite.router.ServeHTTP(w, req)
	return w
}

func (suite *UserHandlerTestSuite) errorMessage(w *httptest.ResponseRecorder) string {
	var resp apperr.ErrorResponse
	suite.Require().NoError(json.Unmarshal(w.Body.Bytes(), &resp))
	return resp.Message
}

func (suite *UserHandlerTestSuite) TearDownTest() {
	suite.mockService.AssertExpectations(suite.T())
}

func TestUserHandlerTestSuite(t *testing.T) {
	suite.Run(t, new(UserHandlerTestSuite))
}

// Test successful user listing
func (suite *UserHandlerTestSuite) TestListUsers_Success() {
	next := "next"
	mockUsers := &utils.Page[*user.User]{
		Data: []*user.User{
			{
				ID:          1,
				Email:       "test1@example.com",
				DisplayName: "Test User 1",
			},
			{
				ID:          2,
				Email:       "test2@example.com",
				DisplayName: "Test User 2",
			},
		},
		NextCursor: &next,
	}

	suite.mockService.EXPECT().
		ListUsers(mock.Anything, "abc", 10).
		Return(mockUsers, nil).
		Once()

	req := httptest.NewRequest(http.MethodGet, "/users?cursor=abc&page_size=10", nil)
	w := httptest.NewRecorder()

	suite.router.ServeHTTP(w, req)

	suite.Equal(http.StatusOK, w.Code)

	var response utils.Page[*user.User]
	err := json.Unmarshal(w.Body.Bytes(), &response)
	suite.Require().NoError(err)
	suite.Len(response.Data, 2)
	suite.Require().NotNil(response.NextCursor)
	suite.Equal("next", *response.NextCursor)
}

// Test service error handling
func (suite *UserHandlerTestSuite) TestListUsers_ServiceError() {
	suite.mockService.EXPECT().
		ListUsers(mock.Anything, mock.Anything, mock.Anything).
		Return(nil, assert.AnError).
		Once()

	req := httptest.NewRequest(http.MethodGet, "/users", nil)
	w := httptest.NewRecorder()

	suite.router.ServeHTTP(w, req)

	suite.Equal(http.StatusInternalServerError, w.Code)

	var response apperr.ErrorResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	suite.Require().NoError(err)
	suite.Equal("something went wrong", response.Message)
}

// Test default pagination values
func (suite *UserHandlerTestSuite) TestListUsers_DefaultPagination() {
	suite.mockService.EXPECT().
		ListUsers(mock.Anything, "", 0).
		Return(&utils.Page[*user.User]{Data: []*user.User{}}, nil).
		Twice()

	for _, query := range []string{"", "?page=2&page_size=-x"} {
		req := httptest.NewRequest(http.MethodGet, "/users"+query, nil)
		w := httptest.NewRecorder()

		suite.router.ServeHTTP(w, req)

		suite.Equal(http.StatusOK, w.Code, query)
		suite.JSONEq(`{"data":[],"next_cursor":null}`, w.Body.String(), query)
	}
}

func (suite *UserHandlerTestSuite) TestGetSettings_Success() {
	suite.mockService.EXPECT().
		GetSettings(mock.Anything, testUserID).
		Return(&user.Settings{SortFoldersFirst: true, ShowHidden: true}, nil).
		Once()

	w := suite.settingsRequest(http.MethodGet, "", true)

	suite.Equal(http.StatusOK, w.Code)
	suite.JSONEq(`{"sort_favorites_first":false,"sort_folders_first":true,"show_hidden":true,"show_extensions":false}`,
		w.Body.String())
}

func (suite *UserHandlerTestSuite) TestGetSettings_ServiceError() {
	suite.mockService.EXPECT().
		GetSettings(mock.Anything, testUserID).
		Return(nil, assert.AnError).
		Once()

	w := suite.settingsRequest(http.MethodGet, "", true)

	suite.Equal(http.StatusInternalServerError, w.Code)
}

func (suite *UserHandlerTestSuite) TestSaveSettings_Success() {
	saved := user.Settings{SortFavoritesFirst: true, SortFoldersFirst: false, ShowHidden: true, ShowExtensions: false}
	suite.mockService.EXPECT().
		SaveSettings(mock.Anything, testUserID, saved).
		Return(&saved, nil).
		Once()

	w := suite.settingsRequest(http.MethodPut,
		`{"sort_favorites_first":true,"sort_folders_first":false,"show_hidden":true,"show_extensions":false}`, true)

	suite.Equal(http.StatusOK, w.Code)
	suite.JSONEq(`{"sort_favorites_first":true,"sort_folders_first":false,"show_hidden":true,"show_extensions":false}`,
		w.Body.String())
}

// Every setting needs a value, so a missing one never turns into false.
func (suite *UserHandlerTestSuite) TestSaveSettings_InvalidBody() {
	for _, body := range []string{
		`not json`,
		`{}`,
		`{"sort_favorites_first":true,"sort_folders_first":true,"show_hidden":true}`,
		`{"sort_favorites_first":true,"sort_folders_first":true,"show_hidden":true,"show_extensions":null}`,
		`{"sort_favorites_first":true,"sort_folders_first":"yes","show_hidden":true,"show_extensions":true}`,
	} {
		w := suite.settingsRequest(http.MethodPut, body, true)

		suite.Equal(http.StatusBadRequest, w.Code, body)
		suite.Equal("invalid settings", suite.errorMessage(w), body)
	}
}

func (suite *UserHandlerTestSuite) TestSaveSettings_ServiceError() {
	suite.mockService.EXPECT().
		SaveSettings(mock.Anything, testUserID, mock.Anything).
		Return(nil, assert.AnError).
		Once()

	w := suite.settingsRequest(http.MethodPut,
		`{"sort_favorites_first":false,"sort_folders_first":true,"show_hidden":false,"show_extensions":true}`, true)

	suite.Equal(http.StatusInternalServerError, w.Code)
}

func (suite *UserHandlerTestSuite) TestSettings_Unauthenticated() {
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		w := suite.settingsRequest(method,
			`{"sort_favorites_first":false,"sort_folders_first":true,"show_hidden":false,"show_extensions":true}`, false)

		suite.Equal(http.StatusUnauthorized, w.Code, method)
	}
}
