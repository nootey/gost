package utils

import (
	"errors"

	"github.com/lib/pq"
)

const pqUniqueViolation = "23505"

// IsUniqueViolation reports whether err is a Postgres unique violation on the named constraint or index.
func IsUniqueViolation(err error, constraint string) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == pqUniqueViolation && pqErr.Constraint == constraint
}
