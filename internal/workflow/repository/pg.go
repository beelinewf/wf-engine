package repository

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// isUniqueViolation reports whether err is a PostgreSQL unique-violation
// (SQLSTATE 23505), used to translate version races into conflicts.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isForeignKeyViolation reports whether err is a PostgreSQL
// foreign-key-violation (SQLSTATE 23503), used to translate references to
// unknown rows (a role that does not exist) into invalid input.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
