package service

import "fmt"

// ErrorKind 对服务层失败进行分类，使处理器无需导入 api 包即可将它们映射到
// 统一的 API 错误契约（导入 api 包会导致循环依赖）。
type ErrorKind int

const (
	// KindBadRequest：调用方提供的数据未通过校验。
	KindBadRequest ErrorKind = iota
	// KindNotFound：被引用的资源不存在。
	KindNotFound
	// KindConflict：操作与现有状态冲突（名称重复、资源仍在使用中）。
	KindConflict
)

// Error 是携带 kind 和 message 的服务层错误，其消息可安全地展示给 API 客户端。
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
