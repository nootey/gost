package file

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"gost/config"
	"gost/internal/apperr"
	"gost/internal/database"
	"gost/internal/jobs"
	"gost/internal/utils"
	"io"
	"net/http"
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

var (
	errEmptyFile      = apperr.New(apperr.KindValidation, "file is empty")
	errFolderNotFound = apperr.New(apperr.KindNotFound, "folder not found")
	errItemNotFound   = apperr.New(apperr.KindNotFound, "item not found")
	errNameTaken      = apperr.New(apperr.KindConflict, "an item with this name already exists")
	errMoveIntoItself = apperr.New(apperr.KindValidation, "a folder cannot move into itself or its own subfolder")
	errSameFolder     = apperr.New(apperr.KindValidation, "the items are already in this folder")
)

// A parentID of 0 means the root folder.
type FileServiceInterface interface {
	Upload(ctx context.Context, userID, parentID int64, name string, src io.Reader) (*File, error)
	CreateFolder(ctx context.Context, userID, parentID int64, name string) (*File, error)
	List(ctx context.Context, userID int64, q ListQuery) (*utils.Page[*File], error)
	ResolveFolder(ctx context.Context, userID int64, names []string) (*FolderPath, error)
	Rename(ctx context.Context, userID, id int64, name string) (*File, error)
	Move(ctx context.Context, userID, parentID int64, ids []int64) error
	Trash(ctx context.Context, userID int64, ids []int64) error
	Restore(ctx context.Context, userID int64, ids []int64) error
	Purge(ctx context.Context, userID int64, ids []int64) error
	ListTrash(ctx context.Context, userID int64, q TrashQuery) (*utils.Page[*TrashedFile], error)
}

var _ FileServiceInterface = (*FileService)(nil)

type FileService struct {
	queries *database.Queries
	db      *sql.DB
	queue   jobs.InserterInterface
	cfg     config.UploadConfig
	logger  *zap.Logger
}

func NewFileService(queries *database.Queries, db *sql.DB, queue jobs.InserterInterface, cfg config.UploadConfig, logger *zap.Logger) *FileService {
	return &FileService{
		queries: queries,
		db:      db,
		queue:   queue,
		cfg:     cfg,
		logger:  logger,
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
	}
}

// fileFields follows the column order of the files table, which SELECT * returns.
func fileFields(f *database.File) []any {
	return []any{
		&f.ID, &f.UserID, &f.Name, &f.StorageKey, &f.ContentType, &f.SizeBytes,
		&f.CreatedAt, &f.UpdatedAt, &f.ParentID, &f.TrashedAt, &f.TrashRootID, &f.PurgeRequestedAt,
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

	dbFile, err := s.insertWithFreeName(ctx, userID, parentID, name, utils.FileName, func(q *database.Queries, freeName string) (database.File, error) {
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

	dbFile, err := s.insertWithFreeName(ctx, userID, parentID, name, utils.FolderName, func(q *database.Queries, freeName string) (database.File, error) {
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
		Args:     []any{userID, q.ParentID, strings.TrimSpace(q.Search)},
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

	dbFile, err = s.queries.RenameFile(ctx, database.RenameFileParams{ID: id, UserID: userID, Name: name})
	if utils.IsUniqueViolation(err, nameIndex) {
		return nil, errNameTaken
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errItemNotFound
	}
	if err != nil {
		return nil, err
	}

	return toFileModel(dbFile), nil
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
		items, err := q.LockLiveItems(ctx, database.LockLiveItemsParams{UserID: userID, Ids: ids})
		if err != nil {
			return err
		}
		if len(items) != len(ids) {
			return errItemNotFound
		}

		if parentID != 0 {
			path, err := q.ListFolderPathIDs(ctx, database.ListFolderPathIDsParams{ID: parentID, UserID: userID})
			if err != nil {
				return err
			}
			if len(path) == 0 {
				return errFolderNotFound
			}
			if slices.ContainsFunc(path, func(id int64) bool { return slices.Contains(ids, id) }) {
				return errMoveIntoItself
			}
		}

		names := make([]string, len(items))
		for i, item := range items {
			if item.ParentID == parentID {
				return errSameFolder
			}
			names[i] = item.Name
		}
		taken, err := q.ListTakenNames(ctx, database.ListTakenNamesParams{UserID: userID, ParentID: parentID, Names: names})
		if err != nil {
			return err
		}
		if len(taken) > 0 {
			return apperr.New(apperr.KindConflict, taken[0]+" already exists in this folder")
		}

		return q.MoveItems(ctx, database.MoveItemsParams{ParentID: parentID, UserID: userID, Ids: ids})
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
			if item.IsFolder {
				folders = append(folders, item.ID)
			}
		}
		if len(folders) == 0 {
			return nil
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

			return q.RestoreTrashRoots(ctx, restored)
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
	for i, dbFile := range page.Data {
		items[i] = &TrashedFile{File: *toFileModel(dbFile), TrashedAt: dbFile.TrashedAt.Time}
	}

	return &utils.Page[*TrashedFile]{Data: items, NextCursor: page.NextCursor}, nil
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

			dbFile, err = insert(q, freeName)
			return err
		})
		if utils.IsUniqueViolation(err, nameIndex) {
			continue
		}
		return dbFile, err
	}

	return database.File{}, errNameTaken
}
