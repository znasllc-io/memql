package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWebEditorFileCORSStillRequiresBearer(t *testing.T) {
	for _, path := range []string{"/artifacts", "/artifacts/file/content"} {
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Cookie") != "" {
				t.Error("editor request retained cookies")
			}
			if r.Header.Get("Authorization") != "Bearer supplied" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
		})
		handler := corsMiddleware(nil, next, nil)
		for _, bearer := range []string{"", "Bearer supplied"} {
			req := httptest.NewRequest("GET", path, nil)
			req.Header.Set("Origin", "https://vscode.dev")
			req.Header.Set("Authorization", bearer)
			req.Header.Set("Cookie", "refresh_token=secret")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			want := http.StatusOK
			if bearer == "" {
				want = http.StatusUnauthorized
			}
			if rec.Code != want || rec.Header().Get("Access-Control-Allow-Origin") != "https://vscode.dev" || rec.Header().Get("Access-Control-Allow-Credentials") != "" {
				t.Fatalf("%s bearer=%q: %v", path, bearer, rec.Result())
			}
		}
	}
	for _, path := range []string{"/artifacts-extra", "/other"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Origin", "https://vscode.dev")
		rec := httptest.NewRecorder()
		corsMiddleware(nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), nil).ServeHTTP(rec, req)
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("editor granted unrelated route %s", path)
		}
	}
}
