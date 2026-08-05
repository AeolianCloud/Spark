package handlers

import (
	"strconv"

	"github.com/gin-gonic/gin"
)

// XTotalCountHeader 是分页列表端点的响应头
// （GET /zones、/ip-pools、/storage-types、/images、/vms）：应用
// limit/offset 之前匹配查询的条目总数。它让客户端无需二次请求即可
// 渲染分页控件。
const XTotalCountHeader = "X-Total-Count"

const (
	// defaultPageLimit 是客户端未发送 limit 时的分页大小。
	defaultPageLimit = 25
	// maxPageLimit 限制分页大小：过大的 limit 被截断为
	// 该值而不是被拒绝。该上限是 DoS 防护（一次分页查询最多返回
	// 这么多行，对 /vms 最多执行这么多次 PVE 合并），
	// 截断而非报错让异常客户端仍走可用路径，而不是可能不会重试的 400。
	maxPageLimit = 100
)

// parsePagination 读取分页列表端点共享的 limit/offset 查询参数。
// limit 默认为 defaultPageLimit，过大的 limit 上限为 maxPageLimit，
// offset 默认为 0；负数或非数值被拒绝并返回 400 bad_request。
// 返回的 limit/offset 始终非负。
func parsePagination(c *gin.Context) (limit, offset int, err error) {
	limit = defaultPageLimit
	if raw := c.Query("limit"); raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n < 0 {
			return 0, 0, ErrBadRequest("invalid limit query parameter")
		}
		if n > maxPageLimit {
			n = maxPageLimit
		}
		limit = n
	}
	if raw := c.Query("offset"); raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n < 0 {
			return 0, 0, ErrBadRequest("invalid offset query parameter")
		}
		offset = n
	}
	return limit, offset, nil
}

// setTotalCount 写入 X-Total-Count 响应头。
func setTotalCount(c *gin.Context, total int) {
	c.Header(XTotalCountHeader, strconv.Itoa(total))
}
