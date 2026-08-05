package handlers

import (
	"errors"
	"net/http"

	"spark/service"
)

// mapServiceError 将 service 层错误转换为统一的 *APIError；
// 若非 service 错误则原样返回 —— Handler 包装器随后记录它并返回
// 通用 500，使内部细节永不泄露。
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
