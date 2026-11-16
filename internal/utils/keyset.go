package utils

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"gost/internal/apperr"
	"slices"
	"strconv"
	"strings"
)

const (
	defaultPageSize = 25
	maxPageSize     = 100
)

var errInvalidCursor = apperr.New(apperr.KindInvalid, "invalid cursor")

var (
	sortDirection = map[bool]string{false: " ASC", true: " DESC"}
	pastCursor    = map[bool]string{false: " > ", true: " < "}
)

// Page is one page of a list. NextCursor is nil on the last page.
type Page[T any] struct {
	Data       []T     `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

// PointerPage turns a page of values into a page of pointers to them, the shape the services send.
func PointerPage[T any](page *Page[T]) *Page[*T] {
	list := make([]*T, len(page.Data))
	for i := range page.Data {
		list[i] = &page.Data[i]
	}
	return &Page[*T]{Data: list, NextCursor: page.NextCursor}
}

// KeysetList is a list that pages by cursor. sqlc needs a fixed ORDER BY, so this SQL is built at runtime.
type KeysetList[S ~string] struct {
	Select string
	From   string
	// Where filters the list. $1, $2, ... are the PageQuery args.
	Where string
	// Sorts maps each sort field to its SQL keys. No key may be null, and the last key must be unique, such as id.
	// Every key sorts in the same direction, so one index per field serves both directions.
	Sorts map[S][]string
	// Parts split the list, such as folders then files. Each part is its own query, in this order.
	Parts []string
}

// Split returns a copy of the list in parts. Each split divides every part of the splits before it, so two splits of
// two give four parts, such as favorite folders, favorite files, other folders, other files. A nil split is skipped.
// A cursor keeps only its part's index, so a list must page with the same splits from start to end.
func (l KeysetList[S]) Split(splits ...[]string) KeysetList[S] {
	parts := []string{""}
	for _, split := range splits {
		if len(split) == 0 {
			continue
		}
		next := make([]string, 0, len(parts)*len(split))
		for _, part := range parts {
			for _, cond := range split {
				if part == "" {
					next = append(next, cond)
				} else {
					next = append(next, "("+part+") AND ("+cond+")")
				}
			}
		}
		parts = next
	}
	l.Parts = parts
	return l
}

type PageQuery[S ~string] struct {
	Args     []any
	Sort     S
	Desc     bool
	Cursor   string
	PageSize int
}

// The values are the last row's keys as Postgres prints them, so Postgres parses them back as the key types.
type pageCursor struct {
	Sort   string   `json:"s"`
	Desc   bool     `json:"d,omitempty"`
	Part   int      `json:"p,omitempty"`
	Values []string `json:"v"`
}

// FetchPage reads one page of list. fields gives the scan targets of one row, in the order of list.Select.
func FetchPage[T any, S ~string](ctx context.Context, db *sql.DB, list KeysetList[S], q PageQuery[S], fields func(*T) []any) (*Page[T], error) {
	keys, ok := list.Sorts[q.Sort]
	if !ok {
		return nil, fmt.Errorf("unknown sort %q", q.Sort)
	}

	parts := list.Parts
	if len(parts) == 0 {
		parts = []string{""}
	}

	start := pageCursor{Sort: string(q.Sort), Desc: q.Desc}
	if q.Cursor != "" {
		c, err := decodeCursor(q.Cursor)
		if err != nil || c.Sort != start.Sort || c.Desc != start.Desc || c.Part < 0 || c.Part >= len(parts) || len(c.Values) != len(keys) {
			return nil, errInvalidCursor
		}
		start = c
	}

	pageSize := q.PageSize
	if pageSize < 1 || pageSize > maxPageSize {
		pageSize = defaultPageSize
	}

	page := &Page[T]{Data: []T{}}
	last := start
	for part := start.Part; part < len(parts); part++ {
		var after []string
		if part == start.Part {
			after = start.Values
		}
		// One row past the page tells if a next page exists. A full page still asks the next part for one row.
		query, args := keysetQuery(list, keys, q, parts[part], after, pageSize-len(page.Data)+1)

		full, err := func() (bool, error) {
			rows, err := db.QueryContext(ctx, query, args...)
			if err != nil {
				return false, err
			}
			defer func() { _ = rows.Close() }()

			for rows.Next() {
				if len(page.Data) == pageSize {
					return true, nil
				}

				var row T
				values := make([]string, len(keys))
				dest := fields(&row)
				for i := range values {
					dest = append(dest, &values[i])
				}
				if err := rows.Scan(dest...); err != nil {
					return false, err
				}

				page.Data = append(page.Data, row)
				last = pageCursor{Sort: start.Sort, Desc: start.Desc, Part: part, Values: values}
			}
			return false, rows.Err()
		}()
		// Only the key types know which cursor values are valid, so a tampered cursor shows as a Postgres data error.
		if after != nil && isDataException(err) {
			return nil, apperr.Wrap(apperr.KindInvalid, errInvalidCursor.Message, err)
		}
		if err != nil {
			return nil, err
		}

		if full {
			page.NextCursor = encodeCursor(last)
			break
		}
	}

	return page, nil
}

func keysetQuery[S ~string](list KeysetList[S], keys []string, q PageQuery[S], part string, after []string, limit int) (string, []any) {
	// The aliases keep ORDER BY clear: a plain (created_at)::text would also be named created_at.
	columns := []string{list.Select}
	for i, k := range keys {
		columns = append(columns, "("+k+")::text AS cursor_"+strconv.Itoa(i))
	}

	args := slices.Clip(q.Args)
	conds := []string{list.Where, part}
	if after != nil {
		marks := make([]string, len(after))
		for i, v := range after {
			args = append(args, v)
			marks[i] = "$" + strconv.Itoa(len(args))
		}
		conds = append(conds, "("+strings.Join(keys, ", ")+")"+pastCursor[q.Desc]+"("+strings.Join(marks, ", ")+")")
	}

	var where []string
	for _, c := range conds {
		if c != "" {
			where = append(where, "("+c+")")
		}
	}

	order := make([]string, len(keys))
	for i, k := range keys {
		order[i] = k + sortDirection[q.Desc]
	}

	query := "SELECT " + strings.Join(columns, ", ") + " FROM " + list.From
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	return query + " ORDER BY " + strings.Join(order, ", ") + " LIMIT " + strconv.Itoa(limit), args
}

func encodeCursor(c pageCursor) *string {
	raw, _ := json.Marshal(c)
	cursor := base64.RawURLEncoding.EncodeToString(raw)
	return &cursor
}

func decodeCursor(s string) (pageCursor, error) {
	var c pageCursor
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(raw, &c)
}
