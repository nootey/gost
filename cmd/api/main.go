package main

import (
	"context"
	"database/sql"
	"gost/config"
	"gost/internal/apperr"
	"gost/internal/database"
	"gost/internal/domains/auth"
	"gost/internal/domains/file"
	"gost/internal/domains/user"
	"gost/internal/health"
	"gost/internal/jobs"
	"gost/internal/mailer"
	"gost/internal/sessions"
	"gost/internal/store"
	"gost/internal/tokens"
	"gost/internal/worker"
	"gost/pkg/logging"
	"gost/pkg/telemetry"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	mWare "gost/internal/middleware"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	_ "github.com/joho/godotenv/autoload"
	"github.com/redis/go-redis/v9"
	"github.com/riverqueue/river"
	"go.uber.org/zap"
)

type Handlers struct {
	Auth   *auth.AuthHandler
	User   *user.UserHandler
	File   *file.FileHandler
	Health *health.HealthHandler
}

func main() {
	if err := config.Load(); err != nil {
		panic("failed to load config: " + err.Error())
	}
	cfg := config.Get()

	logger := logging.InitLogger(cfg.App.Environment == "production", cfg.App.LogLevel)
	defer func() {
		_ = logger.Sync()
	}()
	logger = logger.Named("api")

	logger.Info("starting application",
		zap.String("environment", cfg.App.Environment),
		zap.String("port", cfg.Server.Port),
	)

	db, err := store.NewDB(cfg.Database)
	if err != nil {
		logger.Fatal("failed to connect to database", zap.Error(err))
	}
	defer func(db *sql.DB) {
		err := db.Close()
		if err != nil {
			logger.Fatal("failed to close database connection", zap.Error(err))
		}
	}(db)
	logger.Info("database connected")

	rdb, err := store.NewRedis(cfg.Redis)
	if err != nil {
		logger.Fatal("failed to connect to redis", zap.Error(err))
	}
	defer func(rdb *redis.Client) {
		err := rdb.Close()
		if err != nil {
			logger.Error("failed to close redis connection", zap.Error(err))
		}
	}(rdb)
	logger.Info("redis connected")

	// Create SQLC queries instance
	queries := database.New(db)

	// Initialize auth middleware
	sessionStore := sessions.NewStore(rdb, cfg.Session)
	authMiddleware := mWare.NewAuthMiddleware(sessionStore, cfg, logger)

	// Initialize infrastructure
	tokenStore := tokens.NewStore(rdb, cfg.Token)
	mail := mailer.NewMailer(cfg.Mailer, logger)

	if err := os.MkdirAll(cfg.Upload.Dir, 0o750); err != nil {
		logger.Fatal("failed to create upload directory", zap.Error(err))
	}

	tel, err := telemetry.New()
	if err != nil {
		logger.Fatal("failed to initialize telemetry", zap.Error(err))
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tel.Shutdown(ctx); err != nil {
			logger.Error("failed to shutdown telemetry", zap.Error(err))
		}
	}()

	healthSvc, err := health.New(logger.Named("health"))
	if err != nil {
		logger.Fatal("failed to initialize health checks", zap.Error(err))
	}
	healthSvc.Add(health.NewDBChecker(db))
	healthSvc.Add(health.NewRedisChecker(rdb))

	// The workers go in once the services they call exist. River reads the bundle on insert and on Start.
	jobWorkers := river.NewWorkers()
	jobClient, err := jobs.NewClient(db, jobWorkers, logger.Named("jobs"))
	if err != nil {
		logger.Fatal("failed to create job client", zap.Error(err))
	}

	// Initialize services
	userService := user.NewUserService(queries, db)
	authService := auth.NewAuthService(queries, tokenStore, mail, cfg)
	fileService := file.NewFileService(queries, db, jobClient, cfg.Upload, logger.Named("files"))
	// Add more services as needed

	if err := jobs.RegisterWorkers(jobWorkers, fileService); err != nil {
		logger.Fatal("failed to register job workers", zap.Error(err))
	}

	// Initialize handlers
	responder := apperr.NewResponder(logger)
	userHandler := user.NewUserHandler(userService, responder)
	authHandler := auth.NewAuthHandler(authService, authMiddleware, responder, logger)
	fileHandler := file.NewFileHandler(fileService, cfg.Upload, responder)
	healthHandler := health.NewHealthHandler(healthSvc, responder)
	// Add more handlers as needed

	h := Handlers{
		Auth:   authHandler,
		User:   userHandler,
		File:   fileHandler,
		Health: healthHandler,
	}

	r := setupRouter(cfg, &h, authMiddleware, tel, logger)

	srv := &http.Server{
		Addr:         cfg.Server.Host + ":" + cfg.Server.Port,
		Handler:      r,
		ReadTimeout:  time.Duration(cfg.Server.ReadTimeout) * time.Second,
		WriteTimeout: time.Duration(cfg.Server.WriteTimeout) * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	supervisor := worker.NewSupervisor(logger)
	supervisor.Register(&httpService{srv: srv, logger: logger, shutdownTimeout: 10 * time.Second})
	supervisor.Register(healthSvc)
	supervisor.Register(jobs.NewService(jobClient))

	if err := supervisor.Run(ctx); err != nil {
		logger.Fatal("supervisor exited with error", zap.Error(err))
	}
	logger.Info("shutdown complete")
}

func setupRouter(cfg *config.Config, h *Handlers, authMiddleware *mWare.AuthMiddleware, tel *telemetry.Telemetry, logger *zap.Logger) *chi.Mux {
	r := chi.NewRouter()

	// Middleware
	r.Use(tel.HTTPMiddleware())
	r.Use(middleware.RequestID)
	r.Use(middleware.ClientIPFromRemoteAddr)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// CORS
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   cfg.Cors.AllowedOrigins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	// Timeout is set per group, because uploads need a longer one than the rest.
	requestTimeout := middleware.Timeout(60 * time.Second)
	uploadTimeout := middleware.Timeout(time.Duration(cfg.Upload.TimeoutSeconds) * time.Second)

	r.Group(func(r chi.Router) {
		r.Use(requestTimeout)

		r.Get("/health", h.Health.Report)

		r.Handle("/metrics", tel.MetricsHandler)
	})

	// API routes
	r.Route("/api", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(requestTimeout)

			// Public routes
			r.Mount("/auth", h.Auth.Routes())

			// Protected routes
			r.Group(func(r chi.Router) {
				r.Use(authMiddleware.WebClientAuthentication)

				r.Mount("/users", h.User.Routes())
				r.Get("/files", h.File.List)
				r.Patch("/files/{id}", h.File.Rename)
				r.Post("/files/move", h.File.Move)
				r.Post("/files/copy", h.File.Copy)
				r.Post("/folders", h.File.CreateFolder)
				r.Get("/folders/resolve", h.File.ResolveFolder)
				r.Get("/trash", h.File.ListTrash)
				r.Post("/trash", h.File.Trash)
				r.Post("/trash/restore", h.File.Restore)
				r.Post("/trash/purge", h.File.Purge)
				// Mount more protected handlers as needed
			})
		})

		// Protected upload routes
		r.Group(func(r chi.Router) {
			r.Use(uploadTimeout)
			r.Use(authMiddleware.WebClientAuthentication)

			r.Post("/files", h.File.Upload)
		})
	})

	logger.Info("router configured")
	return r
}
