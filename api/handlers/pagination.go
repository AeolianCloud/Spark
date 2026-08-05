package handlers

import (
	"strconv"

	"github.com/gin-gonic/gin"
)

// XTotalCountHeader is the response header of the paginated list endpoints
// (GET /zones, /ip-pools, /storage-types, /images, /vms): the total number
// of items matching the query, before limit/offset are applied. It lets
// clients render pager controls without a second request.
const XTotalCountHeader = "X-Total-Count"

const (
	// defaultPageLimit is the page size when the client sends no limit.
	defaultPageLimit = 25
	// maxPageLimit caps the page size: an oversized limit is truncated to
	// this value instead of being rejected. The cap is a DoS guard (a page
	// query is at most this many rows, and for /vms at most this many PVE
	// merges), and truncating instead of erroring keeps a misbehaving client
	// on a working path instead of a 400 it may not retry.
	maxPageLimit = 100
)

// parsePagination reads the shared limit/offset query parameters of the
// paginated list endpoints. limit defaults to defaultPageLimit, an
// oversized limit is capped at maxPageLimit, offset defaults to 0; a
// negative or non-numeric value is rejected with 400 bad_request. The
// returned limit/offset are always non-negative.
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

// setTotalCount writes the X-Total-Count response header.
func setTotalCount(c *gin.Context, total int) {
	c.Header(XTotalCountHeader, strconv.Itoa(total))
}
