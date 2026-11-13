package jobs

import (
	"context"
	"database/sql"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverdatabasesql"
	"github.com/riverqueue/river/rivertype"
)

const (
	maxWorkers = 4

	// Running jobs get this long to finish when the app stops. Then River cancels them, and they run again later.
	// Must stay under stopTimeout.
	softStopTimeout = 5 * time.Second
	stopTimeout     = 10 * time.Second
)

// InserterInterface adds a job inside the caller's transaction, so a rollback drops the job too.
// *river.Client[*sql.Tx] has this method.
type InserterInterface interface {
	InsertTx(ctx context.Context, tx *sql.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// NewClient builds a River client on the app's database/sql pool. This driver has no LISTEN/NOTIFY,
// so River polls for new jobs, once a second by default.
func NewClient(db *sql.DB, workers *river.Workers) (*river.Client[*sql.Tx], error) {
	return river.NewClient(riverdatabasesql.New(db), &river.Config{
		Workers:         workers,
		Queues:          map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: maxWorkers}},
		SoftStopTimeout: softStopTimeout,
	})
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
