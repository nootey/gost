package jobs

import (
	"context"
	"errors"

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

// CopyItemsArgs names the copy roots a paste made. The job copies their bytes and the rows under them.
type CopyItemsArgs struct {
	UserID  int64   `json:"user_id"`
	RootIDs []int64 `json:"root_ids"`
}

func (CopyItemsArgs) Kind() string { return "copy_items" }

// InsertOpts makes River skip a job while one with the same args waits or runs. The copy sweep adds a copy job
// again for roots marked long ago, and the first job can still be alive.
func (CopyItemsArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

// BuildZipArgs names the download a zip request made. The job builds its zip.
type BuildZipArgs struct {
	UserID     int64 `json:"user_id"`
	DownloadID int64 `json:"download_id"`
}

func (BuildZipArgs) Kind() string { return "build_zip" }

// InsertOpts puts zips on their own queue. A zip that fails three times will not work on a fourth, and the client
// waits on it, so it gives up sooner than the other jobs.
func (BuildZipArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueDownloads, MaxAttempts: 3}
}

// SweepTrashArgs starts the trash sweep. It runs on a schedule, see NewClient.
type SweepTrashArgs struct{}

func (SweepTrashArgs) Kind() string { return "sweep_trash" }

// InsertOpts runs a sweep once. The next run on the schedule is the retry.
func (SweepTrashArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 1}
}

// SweepCopiesArgs starts the copy sweep. It runs on a schedule, see NewClient.
type SweepCopiesArgs struct{}

func (SweepCopiesArgs) Kind() string { return "sweep_copies" }

// InsertOpts runs a sweep once. The next run on the schedule is the retry.
func (SweepCopiesArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 1}
}

// SweepDownloadsArgs starts the download sweep. It runs on a schedule, see NewClient.
type SweepDownloadsArgs struct{}

func (SweepDownloadsArgs) Kind() string { return "sweep_downloads" }

// InsertOpts runs a sweep once. The next run on the schedule is the retry.
func (SweepDownloadsArgs) InsertOpts() river.InsertOpts {
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

type itemCopier interface {
	CopyItems(ctx context.Context, userID int64, rootIDs []int64) error
}

type zipBuilder interface {
	BuildZip(ctx context.Context, userID, downloadID int64) error
	FailZip(ctx context.Context, userID, downloadID int64) error
}

// SweepUploadsArgs starts the upload sweep. It runs on a schedule, see NewClient.
type SweepUploadsArgs struct{}

func (SweepUploadsArgs) Kind() string { return "sweep_uploads" }

// InsertOpts runs a sweep once. The next run on the schedule is the retry.
func (SweepUploadsArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 1}
}

type trashSweeper interface {
	SweepTrash(ctx context.Context) error
}

type copySweeper interface {
	SweepCopies(ctx context.Context) error
}

type downloadSweeper interface {
	SweepDownloads(ctx context.Context) error
}

type orphanSweeper interface {
	SweepOrphans(ctx context.Context) error
}

type uploadSweeper interface {
	SweepUploads(ctx context.Context) error
}

// fileJobs is what the file workers need from the file service.
type fileJobs interface {
	subtreeTrasher
	groupPurger
	itemCopier
	zipBuilder
	trashSweeper
	copySweeper
	downloadSweeper
	orphanSweeper
	uploadSweeper
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

type CopyItemsWorker struct {
	river.WorkerDefaults[CopyItemsArgs]
	files itemCopier
}

func NewCopyItemsWorker(files itemCopier) *CopyItemsWorker {
	return &CopyItemsWorker{files: files}
}

func (w *CopyItemsWorker) Work(ctx context.Context, job *river.Job[CopyItemsArgs]) error {
	return w.files.CopyItems(ctx, job.Args.UserID, job.Args.RootIDs)
}

type BuildZipWorker struct {
	river.WorkerDefaults[BuildZipArgs]
	files zipBuilder
}

func NewBuildZipWorker(files zipBuilder) *BuildZipWorker {
	return &BuildZipWorker{files: files}
}

// Work marks the download failed after the last attempt, so the client stops waiting for it. The run context can be
// over by then, so that write does not use its deadline.
func (w *BuildZipWorker) Work(ctx context.Context, job *river.Job[BuildZipArgs]) error {
	err := w.files.BuildZip(ctx, job.Args.UserID, job.Args.DownloadID)
	if err != nil && job.Attempt >= job.MaxAttempts {
		return errors.Join(err, w.files.FailZip(context.WithoutCancel(ctx), job.Args.UserID, job.Args.DownloadID))
	}
	return err
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

type SweepCopiesWorker struct {
	river.WorkerDefaults[SweepCopiesArgs]
	files copySweeper
}

func NewSweepCopiesWorker(files copySweeper) *SweepCopiesWorker {
	return &SweepCopiesWorker{files: files}
}

func (w *SweepCopiesWorker) Work(ctx context.Context, _ *river.Job[SweepCopiesArgs]) error {
	return w.files.SweepCopies(ctx)
}

type SweepDownloadsWorker struct {
	river.WorkerDefaults[SweepDownloadsArgs]
	files downloadSweeper
}

func NewSweepDownloadsWorker(files downloadSweeper) *SweepDownloadsWorker {
	return &SweepDownloadsWorker{files: files}
}

func (w *SweepDownloadsWorker) Work(ctx context.Context, _ *river.Job[SweepDownloadsArgs]) error {
	return w.files.SweepDownloads(ctx)
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

type SweepUploadsWorker struct {
	river.WorkerDefaults[SweepUploadsArgs]
	files uploadSweeper
}

func NewSweepUploadsWorker(files uploadSweeper) *SweepUploadsWorker {
	return &SweepUploadsWorker{files: files}
}

func (w *SweepUploadsWorker) Work(ctx context.Context, _ *river.Job[SweepUploadsArgs]) error {
	return w.files.SweepUploads(ctx)
}

func RegisterWorkers(workers *river.Workers, files fileJobs) error {
	if err := river.AddWorkerSafely(workers, NewTrashSubtreesWorker(files)); err != nil {
		return err
	}
	if err := river.AddWorkerSafely(workers, NewPurgeGroupsWorker(files)); err != nil {
		return err
	}
	if err := river.AddWorkerSafely(workers, NewCopyItemsWorker(files)); err != nil {
		return err
	}
	if err := river.AddWorkerSafely(workers, NewBuildZipWorker(files)); err != nil {
		return err
	}
	if err := river.AddWorkerSafely(workers, NewSweepTrashWorker(files)); err != nil {
		return err
	}
	if err := river.AddWorkerSafely(workers, NewSweepCopiesWorker(files)); err != nil {
		return err
	}
	if err := river.AddWorkerSafely(workers, NewSweepDownloadsWorker(files)); err != nil {
		return err
	}
	if err := river.AddWorkerSafely(workers, NewSweepOrphansWorker(files)); err != nil {
		return err
	}
	return river.AddWorkerSafely(workers, NewSweepUploadsWorker(files))
}
