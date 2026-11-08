package visit

import "strings"

// Agent is what a user agent string says about the visitor. Every field is a
// member of a closed set so a statistic groups on it without cleaning.
type Agent struct {
	Device  string // desktop, mobile, tablet, or "" for an empty user agent
	Browser string
	OS      string
	Bot     bool
}

// botTokens are lowercase substrings that mark automation. Each is a word
// real browsers do not put in their user agent, so a person is rarely counted
// as a bot. An empty user agent is a bot too: no browser sends none.
var botTokens = []string{
	// crawlers and fetchers
	"bot", "crawl", "spider", "slurp", "facebookexternalhit", "embedly", "preview",
	// browser automation
	"headless", "phantomjs", "selenium", "puppeteer", "playwright", "lighthouse",
	// monitors
	"pingdom", "uptime", "monitor", "memql-site-health",
	// HTTP clients
	"curl/", "wget/", "python-requests", "go-http-client", "java/", "libwww-perl", "httpie", "axios/", "node-fetch",
}

// Classify reads a user agent. Order matters throughout: browsers hide inside
// each other's strings (Edge says Chrome, Chrome says Safari), so the most
// specific token is tested first.
func Classify(userAgent string) Agent {
	if strings.TrimSpace(userAgent) == "" {
		return Agent{Bot: true}
	}
	return Agent{
		Device:  device(userAgent),
		Browser: browser(userAgent),
		OS:      operatingSystem(userAgent),
		Bot:     isBot(userAgent),
	}
}

func isBot(ua string) bool {
	l := strings.ToLower(ua)
	for _, t := range botTokens {
		if strings.Contains(l, t) {
			return true
		}
	}
	return false
}

func has(ua string, tokens ...string) bool {
	for _, t := range tokens {
		if strings.Contains(ua, t) {
			return true
		}
	}
	return false
}

func browser(ua string) string {
	switch {
	case has(ua, "Edg/", "Edge/"):
		return "Edge"
	case has(ua, "OPR/", "Opera"):
		return "Opera"
	case has(ua, "SamsungBrowser"):
		return "Samsung Internet"
	case has(ua, "Firefox/", "FxiOS"):
		return "Firefox"
	case has(ua, "Chrome/", "CriOS"):
		return "Chrome"
	case has(ua, "Safari/") && has(ua, "Version/"):
		return "Safari"
	}
	return "Other"
}

func operatingSystem(ua string) string {
	switch {
	case has(ua, "iPad"):
		return "iPadOS"
	case has(ua, "iPhone", "iPod"):
		return "iOS"
	case has(ua, "Android"):
		return "Android"
	case has(ua, "Windows"):
		return "Windows"
	case has(ua, "Macintosh"):
		return "macOS"
	case has(ua, "CrOS"):
		return "ChromeOS"
	case has(ua, "Linux"):
		return "Linux"
	}
	return "Other"
}

// device: an Android user agent without "Mobile" is a tablet, which is how
// Chrome itself distinguishes the two.
func device(ua string) string {
	switch {
	case has(ua, "iPad"):
		return "tablet"
	case has(ua, "iPhone", "iPod"):
		return "mobile"
	case has(ua, "Android"):
		if has(ua, "Mobile") {
			return "mobile"
		}
		return "tablet"
	}
	return "desktop"
}
