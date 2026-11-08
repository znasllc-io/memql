package visit

import (
	"net/http"
	"strings"
	"testing"
)

func TestIsPageView(t *testing.T) {
	nav := map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}
	cases := []struct {
		name, method string
		h            map[string]string
		class        string
		status       int
		want         bool
	}{
		{"navigate 200", "GET", nav, "document", 200, true},
		{"navigate 304", "GET", nav, "document", 304, true},
		{"legacy html fallback", "GET", map[string]string{"Accept": "text/html,*/*"}, "fallback", 200, true},
		{"404", "GET", nav, "document", 404, false},
		{"iframe", "GET", map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "iframe"}, "document", 200, false},
		{"sec-purpose prefetch", "GET", map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document", "Sec-Purpose": "prefetch"}, "document", 200, false},
		{"purpose prefetch", "GET", map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document", "Purpose": "prefetch"}, "document", 200, false},
		{"post", "POST", nav, "document", 200, false},
		{"head", "HEAD", nav, "document", 200, false},
		{"legacy asset", "GET", map[string]string{"Accept": "*/*"}, "asset", 200, false},
	}
	for _, tc := range cases {
		r, _ := http.NewRequest(tc.method, "/", nil)
		for k, v := range tc.h {
			r.Header.Set(k, v)
		}
		if got := IsPageView(r, tc.class, tc.status); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestNormalizePath(t *testing.T) {
	if got := NormalizePath("/" + strings.Repeat("a", 299)); len(got) != 256 {
		t.Fatalf("long path len %d", len(got))
	}
	// A multi-byte rune straddling the limit is dropped whole.
	if got := NormalizePath("/" + strings.Repeat("a", 254) + "é"); got != "/"+strings.Repeat("a", 254) {
		t.Fatalf("rune boundary: len %d", len(got))
	}
	for in, want := range map[string]string{"/about/": "/about", "/": "/", "/a?b=c": "/a", "/a#x": "/a", "about": ""} {
		if got := NormalizePath(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}
