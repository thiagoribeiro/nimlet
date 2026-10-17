package postgres

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	pgErrUniqueViolation     = "23505"
	pgErrForeignKeyViolation = "23503"
	pgErrInsufficientPriv    = "42501"
)

// isInsufficientPrivilege reports a permission error from the database role — e.g. the
// server's runtime role trying an operator-only write.
func isInsufficientPrivilege(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgErrInsufficientPriv
}

func isUniqueViolation(err error) bool {
	if pgErr, ok := err.(*pgconn.PgError); ok {
		return pgErr.Code == pgErrUniqueViolation
	}
	return false
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgErrForeignKeyViolation
}

func wrapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("postgres: %s: %w", op, err)
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
