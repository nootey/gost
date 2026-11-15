package file

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"gost/config"
	"gost/internal/apperr"
	"gost/internal/database"
	"gost/internal/jobs"
	"gost/internal/utils"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/riverqueue/river/rivertype"
	"go.uber.org/zap"
)

const nameIndex = "idx_files_user_parent_name"

const maxNameAttempts = 3

// trashBatchSize caps the rows one transaction of the trash job marks, so the job never holds the tree lock long.
const trashBatchSize = 1000

// purgeBatchSize caps the rows one transaction of the purge job changes. It is smaller than trashBatchSize, because
// Postgres checks the parent_id foreign key for each deleted row: about 10 ms for 250 rows, 37 ms for 1000.
const purgeBatchSize = 250

// trashRetentionDays is how long an item stays in the trash before the sweep deletes it for good.
const trashRetentionDays = 30

// purgeRetryHours is how long a marked group waits for its purge job before the sweep adds the job again.
const purgeRetryHours = 24

// orphanGraceHours is how long a file with no row stays before the orphan sweep removes it. Upload gives the bytes
// their key before the row commits, so a newer file can still get its row.
const orphanGraceHours = 24

// orphanBatchSize caps the file names the orphan sweep checks in one query.
const orphanBatchSize = 1000

// copyBatchSize caps the rows one transaction of the copy job adds, and the copy folders one query reads under.
// The insert updates every index of the table for each row: about 8 ms for 100 rows, 23 ms for 250.
// The job copies the bytes of a batch before its transaction, so a crash loses at most one batch of work.
const copyBatchSize = 100

// copyRetryHours is how long a copy root waits for its copy job before the sweep adds the job again.
const copyRetryHours = 24

// maxBuildingZips caps the zips one user can have building at once, so a user cannot fill the zip queue or the disk.
const maxBuildingZips = 3

// zipPageSize is the rows the zip job reads from a folder per query. It is the largest page the list allows.
const zipPageSize = 100

// zipRetentionHours is how long a download stays: a ready zip from when it got ready, any other from when it was asked
// for. A file in the zip dir with no row is removed after the same time.
const zipRetentionHours = 24

// zipSweepBatchSize caps the downloads one statement of the download sweep deletes, and the zip files one query checks.
const zipSweepBatchSize = 1000

// downloadListSize is the rows the downloads list shows. The sweep keeps the list to one day, so it has no pages.
const downloadListSize = 50

// zipCheckInterval is how often the zip job reads its row again. A removed download stops its job by then, so it
// does not hold the one zip worker. The read is one lookup by key.
const zipCheckInterval = time.Second

var (
	errEmptyFile      = apperr.New(apperr.KindValidation, "file is empty")
	errFolderNotFound = apperr.New(apperr.KindNotFound, "folder not found")
	errItemNotFound   = apperr.New(apperr.KindNotFound, "item not found")
	errNameTaken      = apperr.New(apperr.KindConflict, "an item with this name already exists")
	errMoveIntoItself = apperr.New(apperr.KindValidation, "a folder cannot move into itself or its own subfolder")
	errCopyIntoItself = apperr.New(apperr.KindValidation, "a folder cannot be copied into itself or its own subfolder")
	errSameFolder     = apperr.New(apperr.KindValidation, "the items are already in this folder")
	errFolderDownload = apperr.New(apperr.KindValidation, "a folder downloads as a zip")
	errNoBytes        = apperr.New(apperr.KindNotFound, "file not found on disk")
	errDownloadGone   = apperr.New(apperr.KindNotFound, "download not found")
	errTooManyZips    = apperr.New(apperr.KindConflict, "wait for your other downloads to finish")
	errZipNotReady    = apperr.New(apperr.KindConflict, "the zip is not ready yet")
	// errZipRemoved stops a zip job whose download the user removed.
	errZipRemoved = errors.New("the download was removed")
)

var noExtra = json.RawMessage("{}")

// zipFailedMessage is what the client shows for a zip whose job failed for good.
const zipFailedMessage = "the zip could not be built"

// A parentID of 0 means the root folder.
type FileServiceInterface interface {
	Upload(ctx context.Context, userID, parentID int64, name string, src io.Reader) (*File, error)
	CreateFolder(ctx context.Context, userID, parentID int64, name string) (*File, error)
	List(ctx context.Context, userID int64, q ListQuery) (*utils.Page[*File], error)
	ResolveFolder(ctx context.Context, userID int64, names []string) (*FolderPath, error)
	GetItem(ctx context.Context, userID, id int64) (*File, error)
	ListActivity(ctx context.Context, userID, id int64, q ActivityQuery) (*utils.Page[*Activity], error)
	Rename(ctx context.Context, userID, id int64, name string) (*File, error)
	AddFavorites(ctx context.Context, userID int64, ids []int64) error
	SetHidden(ctx context.Context, userID int64, ids []int64, hidden bool) error
	RemoveFavorites(ctx context.Context, userID int64, ids []int64) error
	OpenFile(ctx context.Context, userID, id int64) (*FileContent, error)
	RequestZip(ctx context.Context, userID int64, ids []int64) (*Download, error)
	GetDownload(ctx context.Context, userID, id int64) (*Download, error)
	ListDownloads(ctx context.Context, userID int64) ([]*Download, error)
	RemoveDownload(ctx context.Context, userID, id int64) error
	OpenZip(ctx context.Context, userID, id int64) (*FileContent, error)
	Move(ctx context.Context, userID, parentID int64, ids []int64) error
	Copy(ctx context.Context, userID, parentID int64, ids []int64) error
	Trash(ctx context.Context, userID int64, ids []int64) error
	Restore(ctx context.Context, userID int64, ids []int64) error
	Purge(ctx context.Context, userID int64, ids []int64) error
	ListTrash(ctx context.Context, userID int64, q TrashQuery) (*utils.Page[*TrashedFile], error)
}

var _ FileServiceInterface = (*FileService)(nil)

type FileService struct {
	queries  *database.Queries
	db       *sql.DB
	queue    jobs.InserterInterface
	cfg      config.UploadConfig
	download config.DownloadConfig
	logger   *zap.Logger
}

func NewFileService(
	queries *database.Queries,
	db *sql.DB,
	queue jobs.InserterInterface,
	cfg config.UploadConfig,
	download config.DownloadConfig,
	logger *zap.Logger,
) *FileService {
	return &FileService{
		queries:  queries,
		db:       db,
		queue:    queue,
		cfg:      cfg,
		download: download,
		logger:   logger,
	}
}

func toFileModel(dbFile database.File) *File {
	var parentID *int64
	if dbFile.ParentID.Valid {
		parentID = &dbFile.ParentID.Int64
	}

	return &File{
		ID:          dbFile.ID,
		ParentID:    parentID,
		IsFolder:    !dbFile.StorageKey.Valid,
		Name:        dbFile.Name,
		ContentType: dbFile.ContentType,
		SizeBytes:   dbFile.SizeBytes,
		CreatedAt:   dbFile.CreatedAt,
		UpdatedAt:   dbFile.UpdatedAt,
		IsCopying:   dbFile.CopyRequestedAt.Valid,
		IsHidden:    dbFile.HiddenAt.Valid,
	}
}

// fileFields follows the column order of the files table, which SELECT * returns.
func fileFields(f *database.File) []any {
	return []any{
		&f.ID, &f.UserID, &f.Name, &f.StorageKey, &f.ContentType, &f.SizeBytes,
		&f.CreatedAt, &f.UpdatedAt, &f.ParentID, &f.TrashedAt, &f.TrashRootID, &f.PurgeRequestedAt,
		&f.CopiedFromID, &f.CopyRequestedAt, &f.HiddenAt,
	}
}

func toDownloadModel(dl database.Download) *Download {
	// The sweep removes a ready zip a day after it got ready, any other row a day after the request.
	state, from := DownloadBuilding, dl.CreatedAt
	switch {
	case dl.ReadyAt.Valid:
		state, from = DownloadReady, dl.ReadyAt.Time
	case dl.Error.Valid:
		state = DownloadFailed
	}

	return &Download{
		ID:         dl.ID,
		Name:       dl.Name,
		State:      state,
		Error:      dl.Error.String,
		SizeBytes:  dl.SizeBytes,
		CreatedAt:  dl.CreatedAt,
		ExpiresAt:  from.Add(zipRetentionHours * time.Hour),
		Downloaded: dl.DownloadedAt.Valid,
	}
}

func (s *FileService) Upload(ctx context.Context, userID, parentID int64, name string, src io.Reader) (*File, error) {
	if err := utils.ValidateName(name, utils.FileName); err != nil {
		return nil, err
	}
	// Check the folder before writing bytes, so a bad folder leaves nothing on disk.
	if err := s.checkFolder(ctx, s.queries, userID, parentID); err != nil {
		return nil, err
	}

	br := bufio.NewReader(src)
	head, err := br.Peek(512)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("failed to read upload: %w", err)
	}
	if len(head) == 0 {
		return nil, errEmptyFile
	}

	key, size, err := utils.StoreFile(s.cfg.Dir, br, s.cfg.MaxSizeMB<<20)
	if err != nil {
		if errors.Is(err, utils.ErrFileTooLarge) {
			return nil, apperr.New(apperr.KindTooLarge, fmt.Sprintf("file is larger than the %d MB limit", s.cfg.MaxSizeMB))
		}
		return nil, err
	}

	dbFile, err := s.insertWithFreeName(ctx, userID, parentID, name, utils.FileName, EventUploaded, func(q *database.Queries, freeName string) (database.File, error) {
		return q.CreateFile(ctx, database.CreateFileParams{
			UserID:      userID,
			ParentID:    sql.NullInt64{Int64: parentID, Valid: parentID != 0},
			Name:        freeName,
			StorageKey:  sql.NullString{String: key, Valid: true},
			ContentType: http.DetectContentType(head),
			SizeBytes:   size,
		})
	})
	if err != nil {
		_ = utils.RemoveStoredFile(s.cfg.Dir, key)
		return nil, err
	}

	return toFileModel(dbFile), nil
}

func (s *FileService) CreateFolder(ctx context.Context, userID, parentID int64, name string) (*File, error) {
	name = strings.TrimSpace(name)
	if err := utils.ValidateName(name, utils.FolderName); err != nil {
		return nil, err
	}

	dbFile, err := s.insertWithFreeName(ctx, userID, parentID, name, utils.FolderName, EventCreated, func(q *database.Queries, freeName string) (database.File, error) {
		return q.CreateFolder(ctx, database.CreateFolderParams{
			UserID:   userID,
			ParentID: sql.NullInt64{Int64: parentID, Valid: parentID != 0},
			Name:     freeName,
		})
	})
	if err != nil {
		return nil, err
	}

	return toFileModel(dbFile), nil
}

func (s *FileService) List(ctx context.Context, userID int64, q ListQuery) (*utils.Page[*File], error) {
	if err := s.checkFolder(ctx, s.queries, userID, q.ParentID); err != nil {
		return nil, err
	}

	page, err := utils.FetchPage(ctx, s.db, fileList, utils.PageQuery[SortField]{
		Args:     []any{userID, q.ParentID, strings.TrimSpace(q.Search), q.ShowHidden},
		Sort:     q.Sort,
		Desc:     q.Desc,
		Cursor:   q.Cursor,
		PageSize: q.PageSize,
	}, fileFields)
	if err != nil {
		return nil, err
	}

	files := make([]*File, len(page.Data))
	for i, dbFile := range page.Data {
		files[i] = toFileModel(dbFile)
	}
	if err := s.markFavorites(ctx, userID, files); err != nil {
		return nil, err
	}

	return &utils.Page[*File]{Data: files, NextCursor: page.NextCursor}, nil
}

func (s *FileService) ResolveFolder(ctx context.Context, userID int64, names []string) (*FolderPath, error) {
	if len(names) == 0 {
		return &FolderPath{Path: []*PathEntry{}}, nil
	}

	rows, err := s.queries.ResolveFolderPath(ctx, database.ResolveFolderPathParams{UserID: userID, Names: names})
	if err != nil {
		return nil, err
	}
	if len(rows) < len(names) {
		return nil, errFolderNotFound
	}

	path := make([]*PathEntry, len(rows))
	for i, row := range rows {
		path[i] = &PathEntry{ID: row.ID, Name: row.Name}
	}

	return &FolderPath{Path: path}, nil
}

// GetItem reads one live item of the user, for its details.
func (s *FileService) GetItem(ctx context.Context, userID, id int64) (*File, error) {
	dbFile, err := s.queries.GetFile(ctx, database.GetFileParams{ID: id, UserID: userID})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errItemNotFound
	}
	if err != nil {
		return nil, err
	}

	f := toFileModel(dbFile)
	if err := s.markFavorites(ctx, userID, []*File{f}); err != nil {
		return nil, err
	}
	return f, nil
}

// ListActivity reads one page of an item's audit log, newest first. The rows belong to the user, so an id that is not
// the user's gives an empty page. The log stays after a purge, so it does not check that the item is live.
func (s *FileService) ListActivity(ctx context.Context, userID, id int64, q ActivityQuery) (*utils.Page[*Activity], error) {
	page, err := utils.FetchPage(ctx, s.db, activityList, utils.PageQuery[SortField]{
		Args:     []any{userID, id},
		Sort:     SortByDate,
		Desc:     true,
		Cursor:   q.Cursor,
		PageSize: q.PageSize,
	}, func(a *Activity) []any {
		return []any{&a.ID, &a.Event, &a.Metadata, &a.CreatedAt}
	})
	if err != nil {
		return nil, err
	}

	list := make([]*Activity, len(page.Data))
	for i := range page.Data {
		list[i] = &page.Data[i]
	}
	return &utils.Page[*Activity]{Data: list, NextCursor: page.NextCursor}, nil
}

func (s *FileService) Rename(ctx context.Context, userID, id int64, name string) (*File, error) {
	dbFile, err := s.queries.GetFile(ctx, database.GetFileParams{ID: id, UserID: userID})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errItemNotFound
	}
	if err != nil {
		return nil, err
	}

	name = strings.TrimSpace(name)
	if err := utils.ValidateName(name, utils.NameKindOf(toFileModel(dbFile).IsFolder)); err != nil {
		return nil, err
	}

	oldName := dbFile.Name
	extra, err := json.Marshal(map[string]string{"old_name": oldName})
	if err != nil {
		return nil, err
	}
	err = utils.InTx(ctx, s.db, func(tx *sql.Tx) error {
		q := s.queries.WithTx(tx)
		var err error
		if dbFile, err = q.RenameFile(ctx, database.RenameFileParams{ID: id, UserID: userID, Name: name}); err != nil {
			return err
		}
		if dbFile.Name == oldName {
			return nil
		}
		return q.LogItems(ctx, database.LogItemsParams{Event: string(EventRenamed), Extra: extra, UserID: userID, Ids: []int64{id}})
	})
	if utils.IsUniqueViolation(err, nameIndex) {
		return nil, errNameTaken
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errItemNotFound
	}
	if err != nil {
		return nil, err
	}

	f := toFileModel(dbFile)
	if err := s.markFavorites(ctx, userID, []*File{f}); err != nil {
		return nil, err
	}
	return f, nil
}

// AddFavorites marks the items as favorites. It is all or none, the same as Trash. An item that is a favorite already
// stays one. It changes no item, so it takes no tree lock. A favorite stays with its item in the trash, and a purge
// removes it.
func (s *FileService) AddFavorites(ctx context.Context, userID int64, ids []int64) error {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))

	items, err := s.queries.ListLiveItems(ctx, database.ListLiveItemsParams{UserID: userID, Ids: ids})
	if err != nil {
		return err
	}
	if len(items) != len(ids) {
		return errItemNotFound
	}
	return s.queries.AddFavorites(ctx, database.AddFavoritesParams{UserID: userID, Ids: ids})
}

// SetHidden hides the items from the list, or shows them again. It is all or none, the same as Trash. An item already
// in that state keeps it and gets no audit row. It changes no tree, so it takes no tree lock.
func (s *FileService) SetHidden(ctx context.Context, userID int64, ids []int64, hidden bool) error {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))

	return utils.InTx(ctx, s.db, func(tx *sql.Tx) error {
		q := s.queries.WithTx(tx)
		rows, err := q.SetHidden(ctx, database.SetHiddenParams{UserID: userID, Ids: ids, Hidden: hidden})
		if err != nil {
			return err
		}
		if len(rows) != len(ids) {
			return errItemNotFound
		}

		var changed []int64
		for _, row := range rows {
			if row.Changed {
				changed = append(changed, row.ID)
			}
		}
		if len(changed) == 0 {
			return nil
		}
		return q.LogItems(ctx, database.LogItemsParams{Event: string(hiddenEvents[hidden]), Extra: noExtra, UserID: userID, Ids: changed})
	})
}

// RemoveFavorites drops the favorite mark of the items. An id that is not a favorite of the user changes nothing.
func (s *FileService) RemoveFavorites(ctx context.Context, userID int64, ids []int64) error {
	return s.queries.RemoveFavorites(ctx, database.RemoveFavoritesParams{UserID: userID, Ids: ids})
}

// OpenFile opens one live file of the user for a download. A folder downloads as a zip, and a file that still copies
// may not have its bytes yet.
func (s *FileService) OpenFile(ctx context.Context, userID, id int64) (*FileContent, error) {
	dbFile, err := s.queries.GetFile(ctx, database.GetFileParams{ID: id, UserID: userID})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errItemNotFound
	}
	if err != nil {
		return nil, err
	}
	if !dbFile.StorageKey.Valid {
		return nil, errFolderDownload
	}
	if dbFile.CopyRequestedAt.Valid {
		return nil, apperr.New(apperr.KindConflict, dbFile.Name+" is still copying")
	}

	f, err := utils.OpenStoredFile(s.cfg.Dir, dbFile.StorageKey.String)
	if errors.Is(err, utils.ErrNoStoredFile) {
		s.logger.Warn("a file has no bytes on disk", zap.Int64("user_id", userID), zap.Int64("file_id", id))
		return nil, errNoBytes
	}
	if err != nil {
		return nil, err
	}

	return &FileContent{
		Name:        dbFile.Name,
		ContentType: dbFile.ContentType,
		ModTime:     dbFile.CreatedAt,
		ETag:        `"` + dbFile.StorageKey.String + `"`,
		Content:     f,
	}, nil
}

// RequestZip adds a zip download of the items and its build job. It is all or none, the same as Trash. It changes no
// item, so it takes no tree lock, and it reads the items before the transaction. The user lock keeps two requests at
// once from both passing the limit on building zips.
func (s *FileService) RequestZip(ctx context.Context, userID int64, ids []int64) (*Download, error) {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))

	items, err := s.queries.ListLiveItems(ctx, database.ListLiveItemsParams{UserID: userID, Ids: ids})
	if err != nil {
		return nil, err
	}
	if len(items) != len(ids) {
		return nil, errItemNotFound
	}
	for _, item := range items {
		if item.CopyRequestedAt.Valid {
			return nil, apperr.New(apperr.KindConflict, item.Name+" is still copying")
		}
	}
	name, err := s.zipName(ctx, userID, items)
	if err != nil {
		return nil, err
	}

	var dl database.Download
	err = utils.InTx(ctx, s.db, func(tx *sql.Tx) error {
		q := s.queries.WithTx(tx)
		if err := q.LockUserDownloads(ctx, userID); err != nil {
			return err
		}

		building, err := q.CountBuildingDownloads(ctx, userID)
		if err != nil {
			return err
		}
		if building >= maxBuildingZips {
			return errTooManyZips
		}

		dl, err = q.CreateDownload(ctx, database.CreateDownloadParams{UserID: userID, Name: name, ItemIds: ids})
		if err != nil {
			return err
		}
		_, err = s.queue.InsertTx(ctx, tx, jobs.BuildZipArgs{UserID: userID, DownloadID: dl.ID}, nil)
		return err
	})
	if err != nil {
		return nil, err
	}
	return toDownloadModel(dl), nil
}

func (s *FileService) GetDownload(ctx context.Context, userID, id int64) (*Download, error) {
	dl, err := s.queries.GetDownload(ctx, database.GetDownloadParams{ID: id, UserID: userID})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errDownloadGone
	}
	if err != nil {
		return nil, err
	}
	return toDownloadModel(dl), nil
}

func (s *FileService) ListDownloads(ctx context.Context, userID int64) ([]*Download, error) {
	rows, err := s.queries.ListDownloads(ctx, database.ListDownloadsParams{UserID: userID, RowLimit: downloadListSize})
	if err != nil {
		return nil, err
	}
	list := make([]*Download, len(rows))
	for i, row := range rows {
		list[i] = toDownloadModel(row)
	}
	return list, nil
}

// RemoveDownload deletes a download, then its zip, the same order as the sweep, so a client never gets a ready row
// whose zip is gone. A zip that does not go is logged, and the sweep removes it later. A job that still builds the
// zip stops at its next check. A browser that downloads the zip right now still gets all of it, because Linux keeps an
// open file readable after a remove.
func (s *FileService) RemoveDownload(ctx context.Context, userID, id int64) error {
	key, err := s.queries.DeleteDownload(ctx, database.DeleteDownloadParams{ID: id, UserID: userID})
	if errors.Is(err, sql.ErrNoRows) {
		return errDownloadGone
	}
	if err != nil || !key.Valid {
		return err
	}

	if err := utils.RemoveStoredFile(filepath.Join(s.cfg.Dir, utils.ZipDir), key.String); err != nil {
		s.logger.Warn("a removed download kept its zip", zap.Int64("user_id", userID), zap.Int64("download_id", id),
			zap.Error(err))
	}
	return nil
}

// OpenZip opens a ready zip of the user for a download and marks the zip downloaded. The ETag is the zip's key,
// because a zip never changes.
func (s *FileService) OpenZip(ctx context.Context, userID, id int64) (*FileContent, error) {
	dl, err := s.queries.GetDownload(ctx, database.GetDownloadParams{ID: id, UserID: userID})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errDownloadGone
	}
	if err != nil {
		return nil, err
	}
	if !dl.ReadyAt.Valid {
		return nil, errZipNotReady
	}

	f, err := utils.OpenStoredFile(filepath.Join(s.cfg.Dir, utils.ZipDir), dl.StorageKey.String)
	if errors.Is(err, utils.ErrNoStoredFile) {
		s.logger.Warn("a zip has no bytes on disk", zap.Int64("user_id", userID), zap.Int64("download_id", id))
		return nil, errNoBytes
	}
	if err != nil {
		return nil, err
	}
	if !dl.DownloadedAt.Valid {
		if err := s.queries.MarkDownloaded(ctx, database.MarkDownloadedParams{ID: id, UserID: userID}); err != nil {
			_ = f.Close()
			return nil, err
		}
	}

	return &FileContent{
		Name:        dl.Name,
		ContentType: "application/zip",
		ModTime:     dl.ReadyAt.Time,
		ETag:        `"` + dl.StorageKey.String + `"`,
		Content:     f,
	}, nil
}

// Move is all or none, the same as Trash. A name in use in the target fails the move: it does not auto-rename.
// The tree lock makes moves run one at a time. Two moves at once (A into Y under B, B into X under A) would each
// see no loop and lock different rows, so both would commit.
func (s *FileService) Move(ctx context.Context, userID, parentID int64, ids []int64) error {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))

	err := utils.InTx(ctx, s.db, func(tx *sql.Tx) error {
		q := s.queries.WithTx(tx)
		if err := q.LockUserTree(ctx, userID); err != nil {
			return err
		}
		if _, err := s.lockPasteItems(ctx, q, userID, parentID, ids, errMoveIntoItself); err != nil {
			return err
		}
		// Before the move, so the log still sees the folder each item leaves.
		if err := q.LogMoves(ctx, database.LogMovesParams{ParentID: parentID, UserID: userID, Ids: ids}); err != nil {
			return err
		}
		return q.MoveItems(ctx, database.MoveItemsParams{ParentID: parentID, UserID: userID, Ids: ids})
	})
	if utils.IsUniqueViolation(err, nameIndex) {
		return errNameTaken
	}
	return err
}

// Copy is all or none, with the same checks as Move. It adds the copy roots at once, marked as copying, so their names
// are taken. A job copies the bytes and the rows under the folders (CopyItems). An item that still copies cannot be
// copied, because the job would copy only part of it.
func (s *FileService) Copy(ctx context.Context, userID, parentID int64, ids []int64) error {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))

	err := utils.InTx(ctx, s.db, func(tx *sql.Tx) error {
		q := s.queries.WithTx(tx)
		if err := q.LockUserTree(ctx, userID); err != nil {
			return err
		}
		items, err := s.lockPasteItems(ctx, q, userID, parentID, ids, errCopyIntoItself)
		if err != nil {
			return err
		}

		roots := database.CreateCopyRootsParams{
			ParentID: parentID,
			Keys:     make([]string, len(items)),
			Ids:      make([]int64, len(items)),
			UserID:   userID,
		}
		for i, item := range items {
			if item.Copying {
				return apperr.New(apperr.KindConflict, item.Name+" is still copying")
			}
			roots.Ids[i] = item.ID
			if item.IsFolder {
				continue
			}
			// The job writes the bytes under this key.
			if roots.Keys[i], err = utils.NewStorageKey(); err != nil {
				return err
			}
		}

		rootIDs, err := q.CreateCopyRoots(ctx, roots)
		if err != nil {
			return err
		}
		slices.Sort(rootIDs)
		// Only the copies the user pasted get a row. The rows the job adds under them get none.
		err = q.LogItems(ctx, database.LogItemsParams{Event: string(EventCopied), Extra: noExtra, UserID: userID, Ids: rootIDs})
		if err != nil {
			return err
		}
		_, err = s.queue.InsertTx(ctx, tx, jobs.CopyItemsArgs{UserID: userID, RootIDs: rootIDs}, nil)
		return err
	})
	if utils.IsUniqueViolation(err, nameIndex) {
		return errNameTaken
	}
	return err
}

// Trash is all or none: an id that is unknown, someone else's or already in the trash undoes the rest.
// It marks only the items. A job marks the rows under the folders (TrashSubtrees), so a big folder does not hold the tree lock long.
func (s *FileService) Trash(ctx context.Context, userID int64, ids []int64) error {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))

	return utils.InTx(ctx, s.db, func(tx *sql.Tx) error {
		q := s.queries.WithTx(tx)
		if err := q.LockUserTree(ctx, userID); err != nil {
			return err
		}
		trashed, err := q.TrashItems(ctx, database.TrashItemsParams{UserID: userID, Ids: ids})
		if err != nil {
			return err
		}
		if len(trashed) != len(ids) {
			return errItemNotFound
		}

		var folders []int64
		for _, item := range trashed {
			// The rollback undoes the whole delete. A delete in the middle of a copy would leave a part copy.
			if item.Copying {
				return apperr.New(apperr.KindConflict, item.Name+" is still copying")
			}
			if item.IsFolder {
				folders = append(folders, item.ID)
			}
		}
		// Only the items the user deleted get a row, not the rows under them.
		err = q.LogItems(ctx, database.LogItemsParams{Event: string(EventTrashed), Extra: noExtra, UserID: userID, Ids: ids})
		if err != nil || len(folders) == 0 {
			return err
		}
		_, err = s.queue.InsertTx(ctx, tx, jobs.TrashSubtreesArgs{UserID: userID, RootIDs: folders}, nil)
		return err
	})
}

// TrashSubtrees marks the live rows under the trash roots, one level at a time.
// Each level walks the folders of the groups, marked now or by an earlier run, so a run after a failure finishes the job.
func (s *FileService) TrashSubtrees(ctx context.Context, userID int64, rootIDs []int64) error {
	var rows int64
	parents := rootIDs
	for len(parents) > 0 {
		var next []int64
		for chunk := range slices.Chunk(parents, trashBatchSize) {
			marked, err := s.trashChildren(ctx, userID, rootIDs, chunk)
			if err != nil {
				return err
			}
			rows += marked
			folders, err := s.queries.ListGroupFolders(ctx, database.ListGroupFoldersParams{
				UserID:    userID,
				ParentIds: chunk,
				RootIds:   rootIDs,
			})
			if err != nil {
				return err
			}
			next = append(next, folders...)
		}
		parents = next
	}

	if rows > 0 {
		s.logger.Info("trashed the rows under folders",
			zap.Int64("user_id", userID), zap.Int64s("root_ids", rootIDs), zap.Int64("rows", rows))
	}
	return nil
}

// Restore is all or none, the same as Trash. Each id must be an item the user deleted, not a row under one.
// Another request can take a free name before the commit. Then the whole restore runs again.
func (s *FileService) Restore(ctx context.Context, userID int64, ids []int64) error {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))

	for range maxNameAttempts {
		err := utils.InTx(ctx, s.db, func(tx *sql.Tx) error {
			q := s.queries.WithTx(tx)
			if err := q.LockUserTree(ctx, userID); err != nil {
				return err
			}
			roots, err := q.ListTrashRoots(ctx, database.ListTrashRootsParams{UserID: userID, Ids: ids})
			if err != nil {
				return err
			}
			if len(roots) != len(ids) {
				return errItemNotFound
			}

			if err := q.RestoreTrashGroups(ctx, database.RestoreTrashGroupsParams{UserID: userID, Ids: ids}); err != nil {
				return err
			}

			restored := database.RestoreTrashRootsParams{
				UserID:    userID,
				Ids:       make([]int64, len(roots)),
				ParentIds: make([]int64, len(roots)),
				Names:     make([]string, len(roots)),
			}
			prefixes := make([]string, len(roots))
			for i, root := range roots {
				restored.Ids[i], restored.ParentIds[i] = root.ID, root.NewParentID
				prefixes[i], _ = utils.SplitName(root.Name, utils.NameKindOf(root.IsFolder))
			}

			rows, err := q.ListNamesByParentsAndPrefixes(ctx, database.ListNamesByParentsAndPrefixesParams{
				UserID:    userID,
				ParentIds: restored.ParentIds,
				Prefixes:  prefixes,
			})
			if err != nil {
				return err
			}
			taken := make(map[int64][]string)
			for _, row := range rows {
				taken[row.ParentID] = append(taken[row.ParentID], row.Name)
			}

			// The roots come in id order, and each name a root takes is taken for the roots after it.
			for i, root := range roots {
				name, err := utils.FreeName(root.Name, utils.NameKindOf(root.IsFolder), taken[root.NewParentID])
				if err != nil {
					return err
				}
				taken[root.NewParentID] = append(taken[root.NewParentID], name)
				restored.Names[i] = name
			}

			if err := q.RestoreTrashRoots(ctx, restored); err != nil {
				return err
			}
			// After the restore, so the row holds the name and the folder the item came back with.
			return q.LogItems(ctx, database.LogItemsParams{Event: string(EventRestored), Extra: noExtra, UserID: userID, Ids: ids})
		})
		if utils.IsUniqueViolation(err, nameIndex) {
			continue
		}
		return err
	}

	return errNameTaken
}

// Purge is all or none, the same as Restore. It only marks the items, so the trash view and restore skip them at once.
// A job deletes the rows and the bytes (PurgeGroups).
func (s *FileService) Purge(ctx context.Context, userID int64, ids []int64) error {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))

	return utils.InTx(ctx, s.db, func(tx *sql.Tx) error {
		q := s.queries.WithTx(tx)
		if err := q.LockUserTree(ctx, userID); err != nil {
			return err
		}
		marked, err := q.RequestPurge(ctx, database.RequestPurgeParams{UserID: userID, Ids: ids})
		if err != nil {
			return err
		}
		if marked != int64(len(ids)) {
			return errItemNotFound
		}
		if err := q.LogItems(ctx, database.LogItemsParams{Event: string(EventPurged), Extra: noExtra, UserID: userID, Ids: ids}); err != nil {
			return err
		}
		_, err = s.queue.InsertTx(ctx, tx, jobs.PurgeGroupsArgs{UserID: userID, RootIDs: ids}, nil)
		return err
	})
}

// PurgeGroups deletes the marked groups for good, in short transactions. It finishes the trash marking first,
// so no live row is left under a group. A run after a failure finishes the job.
func (s *FileService) PurgeGroups(ctx context.Context, userID int64, rootIDs []int64) error {
	rootIDs, err := s.queries.ListPurgeRoots(ctx, database.ListPurgeRootsParams{UserID: userID, Ids: rootIDs})
	if err != nil || len(rootIDs) == 0 {
		return err
	}
	if err := s.TrashSubtrees(ctx, userID, rootIDs); err != nil {
		return err
	}
	movedHome, err := s.moveNestedRootsHome(ctx, userID, rootIDs)
	if err != nil {
		return err
	}
	rows, files, err := s.deleteGroupRows(ctx, userID, rootIDs)
	if err != nil {
		return err
	}
	s.logger.Info("purged trash groups",
		zap.Int64("user_id", userID), zap.Int64s("root_ids", rootIDs),
		zap.Int64("rows", rows), zap.Int64("files", files), zap.Int64("moved_home", movedHome))

	// A root goes last, so a root that is still there means a row outside the groups blocks the delete.
	left, err := s.queries.ListPurgeRoots(ctx, database.ListPurgeRootsParams{UserID: userID, Ids: rootIDs})
	if err != nil {
		return err
	}
	if len(left) > 0 {
		return fmt.Errorf("purge left trash roots %v behind", left)
	}
	return nil
}

// CopyItems copies what the copy roots stand for: the bytes of each file, and the rows under each folder, one level at
// a time. It finds the rows it made by copied_from_id, so a run after a failure skips them. Then it clears the mark.
func (s *FileService) CopyItems(ctx context.Context, userID int64, rootIDs []int64) error {
	roots, err := s.queries.ListCopyRoots(ctx, database.ListCopyRootsParams{UserID: userID, Ids: rootIDs})
	if err != nil || len(roots) == 0 {
		return err
	}

	var rows, files, size int64
	ids := make([]int64, len(roots))
	var parents []int64
	for i, root := range roots {
		ids[i] = root.ID
		if root.IsFolder {
			parents = append(parents, root.ID)
			continue
		}

		n, err := int64(0), utils.ErrNoStoredFile
		if root.SourceKey.Valid {
			n, err = utils.CopyStoredFile(s.cfg.Dir, root.SourceKey.String, root.StorageKey.String)
		}
		if errors.Is(err, utils.ErrNoStoredFile) {
			// The source is gone or lost its bytes, so the copy can never get any.
			err = utils.InTx(ctx, s.db, func(tx *sql.Tx) error {
				q := s.queries.WithTx(tx)
				if err := q.LockUserTree(ctx, userID); err != nil {
					return err
				}
				return q.DeleteCopyRoot(ctx, database.DeleteCopyRootParams{UserID: userID, ID: root.ID})
			})
			if err != nil {
				return err
			}
			s.logger.Warn("removed a copy with no bytes",
				zap.Int64("user_id", userID), zap.Int64("copy_id", root.ID), zap.Int64("source_id", root.SourceID))
			continue
		}
		if err != nil {
			return err
		}
		files++
		size += n
	}

	for len(parents) > 0 {
		var next []int64
		for chunk := range slices.Chunk(parents, copyBatchSize) {
			r, f, n, err := s.copyChildren(ctx, userID, chunk)
			rows, files, size = rows+r, files+f, size+n
			if err != nil {
				return err
			}
			folders, err := s.queries.ListCopyFolders(ctx, database.ListCopyFoldersParams{UserID: userID, ParentIds: chunk})
			if err != nil {
				return err
			}
			next = append(next, folders...)
		}
		parents = next
	}

	if err := s.queries.FinishCopies(ctx, database.FinishCopiesParams{UserID: userID, Ids: ids}); err != nil {
		return err
	}
	s.logger.Info("copied items", zap.Int64("user_id", userID), zap.Int64s("root_ids", ids),
		zap.Int64("rows", rows), zap.Int64("files", files), zap.Int64("bytes", size))
	return nil
}

// BuildZip builds the zip of a download: the selected items, then the folders under them, level by level. The job only
// reads, one short statement at a time, so it takes no tree lock. A rename or move during the build can change what
// the zip holds: each folder goes in as the job finds it, the same as copy. A run after a failure starts again,
// because a zip cannot go on from a half-written file. The job reads its row again every zipCheckInterval, and stops
// when the user removed the download.
func (s *FileService) BuildZip(ctx context.Context, userID, downloadID int64) error {
	dl, err := s.queries.GetDownload(ctx, database.GetDownloadParams{ID: downloadID, UserID: userID})
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil || dl.ReadyAt.Valid || dl.Error.Valid {
		return err
	}

	zb, err := utils.NewZipBuilder(s.cfg.Dir, s.download.MaxZipMB<<20)
	if err != nil {
		return err
	}
	defer zb.Abort()
	zb.CheckEvery(zipCheckInterval, func() error {
		_, err := s.queries.GetDownload(ctx, database.GetDownloadParams{ID: downloadID, UserID: userID})
		if errors.Is(err, sql.ErrNoRows) {
			return errZipRemoved
		}
		return err
	})

	files, folders, err := s.writeZip(ctx, zb, userID, dl.ItemIds)
	if errors.Is(err, errZipRemoved) {
		s.logger.Info("stopped a zip whose download was removed", zap.Int64("user_id", userID),
			zap.Int64("download_id", downloadID))
		return nil
	}
	if errors.Is(err, utils.ErrZipTooLarge) {
		// Another run would stop at the same place, so the zip fails now.
		s.logger.Info("stopped a zip over the size cap", zap.Int64("user_id", userID), zap.Int64("download_id", downloadID))
		return s.queries.FailDownload(ctx, database.FailDownloadParams{
			Error:  sql.NullString{String: fmt.Sprintf("the zip is larger than the %d MB limit", s.download.MaxZipMB), Valid: true},
			ID:     downloadID,
			UserID: userID,
		})
	}
	if err != nil {
		return err
	}

	key, size, err := zb.Finish()
	if err != nil {
		return err
	}
	marked, err := s.queries.FinishDownload(ctx, database.FinishDownloadParams{
		StorageKey: sql.NullString{String: key, Valid: true},
		SizeBytes:  size,
		ID:         downloadID,
		UserID:     userID,
	})
	if err != nil || marked == 0 {
		// The row is gone or failed meanwhile, or the update failed. No one can get this zip.
		_ = utils.RemoveStoredFile(filepath.Join(s.cfg.Dir, utils.ZipDir), key)
		return err
	}

	s.logger.Info("built a zip", zap.Int64("user_id", userID), zap.Int64("download_id", downloadID),
		zap.Int64("files", files), zap.Int64("folders", folders), zap.Int64("bytes", size))
	return nil
}

// FailZip marks a download that is still building as failed. The zip job calls it after its last attempt.
func (s *FileService) FailZip(ctx context.Context, userID, downloadID int64) error {
	return s.queries.FailDownload(ctx, database.FailDownloadParams{
		Error:  sql.NullString{String: zipFailedMessage, Valid: true},
		ID:     downloadID,
		UserID: userID,
	})
}

// SweepTrash marks the items in the trash longer than trashRetentionDays for a permanent delete and adds their purge jobs.
// It also adds the purge job again for groups marked long ago, so a job River gave up on does not hide them forever.
func (s *FileService) SweepTrash(ctx context.Context) error {
	users, err := s.queries.ListUsersWithExpiredTrash(ctx, trashRetentionDays)
	if err != nil {
		return err
	}
	for _, userID := range users {
		if err := s.requestExpiredPurges(ctx, userID); err != nil {
			return err
		}
	}

	stale, err := s.queries.ListStalePurges(ctx, purgeRetryHours)
	if err != nil {
		return err
	}
	for _, group := range stale {
		for rootIDs := range slices.Chunk(group.RootIds, purgeBatchSize) {
			var res *rivertype.JobInsertResult
			err := utils.InTx(ctx, s.db, func(tx *sql.Tx) error {
				var err error
				res, err = s.queue.InsertTx(ctx, tx, jobs.PurgeGroupsArgs{UserID: group.UserID, RootIDs: rootIDs}, nil)
				return err
			})
			if err != nil {
				return err
			}
			if !res.UniqueSkippedAsDuplicate {
				s.logger.Info("added the purge job again",
					zap.Int64("user_id", group.UserID), zap.Int64s("root_ids", rootIDs))
			}
		}
	}
	return nil
}

// SweepCopies adds the copy job again for copy roots marked long ago, so a job River gave up on does not leave them
// copying forever. The first job can still be alive under other args. Two runs at once are safe: a batch skips the rows
// the other run added, and each byte copy has its own temp file.
func (s *FileService) SweepCopies(ctx context.Context) error {
	stale, err := s.queries.ListStaleCopies(ctx, copyRetryHours)
	if err != nil {
		return err
	}
	for _, group := range stale {
		for rootIDs := range slices.Chunk(group.RootIds, copyBatchSize) {
			var res *rivertype.JobInsertResult
			err := utils.InTx(ctx, s.db, func(tx *sql.Tx) error {
				var err error
				res, err = s.queue.InsertTx(ctx, tx, jobs.CopyItemsArgs{UserID: group.UserID, RootIDs: rootIDs}, nil)
				return err
			})
			if err != nil {
				return err
			}
			if !res.UniqueSkippedAsDuplicate {
				s.logger.Info("added the copy job again",
					zap.Int64("user_id", group.UserID), zap.Int64s("root_ids", rootIDs))
			}
		}
	}
	return nil
}

// SweepDownloads removes the downloads older than zipRetentionHours, then the files in the zip dir that no row points
// to, such as the temp file of a build the app stopped. It deletes the rows before their zips, so a client never gets
// a ready row whose zip is gone. A zip that does not go has no row then, and a later sweep removes it.
func (s *FileService) SweepDownloads(ctx context.Context) error {
	zipDir := filepath.Join(s.cfg.Dir, utils.ZipDir)
	for {
		expired, err := s.queries.DeleteExpiredDownloads(ctx, database.DeleteExpiredDownloadsParams{
			RetentionHours: zipRetentionHours,
			BatchSize:      zipSweepBatchSize,
		})
		if err != nil {
			return err
		}

		ids := make([]int64, len(expired))
		var zips int64
		for i, row := range expired {
			ids[i] = row.ID
			if !row.StorageKey.Valid {
				continue
			}
			if err := utils.RemoveStoredFile(zipDir, row.StorageKey.String); err != nil {
				return fmt.Errorf("failed to remove zip: %w", err)
			}
			zips++
		}
		if len(expired) > 0 {
			s.logger.Info("removed expired downloads", zap.Int64s("download_ids", ids), zap.Int64("zips", zips))
		}
		if len(expired) < zipSweepBatchSize {
			break
		}
	}

	cutoff := time.Now().Add(-zipRetentionHours * time.Hour)
	err := utils.ListStoredFiles(zipDir, cutoff, zipSweepBatchSize, func(files []utils.StoredFile) error {
		byName := make(map[string]utils.StoredFile, len(files))
		names := make([]string, len(files))
		for i, f := range files {
			byName[f.Name] = f
			names[i] = f.Name
		}
		strays, err := s.queries.ListUnknownZipKeys(ctx, names)
		if err != nil {
			return err
		}
		for _, name := range strays {
			if err := utils.RemoveStoredFile(zipDir, name); err != nil {
				return fmt.Errorf("failed to remove stray zip file: %w", err)
			}
			s.logger.Info("removed a stray zip file", zap.String("name", name),
				zap.Int64("size_bytes", byName[name].Size), zap.Time("modified_at", byName[name].ModTime))
		}
		return nil
	})
	// The zip dir comes with the first zip.
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// SweepOrphans removes the files in the upload dir that no row points to, such as the bytes of an upload that never got its row.
// If no row has bytes, it removes nothing, because the app can point at a new, empty database by mistake.
func (s *FileService) SweepOrphans(ctx context.Context) error {
	hasFiles, err := s.queries.HasStoredFiles(ctx)
	if err != nil {
		return err
	}
	if !hasFiles {
		s.logger.Info("skipped the orphan sweep: no row has a stored file")
		return nil
	}

	cutoff := time.Now().Add(-orphanGraceHours * time.Hour)
	return utils.ListStoredFiles(s.cfg.Dir, cutoff, orphanBatchSize, func(files []utils.StoredFile) error {
		byName := make(map[string]utils.StoredFile, len(files))
		names := make([]string, len(files))
		for i, f := range files {
			byName[f.Name] = f
			names[i] = f.Name
		}
		orphans, err := s.queries.ListOrphanKeys(ctx, names)
		if err != nil {
			return err
		}
		for _, name := range orphans {
			if err := utils.RemoveStoredFile(s.cfg.Dir, name); err != nil {
				return fmt.Errorf("failed to remove orphan file: %w", err)
			}
			s.logger.Info("removed an orphan file", zap.String("name", name),
				zap.Int64("size_bytes", byName[name].Size), zap.Time("modified_at", byName[name].ModTime))
		}
		return nil
	})
}

func (s *FileService) ListTrash(ctx context.Context, userID int64, q TrashQuery) (*utils.Page[*TrashedFile], error) {
	page, err := utils.FetchPage(ctx, s.db, trashList, utils.PageQuery[SortField]{
		Args:     []any{userID},
		Sort:     q.Sort,
		Desc:     q.Desc,
		Cursor:   q.Cursor,
		PageSize: q.PageSize,
	}, fileFields)
	if err != nil {
		return nil, err
	}

	items := make([]*TrashedFile, len(page.Data))
	files := make([]*File, len(page.Data))
	for i, dbFile := range page.Data {
		items[i] = &TrashedFile{File: *toFileModel(dbFile), TrashedAt: dbFile.TrashedAt.Time}
		files[i] = &items[i].File
	}
	if err := s.markFavorites(ctx, userID, files); err != nil {
		return nil, err
	}

	return &utils.Page[*TrashedFile]{Data: items, NextCursor: page.NextCursor}, nil
}

// markFavorites sets IsFavorite on the items with one query.
func (s *FileService) markFavorites(ctx context.Context, userID int64, items []*File) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]int64, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	favorites, err := s.queries.ListFavoriteIDs(ctx, database.ListFavoriteIDsParams{UserID: userID, Ids: ids})
	if err != nil {
		return err
	}
	for _, item := range items {
		item.IsFavorite = slices.Contains(favorites, item.ID)
	}
	return nil
}

// trashChildren marks the live rows right under parents, one batch per transaction, until a batch comes back short.
// It returns the count of rows it marked.
func (s *FileService) trashChildren(ctx context.Context, userID int64, rootIDs, parents []int64) (int64, error) {
	var total int64
	for {
		var marked int64
		err := utils.InTx(ctx, s.db, func(tx *sql.Tx) error {
			q := s.queries.WithTx(tx)
			if err := q.LockUserTree(ctx, userID); err != nil {
				return err
			}
			var err error
			marked, err = q.TrashChildren(ctx, database.TrashChildrenParams{
				UserID:    userID,
				ParentIds: parents,
				RootIds:   rootIDs,
				BatchSize: trashBatchSize,
			})
			return err
		})
		total += marked
		if err != nil || marked < trashBatchSize {
			return total, err
		}
	}
}

// moveNestedRootsHome moves the items deleted earlier out of the groups, one batch per transaction, until a batch comes back short.
// It returns the count of items it moved.
func (s *FileService) moveNestedRootsHome(ctx context.Context, userID int64, rootIDs []int64) (int64, error) {
	var total int64
	for {
		var moved int64
		err := utils.InTx(ctx, s.db, func(tx *sql.Tx) error {
			q := s.queries.WithTx(tx)
			if err := q.LockUserTree(ctx, userID); err != nil {
				return err
			}
			var err error
			moved, err = q.MoveNestedTrashRootsHome(ctx, database.MoveNestedTrashRootsHomeParams{
				UserID:    userID,
				RootIds:   rootIDs,
				BatchSize: purgeBatchSize,
			})
			return err
		})
		total += moved
		if err != nil || moved < purgeBatchSize {
			return total, err
		}
	}
}

// requestExpiredPurges marks the user's expired items and adds one purge job per batch, until a batch comes back short.
func (s *FileService) requestExpiredPurges(ctx context.Context, userID int64) error {
	for {
		var marked []int64
		err := utils.InTx(ctx, s.db, func(tx *sql.Tx) error {
			q := s.queries.WithTx(tx)
			if err := q.LockUserTree(ctx, userID); err != nil {
				return err
			}
			var err error
			marked, err = q.RequestExpiredPurge(ctx, database.RequestExpiredPurgeParams{
				UserID:        userID,
				RetentionDays: trashRetentionDays,
				BatchSize:     purgeBatchSize,
			})
			if err != nil || len(marked) == 0 {
				return err
			}
			err = q.LogItems(ctx, database.LogItemsParams{Event: string(EventExpired), Extra: noExtra, UserID: userID, Ids: marked})
			if err != nil {
				return err
			}
			_, err = s.queue.InsertTx(ctx, tx, jobs.PurgeGroupsArgs{UserID: userID, RootIDs: marked}, nil)
			return err
		})
		if err != nil {
			return err
		}
		if len(marked) > 0 {
			s.logger.Info("marked expired trash for purge", zap.Int64("user_id", userID), zap.Int64s("root_ids", marked))
		}
		if len(marked) < purgeBatchSize {
			return nil
		}
	}
}

// deleteGroupRows deletes the groups from the bottom up, one batch of rows with nothing under them at a time.
// It removes the bytes before the rows, so a crash leaves rows the next run deletes, never bytes with no row.
// It returns the count of rows it deleted and of files it removed from disk.
func (s *FileService) deleteGroupRows(ctx context.Context, userID int64, rootIDs []int64) (int64, int64, error) {
	var rows, files int64
	for {
		leaves, err := s.queries.ListPurgeLeaves(ctx, database.ListPurgeLeavesParams{
			UserID:    userID,
			RootIds:   rootIDs,
			BatchSize: purgeBatchSize,
		})
		if err != nil || len(leaves) == 0 {
			return rows, files, err
		}

		ids := make([]int64, len(leaves))
		for i, leaf := range leaves {
			ids[i] = leaf.ID
			if !leaf.StorageKey.Valid {
				continue
			}
			if err := utils.RemoveStoredFile(s.cfg.Dir, leaf.StorageKey.String); err != nil {
				return rows, files, fmt.Errorf("failed to remove stored file: %w", err)
			}
			files++
		}

		var deleted int64
		err = utils.InTx(ctx, s.db, func(tx *sql.Tx) error {
			q := s.queries.WithTx(tx)
			if err := q.LockUserTree(ctx, userID); err != nil {
				return err
			}
			var err error
			deleted, err = q.DeletePurgeRows(ctx, database.DeletePurgeRowsParams{UserID: userID, Ids: ids, RootIds: rootIDs})
			return err
		})
		if err != nil {
			return rows, files, err
		}
		rows += deleted
	}
}

// copyChildren copies the rows under the sources of the copy folders, one batch at a time, until a batch comes back
// short. The bytes of a batch go first, then its rows in one short transaction. So a crash leaves bytes with no row,
// which the orphan sweep removes, never a row with no bytes. It returns the count of rows it added, of files and of bytes.
func (s *FileService) copyChildren(ctx context.Context, userID int64, copyIDs []int64) (int64, int64, int64, error) {
	var rows, files, size int64
	// Not nil: pq sends a nil slice as NULL, and "<> ALL(NULL)" matches no row.
	skip := []int64{}
	for {
		batch, err := s.queries.ListUncopiedChildren(ctx, database.ListUncopiedChildrenParams{
			UserID:    userID,
			CopyIds:   copyIDs,
			SkipIds:   skip,
			BatchSize: copyBatchSize,
		})
		if err != nil || len(batch) == 0 {
			return rows, files, size, err
		}

		add := database.InsertCopiesParams{UserID: userID}
		sizes := make(map[string]int64)
		for _, row := range batch {
			key := ""
			if row.StorageKey.Valid {
				if key, err = utils.NewStorageKey(); err != nil {
					break
				}
				var n int64
				n, err = utils.CopyStoredFile(s.cfg.Dir, row.StorageKey.String, key)
				if errors.Is(err, utils.ErrNoStoredFile) {
					s.logger.Warn("skipped a file with no bytes", zap.Int64("user_id", userID), zap.Int64("source_id", row.ID))
					skip = append(skip, row.ID)
					err = nil
					continue
				}
				if err != nil {
					break
				}
				sizes[key] = n
			}
			add.ParentIds = append(add.ParentIds, row.CopyParentID)
			add.Names = append(add.Names, row.Name)
			add.Keys = append(add.Keys, key)
			add.ContentTypes = append(add.ContentTypes, row.ContentType)
			add.Sizes = append(add.Sizes, row.SizeBytes)
			add.SourceIds = append(add.SourceIds, row.ID)
		}

		var added []sql.NullString
		if err == nil && len(add.SourceIds) > 0 {
			added, err = s.insertCopies(ctx, add)
		}
		// Bytes whose row did not go in have no use: the copy folder went to the trash, or the batch failed.
		for _, key := range added {
			if key.Valid {
				files++
				size += sizes[key.String]
				delete(sizes, key.String)
			}
		}
		for key := range sizes {
			_ = utils.RemoveStoredFile(s.cfg.Dir, key)
		}
		rows += int64(len(added))
		if err != nil || len(batch) < copyBatchSize {
			return rows, files, size, err
		}
	}
}

// insertCopies adds one batch of copies in a short transaction with the tree lock. Only the copy job adds rows to a
// copy folder, so a name in use there comes from a rename in the source during the copy: b.txt renamed to a.txt after
// a.txt was copied. Such a row gets a free name, the way restore does. It returns the storage keys of the rows it added.
func (s *FileService) insertCopies(ctx context.Context, add database.InsertCopiesParams) ([]sql.NullString, error) {
	var added []sql.NullString
	err := utils.InTx(ctx, s.db, func(tx *sql.Tx) error {
		q := s.queries.WithTx(tx)
		if err := q.LockUserTree(ctx, add.UserID); err != nil {
			return err
		}
		// Each folder once: the index lookups are folders times names.
		inUse, err := q.ListTakenNamesInFolders(ctx, database.ListTakenNamesInFoldersParams{
			UserID:    add.UserID,
			ParentIds: slices.Compact(slices.Sorted(slices.Values(add.ParentIds))),
			Names:     add.Names,
		})
		if err != nil {
			return err
		}
		type folderName struct {
			parentID int64
			name     string
		}
		clashes := make(map[folderName]bool, len(inUse))
		for _, row := range inUse {
			clashes[folderName{row.ParentID, row.Name}] = true
		}

		var renames []int
		for i := range add.Names {
			if clashes[folderName{add.ParentIds[i], add.Names[i]}] {
				renames = append(renames, i)
			}
		}
		if len(renames) > 0 {
			parents := make([]int64, len(renames))
			prefixes := make([]string, len(renames))
			for j, i := range renames {
				parents[j] = add.ParentIds[i]
				prefixes[j], _ = utils.SplitName(add.Names[i], utils.NameKindOf(add.Keys[i] == ""))
			}
			rows, err := q.ListNamesByParentsAndPrefixes(ctx, database.ListNamesByParentsAndPrefixesParams{
				UserID:    add.UserID,
				ParentIds: parents,
				Prefixes:  prefixes,
			})
			if err != nil {
				return err
			}
			// The batch's own names are taken too, so a new name does not clash with a row added next to it.
			taken := make(map[int64][]string)
			for _, row := range rows {
				taken[row.ParentID] = append(taken[row.ParentID], row.Name)
			}
			for i, name := range add.Names {
				taken[add.ParentIds[i]] = append(taken[add.ParentIds[i]], name)
			}
			for _, i := range renames {
				name, err := utils.FreeName(add.Names[i], utils.NameKindOf(add.Keys[i] == ""), taken[add.ParentIds[i]])
				if err != nil {
					return err
				}
				taken[add.ParentIds[i]] = append(taken[add.ParentIds[i]], name)
				add.Names[i] = name
			}
		}

		added, err = q.InsertCopies(ctx, add)
		return err
	})
	return added, err
}

// zipName names a zip after its one item, else after the folder the items are in. Items from more than one folder
// (only through the API), or a folder gone since, give "Download.zip".
func (s *FileService) zipName(ctx context.Context, userID int64, items []database.File) (string, error) {
	if len(items) == 1 {
		return items[0].Name + ".zip", nil
	}
	parentID := items[0].ParentID
	for _, item := range items[1:] {
		if item.ParentID != parentID {
			return "Download.zip", nil
		}
	}
	if !parentID.Valid {
		return "Home.zip", nil
	}

	parent, err := s.queries.GetFile(ctx, database.GetFileParams{ID: parentID.Int64, UserID: userID})
	if errors.Is(err, sql.ErrNoRows) {
		return "Download.zip", nil
	}
	if err != nil {
		return "", err
	}
	return parent.Name + ".zip", nil
}

// writeZip writes the live items in ids, then the folders under them one level at a time. It returns the count of
// files and of folders it wrote.
func (s *FileService) writeZip(ctx context.Context, zb *utils.ZipBuilder, userID int64, ids []int64) (int64, int64, error) {
	items, err := s.queries.ListLiveItems(ctx, database.ListLiveItemsParams{UserID: userID, Ids: ids})
	if err != nil {
		return 0, 0, err
	}

	var files, folders int64
	level, err := s.addZipEntries(ctx, zb, userID, "", items, make(map[string]bool), &files)
	for len(level) > 0 && err == nil {
		folders += int64(len(level))
		var next []zipFolder
		for _, folder := range level {
			var found []zipFolder
			if found, err = s.addZipFolder(ctx, zb, userID, folder, &files); err != nil {
				break
			}
			next = append(next, found...)
		}
		level = next
	}
	return files, folders, err
}

// addZipFolder writes the rows of one folder through the list query, a page at a time, so no query reads a whole big
// folder. It returns the folders in it.
func (s *FileService) addZipFolder(ctx context.Context, zb *utils.ZipBuilder, userID int64, folder zipFolder, files *int64) ([]zipFolder, error) {
	var found []zipFolder
	taken := make(map[string]bool)
	cursor := ""
	for {
		page, err := utils.FetchPage(ctx, s.db, fileList, utils.PageQuery[SortField]{
			// A zip keeps the hidden items, the way zip keeps dot files.
			Args:     []any{userID, folder.id, "", true},
			Sort:     SortByName,
			Cursor:   cursor,
			PageSize: zipPageSize,
		}, fileFields)
		if err != nil {
			return nil, err
		}
		more, err := s.addZipEntries(ctx, zb, userID, folder.path+"/", page.Data, taken, files)
		if err != nil {
			return nil, err
		}
		found = append(found, more...)
		if page.NextCursor == nil {
			return found, nil
		}
		cursor = *page.NextCursor
	}
}

// addZipEntries writes rows into the zip folder at prefix and returns the folders among them. taken holds the names
// the zip has in that folder. Names in one folder are unique, but a rename during the build can repeat one, and the
// repeat gets a free name. A copying item is left out with all under it. A file with no bytes is logged and left out.
func (s *FileService) addZipEntries(
	ctx context.Context,
	zb *utils.ZipBuilder,
	userID int64,
	prefix string,
	rows []database.File,
	taken map[string]bool,
	files *int64,
) ([]zipFolder, error) {
	var found []zipFolder
	for _, row := range rows {
		if row.CopyRequestedAt.Valid {
			continue
		}
		isFolder := !row.StorageKey.Valid
		name := row.Name
		if taken[name] {
			var err error
			if name, err = utils.FreeName(name, utils.NameKindOf(isFolder), slices.Collect(maps.Keys(taken))); err != nil {
				return nil, err
			}
		}
		taken[name] = true
		path := prefix + name

		if isFolder {
			if err := zb.AddFolder(path, row.CreatedAt); err != nil {
				return nil, err
			}
			found = append(found, zipFolder{id: row.ID, path: path})
			continue
		}
		_, err := zb.AddStoredFile(ctx, path, row.StorageKey.String, row.CreatedAt)
		if errors.Is(err, utils.ErrNoStoredFile) {
			s.logger.Warn("left a file with no bytes out of a zip", zap.Int64("user_id", userID), zap.Int64("file_id", row.ID))
			continue
		}
		if err != nil {
			return nil, err
		}
		*files++
	}
	return found, nil
}

// lockPasteItems runs the checks that Move and Copy share, after the tree lock: the items are live, the target is a
// live folder outside them, no item is in it already, and no name is in use in it. intoItself is the error for a
// target inside one of the items. It returns the items in id order.
func (s *FileService) lockPasteItems(
	ctx context.Context,
	q *database.Queries,
	userID, parentID int64,
	ids []int64,
	intoItself error,
) ([]database.LockLiveItemsRow, error) {
	items, err := q.LockLiveItems(ctx, database.LockLiveItemsParams{UserID: userID, Ids: ids})
	if err != nil {
		return nil, err
	}
	if len(items) != len(ids) {
		return nil, errItemNotFound
	}

	if parentID != 0 {
		path, err := q.ListFolderPathIDs(ctx, database.ListFolderPathIDsParams{ID: parentID, UserID: userID})
		if err != nil {
			return nil, err
		}
		if len(path) == 0 {
			return nil, errFolderNotFound
		}
		if slices.ContainsFunc(path, func(id int64) bool { return slices.Contains(ids, id) }) {
			return nil, intoItself
		}
	}

	names := make([]string, len(items))
	for i, item := range items {
		if item.ParentID == parentID {
			return nil, errSameFolder
		}
		names[i] = item.Name
	}
	taken, err := q.ListTakenNames(ctx, database.ListTakenNamesParams{UserID: userID, ParentID: parentID, Names: names})
	if err != nil {
		return nil, err
	}
	if len(taken) > 0 {
		return nil, apperr.New(apperr.KindConflict, taken[0]+" already exists in this folder")
	}
	return items, nil
}

func (s *FileService) checkFolder(ctx context.Context, q *database.Queries, userID, folderID int64) error {
	if folderID == 0 {
		return nil
	}

	exists, err := q.FolderExists(ctx, database.FolderExistsParams{ID: folderID, UserID: userID})
	if err != nil {
		return err
	}
	if !exists {
		return errFolderNotFound
	}
	return nil
}

func (s *FileService) insertWithFreeName(
	ctx context.Context,
	userID, parentID int64,
	name string,
	kind utils.NameKind,
	event ActivityEvent,
	insert func(q *database.Queries, freeName string) (database.File, error),
) (database.File, error) {
	base, _ := utils.SplitName(name, kind)

	for range maxNameAttempts {
		var dbFile database.File
		err := utils.InTx(ctx, s.db, func(tx *sql.Tx) error {
			q := s.queries.WithTx(tx)
			if err := q.LockUserTree(ctx, userID); err != nil {
				return err
			}
			if err := s.checkFolder(ctx, q, userID, parentID); err != nil {
				return err
			}

			taken, err := q.ListNamesByParentAndPrefix(ctx, database.ListNamesByParentAndPrefixParams{
				UserID:   userID,
				ParentID: parentID,
				Prefix:   base,
			})
			if err != nil {
				return err
			}

			freeName, err := utils.FreeName(name, kind, taken)
			if err != nil {
				return err
			}

			if dbFile, err = insert(q, freeName); err != nil {
				return err
			}
			return q.LogItems(ctx, database.LogItemsParams{Event: string(event), Extra: noExtra, UserID: userID, Ids: []int64{dbFile.ID}})
		})
		if utils.IsUniqueViolation(err, nameIndex) {
			continue
		}
		return dbFile, err
	}

	return database.File{}, errNameTaken
}
