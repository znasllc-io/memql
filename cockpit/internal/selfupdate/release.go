// Package selfupdate updates the installed Cockpit without touching enrollment,
// credentials, permissions or worker policy. Release discovery and installation
// are separate: a background check can never replace a running program.
package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const catalogURL = "https://api.github.com/repos/znasllc-io/memql/releases?per_page=100"
const downloadRoot = "https://github.com/znasllc-io/memql/releases/download/"
const releaseRoot = "https://github.com/znasllc-io/memql/releases/tag/"

var stableVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var numericCatalog = regexp.MustCompile(`^/repositories/[1-9][0-9]*/releases$`)

type release struct {
	Tag        string `json:"tag_name"`
	Draft      *bool  `json:"draft"`
	Prerelease *bool  `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
	} `json:"assets"`
}

func baseVersion(s string) string { return strings.SplitN(strings.TrimPrefix(s, "v"), "+", 2)[0] }

// compareVersions only compares stable release numbers. Build metadata identifies
// a local build but cannot cause a release with a lower number to replace it.
func compareVersions(a, b string) (int, error) {
	a, b = baseVersion(a), baseVersion(b)
	if !stableVersion.MatchString(a) || !stableVersion.MatchString(b) {
		return 0, errors.New("Cannot compare this development version with a stable release")
	}
	left, right := strings.Split(a, "."), strings.Split(b, ".")
	for i := range left {
		// Decimal length comparison avoids overflow on untrusted catalog values.
		if len(left[i]) < len(right[i]) {
			return -1, nil
		}
		if len(left[i]) > len(right[i]) {
			return 1, nil
		}
		if left[i] < right[i] {
			return -1, nil
		}
		if left[i] > right[i] {
			return 1, nil
		}
	}
	return 0, nil
}

func (c *Client) get(ctx context.Context, address string, limit int64) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", "MemQL-Cockpit-Updater/"+c.Version)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, errors.New("Could not reach the Cockpit release service. Check your connection and retry")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("Cockpit release service returned HTTP %d. Retry later", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, nil, errors.New("The release download was interrupted. Retry")
	}
	if int64(len(body)) > limit {
		return nil, nil, errors.New("The release response exceeded its size limit")
	}
	return body, resp.Header, nil
}

func nextPage(header, initial, current string) (string, error) {
	if header == "" {
		return "", nil
	}
	first, _ := url.Parse(initial)
	here, _ := url.Parse(current)
	next := ""
	for _, entry := range strings.Split(header, ",") {
		parts := strings.Split(strings.TrimSpace(entry), ";")
		if len(parts) < 2 || !strings.HasPrefix(parts[0], "<") || !strings.HasSuffix(parts[0], ">") {
			return "", errors.New("Invalid release pagination")
		}
		isNext := false
		haveRelation := false
		for _, attr := range parts[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(attr), "=")
			if !ok {
				return "", errors.New("Invalid release pagination parameters")
			}
			if strings.EqualFold(strings.TrimSpace(key), "rel") {
				if haveRelation {
					return "", errors.New("Ambiguous release pagination relation")
				}
				haveRelation = true
				value = strings.TrimSpace(value)
				if strings.HasPrefix(value, `"`) {
					decoded, err := strconv.Unquote(value)
					if err != nil {
						return "", errors.New("Invalid release pagination relation")
					}
					value = decoded
				}
				for _, relation := range strings.Fields(value) {
					if relation == "next" {
						isNext = true
					}
				}
			}
		}
		if !haveRelation {
			return "", errors.New("Missing release pagination relation")
		}
		if !isNext {
			continue
		}
		u, err := url.Parse(strings.TrimSuffix(strings.TrimPrefix(parts[0], "<"), ">"))
		if err != nil || u.Scheme != first.Scheme || u.Host != first.Host || u.User != nil || u.Fragment != "" || (u.Path != first.Path && !(numericCatalog.MatchString(u.Path) && (here.Path == first.Path || here.Path == u.Path))) {
			return "", errors.New("Release pagination left the trusted catalog")
		}
		if next != "" {
			return "", errors.New("Ambiguous release pagination")
		}
		next = u.String()
	}
	return next, nil
}

func (c *Client) latest(ctx context.Context) (release, error) {
	var best release
	seen := map[string]bool{}
	address := c.catalog
	for address != "" {
		if seen[address] || len(seen) >= 100 {
			return best, errors.New("The complete Cockpit release catalog could not be read")
		}
		seen[address] = true
		body, headers, err := c.get(ctx, address, 2<<20)
		if err != nil {
			return best, err
		}
		var rows []release
		if err = json.Unmarshal(body, &rows); err != nil || strings.TrimSpace(string(body)) == "null" {
			return best, errors.New("Invalid Cockpit release catalog")
		}
		for _, row := range rows {
			if row.Draft == nil || row.Prerelease == nil || *row.Draft || *row.Prerelease || !strings.HasPrefix(row.Tag, "cockpit/v") || !stableVersion.MatchString(strings.TrimPrefix(row.Tag, "cockpit/v")) {
				continue
			}
			if best.Tag == "" {
				best = row
				continue
			}
			if cmp, _ := compareVersions(strings.TrimPrefix(row.Tag, "cockpit/"), strings.TrimPrefix(best.Tag, "cockpit/")); cmp > 0 {
				best = row
			}
		}
		address, err = nextPage(strings.Join(headers.Values("Link"), ","), c.catalog, address)
		if err != nil {
			return best, err
		}
	}
	if best.Tag == "" {
		return best, errors.New("No stable Cockpit release has been published yet")
	}
	return best, nil
}

func (c *Client) assetNames(version string) (string, string) {
	triple := c.OS + "-" + c.Arch
	if c.OS == "darwin" && c.bundle != "" {
		name := "memql-app-" + triple + ".tar.gz"
		return name, name + ".sha256"
	}
	if c.Variant == "computeruse" {
		name := "memql-computeruse-" + triple
		return name, name + ".sha256"
	}
	return "memql-" + version + "-" + triple + ".tar.gz", "memql-" + version + "-SHA256SUMS"
}

func hasAsset(r release, name string) bool {
	for _, a := range r.Assets {
		if a.Name == name {
			return true
		}
	}
	return false
}

func expectedChecksum(body []byte, name string) (string, error) {
	found := ""
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		decoded, err := hex.DecodeString(fields[0])
		if err != nil || len(decoded) != sha256.Size || found != "" {
			return "", errors.New("The release checksum is invalid or ambiguous")
		}
		found = strings.ToLower(fields[0])
	}
	if found == "" {
		return "", errors.New("The release is missing its checksum")
	}
	return found, nil
}

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || req.URL.Scheme != "https" || req.URL.User != nil {
			return errors.New("Untrusted download redirect")
		}
		return nil
	}}
}

func operationID() string { return strconv.FormatInt(time.Now().UnixNano(), 36) }
