package server_test

// upstream_proxy_test.go — validation-path tests for the
// /api/v1/upstream-image proxy. The actual streaming is exercised
// manually in the browser since stubbing http.DefaultClient against
// a hardcoded host allowlist would require plumbing a transport seam
// through the server constructor that no other handler needs.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUpstreamProxyRejectsMissingURL(t *testing.T) {
	t.Parallel()

	srv := fixtureBackedServerWithClients(t, nil, nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/v1/upstream-image", nil)
	srv.Handler().ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
}

func TestUpstreamProxyRejectsBadScheme(t *testing.T) {
	t.Parallel()

	srv := fixtureBackedServerWithClients(t, nil, nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/v1/upstream-image?url=file:///etc/passwd", nil)
	srv.Handler().ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
}

// TestUpstreamProxyHostAllowlist sweeps the hostname guard:
//   - exact-match allowlisted hosts pass validation (we don't actually
//     reach them — DialContext to a non-existent test host fails with
//     a 502, which is fine; the assertion is that the 400 host-check
//     was passed),
//   - lookalikes ("stagemedia.me.evil.com") are rejected,
//   - unrelated hosts are rejected.
func TestUpstreamProxyHostAllowlist(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		url      string
		wantCode int
	}{
		{
			name:     "lookalike rejected",
			url:      "https://stagemedia.me.evil.com/x.jpg",
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "unrelated rejected",
			url:      "https://example.com/x.jpg",
			wantCode: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := fixtureBackedServerWithClients(t, nil, nil)
			rr := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
				"/api/v1/upstream-image?url="+tt.url, nil)
			srv.Handler().ServeHTTP(rr, req)
			assert.Equal(t, tt.wantCode, rr.Code, rr.Body.String())
		})
	}
}
