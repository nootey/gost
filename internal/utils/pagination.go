package utils

import "math"

const (
	defaultPageSize = 20
	maxPageSize     = 100
)

type Pagination struct {
	Page     int
	PageSize int
	Limit    int32
	Offset   int32
}

func NewPagination(page, pageSize int) Pagination {
	if pageSize < 1 || pageSize > maxPageSize {
		pageSize = defaultPageSize
	}
	page = min(max(page, 1), math.MaxInt32/pageSize)

	return Pagination{
		Page:     page,
		PageSize: pageSize,
		Limit:    int32(pageSize),              //nolint:gosec // pageSize is at most maxPageSize
		Offset:   int32((page - 1) * pageSize), //nolint:gosec // page is capped above so the offset fits
	}
}

func (p Pagination) TotalPages(total int64) int {
	return (int(total) + p.PageSize - 1) / p.PageSize
}
