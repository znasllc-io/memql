package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// TokenCookieMiddleware Tests
// =============================================================================

func TestTokenCookieMiddlewareCookieToHeader(t *testing.T) {
	t.Parallel()

	// Create middleware
	middleware := TokenCookieMiddleware()

	// Track what the downstream handler receives
	var receivedAuthHeader string
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuthHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))

	// Request with cookie but no Authorization header
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.AddCookie(&http.Cookie{
		Name:  authCookieName,
		Value: "test-jwt-token",
	})
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "Bearer test-jwt-token", receivedAuthHeader)
}

func TestTokenCookieMiddlewareNoCookie(t *testing.T) {
	t.Parallel()

	middleware := TokenCookieMiddleware()

	var receivedAuthHeader string
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuthHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))

	// Request without cookie
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Empty(t, receivedAuthHeader)
}

func TestTokenCookieMiddlewareExistingHeader(t *testing.T) {
	t.Parallel()

	middleware := TokenCookieMiddleware()

	var receivedAuthHeader string
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuthHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))

	// Request with both cookie AND existing Authorization header
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer existing-token")
	req.AddCookie(&http.Cookie{
		Name:  authCookieName,
		Value: "cookie-token",
	})
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code)
	// Should preserve existing header, NOT override with cookie
	assert.Equal(t, "Bearer existing-token", receivedAuthHeader)
}

func TestTokenCookieMiddlewareEmptyCookie(t *testing.T) {
	t.Parallel()

	middleware := TokenCookieMiddleware()

	var receivedAuthHeader string
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuthHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))

	// Request with empty cookie value
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.AddCookie(&http.Cookie{
		Name:  authCookieName,
		Value: "",
	})
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Empty(t, receivedAuthHeader)
}

func TestTokenCookieMiddlewareNilHandler(t *testing.T) {
	t.Parallel()

	middleware := TokenCookieMiddleware()

	// Should not panic with nil handler - defaults to DefaultServeMux
	handler := middleware(nil)
	require.NotNil(t, handler)
}

func TestTokenCookieMiddlewareNilRequest(t *testing.T) {
	t.Parallel()

	middleware := TokenCookieMiddleware()

	handlerCalled := false
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
	}))

	// Call with nil request - should not panic
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, nil)

	assert.True(t, handlerCalled)
}

// =============================================================================
// Token Cookie Helper Tests
// =============================================================================

func TestReadAuthCookie(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		setup    func() *http.Request
		expected string
	}{
		{
			name: "cookie present",
			setup: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				req.AddCookie(&http.Cookie{Name: authCookieName, Value: "my-token"})
				return req
			},
			expected: "my-token",
		},
		{
			name: "cookie with whitespace",
			setup: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				req.AddCookie(&http.Cookie{Name: authCookieName, Value: "  trimmed  "})
				return req
			},
			expected: "trimmed",
		},
		{
			name: "cookie absent",
			setup: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "/", nil)
			},
			expected: "",
		},
		{
			name: "nil request",
			setup: func() *http.Request {
				return nil
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.setup()
			result := readAuthCookie(req)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestBuildAuthCookie(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	cookie := buildAuthCookie(req, "test-token", 3600)

	require.NotNil(t, cookie)
	assert.Equal(t, authCookieName, cookie.Name)
	assert.Equal(t, "test-token", cookie.Value)
	assert.Equal(t, "/", cookie.Path)
	assert.True(t, cookie.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
	assert.Equal(t, 3600, cookie.MaxAge)
}

func TestBuildAuthCookieEmptyToken(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	cookie := buildAuthCookie(req, "", 3600)

	assert.Nil(t, cookie)
}

func TestBuildAuthCookieWhitespaceToken(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	cookie := buildAuthCookie(req, "   ", 3600)

	assert.Nil(t, cookie)
}

func TestBuildAuthCookieDefaultTTL(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	cookie := buildAuthCookie(req, "token", 0)

	require.NotNil(t, cookie)
	assert.Equal(t, defaultCookieMaxAgeS, cookie.MaxAge)
}

func TestBuildAuthCookieSecure(t *testing.T) {
	t.Parallel()

	// HTTP request - not secure
	httpReq := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
	httpCookie := buildAuthCookie(httpReq, "token", 3600)
	assert.False(t, httpCookie.Secure)

	// HTTPS via X-Forwarded-Proto
	httpsReq := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
	httpsReq.Header.Set("X-Forwarded-Proto", "https")
	httpsCookie := buildAuthCookie(httpsReq, "token", 3600)
	assert.True(t, httpsCookie.Secure)
}

func TestBuildClearAuthCookie(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	cookie := buildClearAuthCookie(req)

	require.NotNil(t, cookie)
	assert.Equal(t, authCookieName, cookie.Name)
	assert.Equal(t, "", cookie.Value)
	assert.Equal(t, -1, cookie.MaxAge)
	assert.True(t, cookie.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
}

func TestIsSecureRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		setup    func() *http.Request
		expected bool
	}{
		{
			name: "nil request",
			setup: func() *http.Request {
				return nil
			},
			expected: false,
		},
		{
			name: "http request",
			setup: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
			},
			expected: false,
		},
		{
			name: "X-Forwarded-Proto https",
			setup: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
				req.Header.Set("X-Forwarded-Proto", "https")
				return req
			},
			expected: true,
		},
		{
			name: "X-Forwarded-Proto HTTPS uppercase",
			setup: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
				req.Header.Set("X-Forwarded-Proto", "HTTPS")
				return req
			},
			expected: true,
		},
		{
			name: "X-Forwarded-Proto http",
			setup: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "http://localhost/", nil)
				req.Header.Set("X-Forwarded-Proto", "http")
				return req
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.setup()
			result := isSecureRequest(req)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestHasAuthorizationHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		setup    func() *http.Request
		expected bool
	}{
		{
			name: "has header",
			setup: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				req.Header.Set("Authorization", "Bearer token")
				return req
			},
			expected: true,
		},
		{
			name: "empty header",
			setup: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				req.Header.Set("Authorization", "")
				return req
			},
			expected: false,
		},
		{
			name: "whitespace header",
			setup: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				req.Header.Set("Authorization", "   ")
				return req
			},
			expected: false,
		},
		{
			name: "no header",
			setup: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "/", nil)
			},
			expected: false,
		},
		{
			name: "nil request",
			setup: func() *http.Request {
				return nil
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.setup()
			result := hasAuthorizationHeader(req)
			assert.Equal(t, tt.expected, result)
		})
	}
}
