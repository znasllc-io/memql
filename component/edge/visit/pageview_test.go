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

// A PATH IS STORED, SO IT MUST BE TEXT THE DATABASE TAKES. A request path is
// percent-decoded and a ping's body is raw bytes, so either can carry a NUL --
// which the database driver refuses outright, failing the whole batch insert
// and every other site's rows in it -- or bytes that are not UTF-8. Both are
// dropped, with every other control character, BEFORE the path is judged and
// cut to length.
func TestNormalizePathDropsControlCharactersAndInvalidUTF8(t *testing.T) {
	for in, want := range map[string]string{
		"/a\x00b":           "/ab",
		"/caf\xff":          "/caf",
		"/a\x01\x1f\x7fb":   "/ab",
		"/tab\tand\nbreak":  "/tabandbreak",
		"/about/\x00":       "/about", // cleaned before the trailing slash is judged
		"/caf\xc3\xa9/menu": "/café/menu",
	} {
		if got := NormalizePath(in); got != want {
			t.Errorf("NormalizePath(%q) = %q, want %q", in, got, want)
		}
	}

	// An invalid byte near the start used to make the cut trim back to the
	// bytes before it; cleaned first, the path keeps its full length.
	long := "/a\xff" + strings.Repeat("b", 300)
	want := ("/a" + strings.Repeat("b", 300))[:256]
	if got := NormalizePath(long); got != want {
		t.Errorf("a long path holding an invalid byte normalized to %d bytes (%q...), want the first 256 bytes of the cleaned path", len(got), got[:min(len(got), 8)])
	}
}

// truncate cuts through at most one rune: a cut that lands inside a
// multi-byte rune drops that rune's leading bytes and nothing more.
func TestTruncateTrimsOnlyATrailingPartialRune(t *testing.T) {
	const fourByte = "\U00010348" // a four-byte rune
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 2, "ab"},
		{"abé", 3, "ab"},           // a two-byte rune cut after its first byte
		{"ab" + fourByte, 5, "ab"}, // a four-byte rune cut after three of its bytes
		{"ab" + fourByte, 6, "ab" + fourByte},
		{"ab�", 5, "ab�"}, // a real replacement character is kept
	} {
		if got := truncate(tc.in, tc.n); got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
	// Handed bytes that are not UTF-8 -- which no caller does now -- it still
	// cuts at the limit rather than trimming the whole string away.
	if got := truncate("\xff"+strings.Repeat("a", 10), 5); got != "\xffaaaa" {
		t.Errorf("truncate of a string with an invalid first byte = %q, want the first five bytes", got)
	}
}
