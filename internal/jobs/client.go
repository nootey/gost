package jobs

import (
	"context"
	"database/sql"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverdatabasesql"
	"github.com/riverqueue/river/rivertype"
	"go.uber.org/zap"
)

const (
	maxWorkers = 4

	// Running jobs get this long to finish when the app stops. Then River cancels them, and they run again later.
	// Must stay under stopTimeout.
	softStopTimeout = 5 * time.Second
	stopTimeout     = 10 * time.Second

	sweepTrashInterval   = time.Hour
	sweepOrphansInterval = 24 * time.Hour
)

// InserterInterface adds a job inside the caller's transaction, so a rollback drops the job too.
// *river.Client[*sql.Tx] has this method.
type InserterInterface interface {
	InsertTx(ctx context.Context, tx *sql.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// NewClient builds a River client on the app's database/sql pool. This driver has no LISTEN/NOTIFY,
// so River polls for new jobs, once a second by default. The trash and orphan sweeps run at start, then on their intervals.
func NewClient(db *sql.DB, workers *river.Workers, logger *zap.Logger) (*river.Client[*sql.Tx], error) {
	return river.NewClient(riverdatabasesql.New(db), &river.Config{
		Workers:         workers,
		Queues:          map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: maxWorkers}},
		SoftStopTimeout: softStopTimeout,
		Middleware:      []rivertype.Middleware{logRuns(logger)},
		PeriodicJobs: []*river.PeriodicJob{
			river.NewPeriodicJob(river.PeriodicInterval(sweepTrashInterval), func() (river.JobArgs, *river.InsertOpts) {
				return SweepTrashArgs{}, nil
			}, &river.PeriodicJobOpts{RunOnStart: true}),
			river.NewPeriodicJob(river.PeriodicInterval(sweepOrphansInterval), func() (river.JobArgs, *river.InsertOpts) {
				return SweepOrphansArgs{}, nil
			}, &river.PeriodicJobOpts{RunOnStart: true}),
		},
	})
}

// logRuns logs the start and the end of every job run, so a new job kind gets these logs with no extra code.
func logRuns(logger *zap.Logger) river.WorkerMiddlewareFunc {
	return func(ctx context.Context, job *rivertype.JobRow, doInner func(ctx context.Context) error) error {
		fields := []zap.Field{zap.String("kind", job.Kind), zap.Int64("job_id", job.ID), zap.Int("attempt", job.Attempt)}
		logger.Info("job started", fields...)

		start := time.Now()
		err := doInner(ctx)
		fields = append(fields, zap.Duration("took", time.Since(start)))
		if err != nil {
			logger.Error("job failed", append(fields, zap.Error(err))...)
			return err
		}
		logger.Info("job finished", fields...)
		return nil
	}
}

// Service runs the River client on the worker supervisor.
type Service struct {
	client *river.Client[*sql.Tx]
}

func NewService(client *river.Client[*sql.Tx]) *Service {
	return &Service{client: client}
}

func (s *Service) Name() string { return "jobs" }

func (s *Service) Run(ctx context.Context) error {
	if err := s.client.Start(ctx); err != nil {
		return err
	}

	<-ctx.Done()

	// The run context is already cancelled, so the stop needs its own deadline.
	stopCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	return s.client.Stop(stopCtx)
}
