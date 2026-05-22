package wg

// helpers.go — small functions copied from dock that the moved wg
// handlers depend on. Kept here so wg-svc has no compile-time
// dependency on the dock package.

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// sanitizeFilename mirrors dock's `handler_helpers.go::sanitizeFilename`.
// Used by bundle download's Content-Disposition header.
func sanitizeFilename(input string) string {
	if input == "" {
		return "untitled"
	}
	var b strings.Builder
	for _, r := range input {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "untitled"
	}
	return out
}

// extractBearerToken mirrors dock's middleware.go helper. Used by the
// /v1/* device-side handlers that authenticate with a per-device
// Bearer token (the wg-mac token).
func extractBearerToken(c *gin.Context) string {
	header := c.GetHeader("Authorization")
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// extractPublicIP is defined in alloc.go (it came with the wg_alloc.go
// copy); leaving the def there to keep the diff minimal.
