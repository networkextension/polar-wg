package wg

// auth.go — admin auth middleware. wg-svc doesn't have its own session
// store; it asks dock to introspect Bearer tokens via /internal/v1/auth/verify
// (cached 30s in the SDK). Only callers whose role is "admin" can hit
// the /api/admin/wg-* surface.

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	ctxKeyUserID      = "user_id"
	ctxKeyUserRole    = "user_role"
	ctxKeyWorkspaceID = "workspace_id"
)

// requireAdminViaDock builds the gin middleware: extract Bearer token →
// dock SDK AuthVerify → require role=admin. Sets user_id / user_role /
// workspace_id on the gin context so handlers can attribute writes.
func (p *Plugin) requireAdminViaDock() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := extractAccessToken(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}
		res, err := p.Dock.AuthVerify(token)
		if err != nil {
			// Dock said no, or dock is unreachable. We can't distinguish
			// without leaking timing — return 401 either way. Dock-down
			// will show up in /healthz + heartbeat metrics; this handler
			// fails closed.
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid session"})
			return
		}
		if !strings.EqualFold(res.Role, "admin") {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin role required"})
			return
		}
		c.Set(ctxKeyUserID, res.UserID)
		c.Set(ctxKeyUserRole, res.Role)
		c.Set(ctxKeyWorkspaceID, res.WorkspaceID)
		c.Next()
	}
}

// extractAccessToken pulls a Bearer token from Authorization, or falls
// back to ?access_token= or a cookie named "access_token" (matches
// dock's policy so iOS / browser clients work the same way).
func extractAccessToken(c *gin.Context) string {
	if v := strings.TrimSpace(c.GetHeader("Authorization")); v != "" {
		if strings.HasPrefix(strings.ToLower(v), "bearer ") {
			return strings.TrimSpace(v[7:])
		}
	}
	if v := strings.TrimSpace(c.Query("access_token")); v != "" {
		return v
	}
	if v, err := c.Cookie("access_token"); err == nil && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return ""
}

// userIDFromCtx returns the authenticated user_id, or "" if none was set.
// Used by admin handlers that need to stamp created_by_user_id.
func userIDFromCtx(c *gin.Context) string {
	if v, ok := c.Get(ctxKeyUserID); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
