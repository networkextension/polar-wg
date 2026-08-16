package wg

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// The internal token routes must refuse anything that is not loopback and
// validate input before touching the DB (p.DB is nil here — a DB hit would panic).
func TestInternalWGTokens_GateAndValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	p := &Plugin{}
	r := gin.New()
	r.POST("/internal/v1/wg-tokens", p.handleInternalWGTokenMint)
	r.GET("/internal/v1/wg-tokens/:id", p.handleInternalWGTokenGet)
	r.POST("/internal/v1/wg-tokens/:id/release", p.handleInternalWGTokenRelease)

	do := func(method, path, body, remote string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = remote
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	// non-loopback → 403 on every route
	for _, c := range []struct{ m, p string }{
		{"POST", "/internal/v1/wg-tokens"}, {"GET", "/internal/v1/wg-tokens/1"}, {"POST", "/internal/v1/wg-tokens/1/release"},
	} {
		if w := do(c.m, c.p, `{"hub_slug":"x","label":"y"}`, "10.0.0.9:4321"); w.Code != http.StatusForbidden {
			t.Fatalf("%s %s from LAN: want 403, got %d %s", c.m, c.p, w.Code, w.Body.String())
		}
	}
	// loopback but bad input → 400 (before any DB access)
	if w := do("POST", "/internal/v1/wg-tokens", `{"hub_slug":"","label":""}`, "127.0.0.1:5555"); w.Code != http.StatusBadRequest {
		t.Fatalf("mint empty: want 400, got %d %s", w.Code, w.Body.String())
	}
	if w := do("POST", "/internal/v1/wg-tokens", `not json`, "[::1]:5555"); w.Code != http.StatusBadRequest {
		t.Fatalf("mint bad json: want 400, got %d", w.Code)
	}
	if w := do("GET", "/internal/v1/wg-tokens/abc", "", "127.0.0.1:5555"); w.Code != http.StatusBadRequest {
		t.Fatalf("get bad id: want 400, got %d", w.Code)
	}
	if w := do("POST", "/internal/v1/wg-tokens/0/release", "", "127.0.0.1:5555"); w.Code != http.StatusBadRequest {
		t.Fatalf("release id 0: want 400, got %d", w.Code)
	}
}
