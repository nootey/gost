package utils

import (
	"context"
	"database/sql"
	"errors"

	"github.com/lib/pq"
)

const pqUniqueViolation = "23505"

// IsUniqueViolation reports whether err is a Postgres unique violation on the named constraint or index.
func IsUniqueViolation(err error, constraint string) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == pqUniqueViolation && pqErr.Constraint == constraint
}

// Class 22 holds the errors for values that do not fit their type, such as "abc" for a bigint.
func isDataException(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code.Class() == "22"
}

// InTx runs fn in a transaction. It commits when fn returns nil, else it rolls back.
func InTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
