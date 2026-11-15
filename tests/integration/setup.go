package integration

import (
	"context"
	"database/sql"
	"gost/config"
	"gost/internal/database"
	"gost/internal/domains/auth"
	"gost/internal/domains/file"
	"gost/internal/domains/user"
	"gost/internal/jobs"
	"gost/internal/store/seeders"
	"gost/internal/tokens"
	"path/filepath"

	"github.com/alicebob/miniredis/v2"
	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"
	"github.com/redis/go-redis/v9"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/suite"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type TestContainer struct {
	Container *postgres.PostgresContainer
	DB        *sql.DB
	Queries   *database.Queries
	Redis     *miniredis.Miniredis
	Mailer    *CaptureMailer
	UploadDir string
	Logs      *observer.ObservedLogs
	Services  *Services
}

type Services struct {
	UserService *user.UserService
	AuthService *auth.AuthService
	FileService *file.FileService
	// Add more services here
}

// CaptureMailer records the last link sent, so tests can read the confirm/reset
// token that would otherwise only reach the user by email.
type CaptureMailer struct {
	ConfirmationLink string
	ResetLink        string
}

func (m *CaptureMailer) SendConfirmationEmail(to, name, link string) error {
	m.ConfirmationLink = link
	return nil
}

func (m *CaptureMailer) SendPasswordResetEmail(to, name, link string) error {
	m.ResetLink = link
	return nil
}

type ServiceIntegrationSuite struct {
	suite.Suite
	TC  *TestContainer
	Ctx context.Context
}

func (s *ServiceIntegrationSuite) SetupSuite() {
	s.Ctx = context.Background()

	// Start PostgreSQL container
	container, err := postgres.Run(s.Ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("testdb"),
		postgres.WithUsername("testuser"),
		postgres.WithPassword("testpass"),
		// The ready log can come before the port is mapped, so wait for both.
		postgres.BasicWaitStrategies(),
	)
	s.Require().NoError(err, "failed to start container")

	// Connect to container
	connStr, err := container.ConnectionString(s.Ctx, "sslmode=disable")
	s.Require().NoError(err, "failed to get connection string")

	db, err := sql.Open("postgres", connStr)
	s.Require().NoError(err, "failed to connect to database")

	// Run migrations
	err = goose.SetDialect("postgres")
	s.Require().NoError(err, "failed to set goose dialect")

	migrationsPath := filepath.Join("..", "..", "..", "migrations")
	err = goose.Up(db, migrationsPath)
	s.Require().NoError(err, "migrations failed")

	// Create queries
	queries := database.New(db)

	// Tests read the logs of the services through Logs.
	logCore, logs := observer.New(zap.InfoLevel)
	logger := zap.New(logCore)

	// Seed basic data (roles, permissions)
	cfg := testConfig()
	cfg.Upload.Dir = s.T().TempDir()
	err = seeders.SeedDatabase(s.Ctx, db, logger, cfg, "core")
	s.Require().NoError(err, "seeding failed")

	// In-memory redis backs the token store.
	mr := miniredis.RunT(s.T())
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	tokenStore := tokens.NewStore(rdb, cfg.Token)
	captureMailer := &CaptureMailer{}

	// The client only adds jobs. Tests run them by hand, so they never race a worker.
	jobWorkers := river.NewWorkers()
	jobClient, err := jobs.NewClient(db, jobWorkers, logger)
	s.Require().NoError(err, "failed to create job client")

	// Initialize services
	userService := user.NewUserService(queries, db)
	authService := auth.NewAuthService(queries, tokenStore, captureMailer, cfg)
	fileService := file.NewFileService(queries, db, jobClient, cfg.Upload, cfg.Download, logger)
	s.Require().NoError(jobs.RegisterWorkers(jobWorkers, fileService), "failed to register job workers")

	s.TC = &TestContainer{
		Container: container,
		DB:        db,
		Queries:   queries,
		Redis:     mr,
		Mailer:    captureMailer,
		UploadDir: cfg.Upload.Dir,
		Logs:      logs,
		Services: &Services{
			UserService: userService,
			AuthService: authService,
			FileService: fileService,
		},
	}
}

// testConfig returns the minimal config the wired services need.
func testConfig() *config.Config {
	cfg := &config.Config{}
	cfg.App.Environment = "local"
	cfg.Server.Host = "localhost"
	cfg.Server.Port = "8080"
	cfg.WebClient.Domain = "localhost"
	cfg.WebClient.Port = "5173"
	cfg.Token.ConfirmTTLHours = 24
	cfg.Token.ResetTTLMinutes = 60
	cfg.Upload.MaxSizeMB = 1
	cfg.Upload.TimeoutSeconds = 60
	cfg.Download.TimeoutSeconds = 60
	cfg.Download.MaxZipMB = 1
	cfg.Download.ZipCheckSeconds = 0
	return cfg
}

func (s *ServiceIntegrationSuite) SetupTest() {
	// Truncate users and jobs between tests (keep roles)
	_, err := s.TC.DB.ExecContext(s.Ctx, `TRUNCATE TABLE users, river_job RESTART IDENTITY CASCADE`)
	s.Require().NoError(err, "failed to truncate users and jobs")

	// Clear tokens/sessions and the captured mail between tests.
	s.TC.Redis.FlushAll()
	s.TC.Mailer.ConfirmationLink = ""
	s.TC.Mailer.ResetLink = ""
	s.TC.Logs.TakeAll()
}

func (s *ServiceIntegrationSuite) TearDownSuite() {
	if s.TC.DB != nil {
		_ = s.TC.DB.Close()
	}

	if s.TC.Container != nil {
		if err := s.TC.Container.Terminate(s.Ctx); err != nil {
			s.T().Logf("container cleanup warning: %s", err)
		}
	}
}
