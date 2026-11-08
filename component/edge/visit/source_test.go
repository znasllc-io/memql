package visit

import (
	"net/http"
	"strings"
	"testing"
)

// A SOURCE IS STORED TOO, and it is written by whoever sent the request: a
// utm_source is percent-decoded, so "%00" arrives as a NUL and "%ff" as a byte
// that is not UTF-8. Both are dropped, with every other control character,
// before the value is judged and cut; a utm_source with nothing left is no
// utm_source at all. A referring host is cut at 253 bytes, the longest a DNS
// name can be: url.Parse puts no limit on a host, so without the cap one
// request could store a header-sized string as its source.
func TestSourceDropsControlCharactersAndCapsTheHost(t *testing.T) {
	for target, want := range map[string]string{
		"https://site.test/?utm_source=news%00let%ffter%01": "newsletter",
		"https://site.test/?utm_source=%00%ff":              "direct",
		"https://site.test/?utm_source=%01%20Mail%7f":       "mail",
	} {
		r, _ := http.NewRequest("GET", target, nil)
		if got := Source(r); got != want {
			t.Errorf("Source(%s) = %q, want %q", target, got, want)
		}
	}

	r, _ := http.NewRequest("GET", "https://site.test/", nil)
	r.Header.Set("Referer", "https://ex%ffample.org/page")
	if got := Source(r); got != "example.org" {
		t.Errorf("a referring host holding a byte that is not UTF-8 gave %q, want example.org", got)
	}

	host := strings.Repeat("a", 296) + ".com" // 300 bytes
	r.Header.Set("Referer", "https://"+host+"/page")
	if got := Source(r); len(got) != 253 || !strings.HasPrefix(host, got) {
		t.Errorf("a 300-byte referring host gave %d bytes, want its first 253", len(got))
	}
}

func TestSource(t *testing.T) {
	cases := []struct {
		name, url, referer string
		own                []string
		want               string
	}{
		{"utm", "https://site.test/?utm_source=Newsletter", "", nil, "newsletter"},
		{"google", "https://site.test/", "https://www.google.com/search?q=x", nil, "google"},
		{"google tld", "https://site.test/", "https://google.co.uk/", nil, "google"},
		{"not google", "https://site.test/", "https://google.evil.example/", nil, "google.evil.example"},
		{"facebook", "https://site.test/", "https://l.facebook.com/l.php", nil, "facebook.com"},
		{"t.co", "https://site.test/", "https://t.co/abc", nil, "x.com"},
		{"same host", "https://site.test/", "https://site.test/other", nil, ""},
		{"own host", "https://site.test/", "https://other.test/", []string{"other.test"}, ""},
		{"none", "https://site.test/", "", nil, "direct"},
		{"malformed", "https://site.test/", "://bad", nil, "direct"},
		{"other host", "https://site.test/", "https://news.example.org/x", nil, "news.example.org"},
	}
	for _, tc := range cases {
		r, _ := http.NewRequest("GET", tc.url, nil)
		if tc.referer != "" {
			r.Header.Set("Referer", tc.referer)
		}
		if got := Source(r, tc.own...); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}
