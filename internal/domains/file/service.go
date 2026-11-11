package file

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"gost/config"
	"gost/internal/apperr"
	"gost/internal/database"
	"gost/internal/utils"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

const maxNameLength = 255

var (
	errNameRequired = apperr.New(apperr.KindValidation, "file name is required")
	errNameInvalid  = apperr.New(apperr.KindValidation, "file name has invalid characters")
	errNameTooLong  = apperr.New(apperr.KindValidation, "file name is too long")
	errEmptyFile    = apperr.New(apperr.KindValidation, "file is empty")
)

type FileServiceInterface interface {
	Upload(ctx context.Context, userID int64, name string, src io.Reader) (*File, error)
	List(ctx context.Context, userID int64, page, pageSize int) (*PaginatedFiles, error)
}

var _ FileServiceInterface = (*FileService)(nil)

type FileService struct {
	queries *database.Queries
	cfg     config.UploadConfig
}

func NewFileService(queries *database.Queries, cfg config.UploadConfig) *FileService {
	return &FileService{
		queries: queries,
		cfg:     cfg,
	}
}

func toFileModel(dbFile database.File) *File {
	return &File{
		ID:          dbFile.ID,
		Name:        dbFile.Name,
		ContentType: dbFile.ContentType,
		SizeBytes:   dbFile.SizeBytes,
		CreatedAt:   dbFile.CreatedAt,
	}
}

func (s *FileService) Upload(ctx context.Context, userID int64, name string, src io.Reader) (*File, error) {
	if name == "" {
		return nil, errNameRequired
	}
	if !utf8.ValidString(name) || strings.ContainsRune(name, 0) {
		return nil, errNameInvalid
	}
	if utf8.RuneCountInString(name) > maxNameLength {
		return nil, errNameTooLong
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

	dbFile, err := s.queries.CreateFile(ctx, database.CreateFileParams{
		UserID:      userID,
		Name:        name,
		StorageKey:  key,
		ContentType: http.DetectContentType(head),
		SizeBytes:   size,
	})
	if err != nil {
		_ = utils.RemoveStoredFile(s.cfg.Dir, key)
		return nil, err
	}

	return toFileModel(dbFile), nil
}

func (s *FileService) List(ctx context.Context, userID int64, page, pageSize int) (*PaginatedFiles, error) {
	p := utils.NewPagination(page, pageSize)

	dbFiles, err := s.queries.ListFilesByUserPaginated(ctx, database.ListFilesByUserPaginatedParams{
		UserID: userID,
		Limit:  p.Limit,
		Offset: p.Offset,
	})
	if err != nil {
		return nil, err
	}

	total, err := s.queries.CountFilesByUser(ctx, userID)
	if err != nil {
		return nil, err
	}

	files := make([]*File, len(dbFiles))
	for i, dbFile := range dbFiles {
		files[i] = toFileModel(dbFile)
	}

	return &PaginatedFiles{
		Data:       files,
		Page:       p.Page,
		PageSize:   p.PageSize,
		Total:      int(total),
		TotalPages: p.TotalPages(total),
	}, nil
}
