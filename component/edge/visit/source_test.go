package visit

import (
	"net/http"
	"testing"
)

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
