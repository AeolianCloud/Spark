package service

import "fmt"

// ErrorKind classifies service-layer failures so handlers can map them onto
// the unified API error contract without importing the api package (which
// would create an import cycle).
type ErrorKind int

const (
	// KindBadRequest: caller-supplied data failed validation.
	KindBadRequest ErrorKind = iota
	// KindNotFound: the referenced resource does not exist.
	KindNotFound
	// KindConflict: the operation clashes with existing state (duplicate
	// name, resource still in use).
	KindConflict
)

// Error is a service-layer error carrying a kind and a message that is safe
// to surface to API clients.
type Error struct {
	Kind    ErrorKind
	Message string
}

func (e *Error) Error() string { return e.Message }

func badRequestf(format string, args ...any) *Error {
	return &Error{Kind: KindBadRequest, Message: fmt.Sprintf(format, args...)}
}

func notFoundf(format string, args ...any) *Error {
	return &Error{Kind: KindNotFound, Message: fmt.Sprintf(format, args...)}
}

func conflictf(format string, args ...any) *Error {
	return &Error{Kind: KindConflict, Message: fmt.Sprintf(format, args...)}
}
