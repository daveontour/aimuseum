package repository

import (
	"context"
	"fmt"

	"github.com/daveontour/aimuseum/internal/appctx"
)

// uidFromCtx returns the authenticated user ID from context, or 0.
func uidFromCtx(ctx context.Context) int64 {
	return appctx.UserIDFromCtx(ctx)
}

// addUIDFilter appends "AND user_id = $N" to q+args when uid > 0.
// When uid == 0 (unauthenticated / single-tenant mode) no filter is added.
func addUIDFilter(q string, args []any, uid int64) (string, []any) {
	if uid == 0 {
		return q, args
	}
	args = append(args, uid)
	return q + fmt.Sprintf(" AND user_id = $%d", len(args)), args
}

// addUIDFilterQualified appends "AND <alias>.user_id = $N" when uid > 0.
// Use when the query JOINs tables that each have user_id (multitenancy), e.g. media_items mi + post_media pm.
func addUIDFilterQualified(q string, args []any, uid int64, tableAlias string) (string, []any) {
	if uid == 0 {
		return q, args
	}
	args = append(args, uid)
	return q + fmt.Sprintf(" AND %s.user_id = $%d", tableAlias, len(args)), args
}

// uidVal returns the user_id value for INSERT statements.
// Returns nil (SQL NULL) for unauthenticated callers (uid==0).
func uidVal(uid int64) any {
	if uid == 0 {
		return nil
	}
	return uid
}
