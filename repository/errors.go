package repository

import "errors"

// Sentinel errors shared by repository implementations. Services map these
// (and pgx.ErrNoRows) onto API errors.
var (
	// ErrConflict is returned when an insert or update would violate a
	// uniqueness constraint (PostgreSQL SQLSTATE 23505).
	ErrConflict = errors.New("repository: unique constraint violation")
	// ErrInUse is returned when an operation is refused because other rows
	// still reference the target row: a delete (or insert/update) that
	// violates a foreign key (PostgreSQL SQLSTATE 23503).
	ErrInUse = errors.New("repository: resource still in use")
	// ErrSpecConflict is returned when an optimistic-lock update touched no
	// row: the spec was concurrently modified (or the row deleted) between
	// the caller's read and the write.
	ErrSpecConflict = errors.New("repository: vm spec was concurrently modified")
)
