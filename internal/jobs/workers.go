package jobs

import (
	"context"

	"github.com/riverqueue/river"
)

// TrashSubtreesArgs names the folders a trash request marked. The job marks the rows under them.
type TrashSubtreesArgs struct {
	UserID  int64   `json:"user_id"`
	RootIDs []int64 `json:"root_ids"`
}

func (TrashSubtreesArgs) Kind() string { return "trash_subtrees" }

// PurgeGroupsArgs names the trash roots a purge request marked. The job deletes their groups for good.
type PurgeGroupsArgs struct {
	UserID  int64   `json:"user_id"`
	RootIDs []int64 `json:"root_ids"`
}

func (PurgeGroupsArgs) Kind() string { return "purge_groups" }

// InsertOpts makes River skip a job while one with the same args waits or runs. The trash sweep adds a purge job
// again for groups marked long ago, and the first job can still be alive.
func (PurgeGroupsArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

// SweepTrashArgs starts the trash sweep. It runs on a schedule, see NewClient.
type SweepTrashArgs struct{}

func (SweepTrashArgs) Kind() string { return "sweep_trash" }

// InsertOpts runs a sweep once. The next run on the schedule is the retry.
func (SweepTrashArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 1}
}

// SweepOrphansArgs starts the orphan sweep. It runs on a schedule, see NewClient.
type SweepOrphansArgs struct{}

func (SweepOrphansArgs) Kind() string { return "sweep_orphans" }

// InsertOpts runs a sweep once. The next run on the schedule is the retry.
func (SweepOrphansArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 1}
}

type subtreeTrasher interface {
	TrashSubtrees(ctx context.Context, userID int64, rootIDs []int64) error
}

type groupPurger interface {
	PurgeGroups(ctx context.Context, userID int64, rootIDs []int64) error
}

type trashSweeper interface {
	SweepTrash(ctx context.Context) error
}

type orphanSweeper interface {
	SweepOrphans(ctx context.Context) error
}

// fileJobs is what the file workers need from the file service.
type fileJobs interface {
	subtreeTrasher
	groupPurger
	trashSweeper
	orphanSweeper
}

type TrashSubtreesWorker struct {
	river.WorkerDefaults[TrashSubtreesArgs]
	files subtreeTrasher
}

func NewTrashSubtreesWorker(files subtreeTrasher) *TrashSubtreesWorker {
	return &TrashSubtreesWorker{files: files}
}

func (w *TrashSubtreesWorker) Work(ctx context.Context, job *river.Job[TrashSubtreesArgs]) error {
	return w.files.TrashSubtrees(ctx, job.Args.UserID, job.Args.RootIDs)
}

type PurgeGroupsWorker struct {
	river.WorkerDefaults[PurgeGroupsArgs]
	files groupPurger
}

func NewPurgeGroupsWorker(files groupPurger) *PurgeGroupsWorker {
	return &PurgeGroupsWorker{files: files}
}

func (w *PurgeGroupsWorker) Work(ctx context.Context, job *river.Job[PurgeGroupsArgs]) error {
	return w.files.PurgeGroups(ctx, job.Args.UserID, job.Args.RootIDs)
}

type SweepTrashWorker struct {
	river.WorkerDefaults[SweepTrashArgs]
	files trashSweeper
}

func NewSweepTrashWorker(files trashSweeper) *SweepTrashWorker {
	return &SweepTrashWorker{files: files}
}

func (w *SweepTrashWorker) Work(ctx context.Context, _ *river.Job[SweepTrashArgs]) error {
	return w.files.SweepTrash(ctx)
}

type SweepOrphansWorker struct {
	river.WorkerDefaults[SweepOrphansArgs]
	files orphanSweeper
}

func NewSweepOrphansWorker(files orphanSweeper) *SweepOrphansWorker {
	return &SweepOrphansWorker{files: files}
}

func (w *SweepOrphansWorker) Work(ctx context.Context, _ *river.Job[SweepOrphansArgs]) error {
	return w.files.SweepOrphans(ctx)
}

func RegisterWorkers(workers *river.Workers, files fileJobs) error {
	if err := river.AddWorkerSafely(workers, NewTrashSubtreesWorker(files)); err != nil {
		return err
	}
	if err := river.AddWorkerSafely(workers, NewPurgeGroupsWorker(files)); err != nil {
		return err
	}
	if err := river.AddWorkerSafely(workers, NewSweepTrashWorker(files)); err != nil {
		return err
	}
	return river.AddWorkerSafely(workers, NewSweepOrphansWorker(files))
}
