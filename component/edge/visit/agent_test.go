package visit

import "testing"

func TestClassifyKnownBrowsers(t *testing.T) {
	cases := []struct {
		name, ua string
		want     Agent
	}{
		{"chrome windows", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36", Agent{"desktop", "Chrome", "Windows", false}},
		{"safari mac", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15", Agent{"desktop", "Safari", "macOS", false}},
		{"iphone", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1", Agent{"mobile", "Safari", "iOS", false}},
		{"ipad", "Mozilla/5.0 (iPad; CPU OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1", Agent{"tablet", "Safari", "iPadOS", false}},
		{"firefox linux", "Mozilla/5.0 (X11; Linux x86_64; rv:133.0) Gecko/20100101 Firefox/133.0", Agent{"desktop", "Firefox", "Linux", false}},
		{"edge windows", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/131.0.0.0", Agent{"desktop", "Edge", "Windows", false}},
		{"samsung phone", "Mozilla/5.0 (Linux; Android 14; SM-S918B) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/25.0 Chrome/121.0.0.0 Mobile Safari/537.36", Agent{"mobile", "Samsung Internet", "Android", false}},
		{"chrome android tablet", "Mozilla/5.0 (Linux; Android 13; SM-X700) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36", Agent{"tablet", "Chrome", "Android", false}},
	}
	for _, tc := range cases {
		if got := Classify(tc.ua); got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestClassifyBots(t *testing.T) {
	for _, ua := range []string{
		"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
		"Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)",
		"curl/8.4.0",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/131.0.0.0 Safari/537.36",
		"",
	} {
		if !Classify(ua).Bot {
			t.Errorf("%q should be a bot", ua)
		}
	}
}
