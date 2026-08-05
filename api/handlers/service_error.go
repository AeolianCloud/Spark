package handlers

import (
	"errors"
	"net/http"

	"spark/service"
)

// mapServiceError converts a service-layer error into the unified *APIError,
// or returns it unchanged when it is not a service error — the Handler
// wrapper then logs it and answers a generic 500 so internals never leak.
func mapServiceError(err error) error {
	var serr *service.Error
	if !errors.As(err, &serr) {
		return err
	}
	switch serr.Kind {
	case service.KindBadRequest:
		return NewError(http.StatusBadRequest, CodeBadRequest, serr.Message)
	case service.KindNotFound:
		return NewError(http.StatusNotFound, CodeNotFound, serr.Message)
	case service.KindConflict:
		return NewError(http.StatusConflict, CodeConflict, serr.Message)
	default:
		return err
	}
}
