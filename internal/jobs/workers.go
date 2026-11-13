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

type subtreeTrasher interface {
	TrashSubtrees(ctx context.Context, userID int64, rootIDs []int64) error
}

type groupPurger interface {
	PurgeGroups(ctx context.Context, userID int64, rootIDs []int64) error
}

// fileJobs is what the file workers need from the file service.
type fileJobs interface {
	subtreeTrasher
	groupPurger
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

func RegisterWorkers(workers *river.Workers, files fileJobs) error {
	if err := river.AddWorkerSafely(workers, NewTrashSubtreesWorker(files)); err != nil {
		return err
	}
	return river.AddWorkerSafely(workers, NewPurgeGroupsWorker(files))
}
