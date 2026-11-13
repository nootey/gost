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
)

const nameIndex = "idx_files_user_parent_name"

const maxNameAttempts = 3

// trashBatchSize caps the rows one transaction of the trash job marks, so the job never holds the tree lock long.
const trashBatchSize = 1000

var (
	errEmptyFile      = apperr.New(apperr.KindValidation, "file is empty")
	errFolderNotFound = apperr.New(apperr.KindNotFound, "folder not found")
	errItemNotFound   = apperr.New(apperr.KindNotFound, "item not found")
	errNameTaken      = apperr.New(apperr.KindConflict, "an item with this name already exists")
)

// A parentID of 0 means the root folder.
type FileServiceInterface interface {
	Upload(ctx context.Context, userID, parentID int64, name string, src io.Reader) (*File, error)
	CreateFolder(ctx context.Context, userID, parentID int64, name string) (*File, error)
	List(ctx context.Context, userID int64, q ListQuery) (*utils.Page[*File], error)
	ResolveFolder(ctx context.Context, userID int64, names []string) (*FolderPath, error)
	Rename(ctx context.Context, userID, id int64, name string) (*File, error)
	Trash(ctx context.Context, userID int64, ids []int64) error
	Restore(ctx context.Context, userID int64, ids []int64) error
	ListTrash(ctx context.Context, userID int64, q TrashQuery) (*utils.Page[*TrashedFile], error)
}

var _ FileServiceInterface = (*FileService)(nil)

type FileService struct {
	queries *database.Queries
	db      *sql.DB
	queue   jobs.InserterInterface
	cfg     config.UploadConfig
}

func NewFileService(queries *database.Queries, db *sql.DB, queue jobs.InserterInterface, cfg config.UploadConfig) *FileService {
	return &FileService{
		queries: queries,
		db:      db,
		queue:   queue,
		cfg:     cfg,
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
		&f.CreatedAt, &f.UpdatedAt, &f.ParentID, &f.TrashedAt, &f.TrashRootID,
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
	parents := rootIDs
	for len(parents) > 0 {
		var next []int64
		for chunk := range slices.Chunk(parents, trashBatchSize) {
			if err := s.trashChildren(ctx, userID, rootIDs, chunk); err != nil {
				return err
			}
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
func (s *FileService) trashChildren(ctx context.Context, userID int64, rootIDs, parents []int64) error {
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
		if err != nil || marked < trashBatchSize {
			return err
		}
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
