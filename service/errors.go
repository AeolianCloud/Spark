package service

import "fmt"

// maxPVEErrorLen 限制 PVE 错误摘要对外呈现的最大字符数（rune）。PVE 错误体
// 最大可达 1MiB（pve 包 maxResponseSize），脱敏后若不截断，超长错误体可能
// 原样进入对外错误消息（详情 503、列表 warnings、节点状态降级等）——违反
// "对外错误消息不得暴露内部细节"红线且放大响应体；500 字符足以承载脱敏
// 摘要与节点名。
const maxPVEErrorLen = 500

// truncatePVEErrorMsg 按 maxPVEErrorLen 以 rune 边界截断错误消息（先脱敏
// 后截断，与 sanitizeOperationError 的"按 rune 切割"风格一致）：多字节
// UTF-8 字符绝不会被切成非法序列。消息短于上限时原样返回。
func truncatePVEErrorMsg(msg string) string {
	r := []rune(msg)
	if len(r) > maxPVEErrorLen {
		return string(r[:maxPVEErrorLen])
	}
	return msg
}

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
