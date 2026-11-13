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

type subtreeTrasher interface {
	TrashSubtrees(ctx context.Context, userID int64, rootIDs []int64) error
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

func RegisterWorkers(workers *river.Workers, files subtreeTrasher) error {
	return river.AddWorkerSafely(workers, NewTrashSubtreesWorker(files))
}
