package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/cockpit/internal/crash"
)

type LocalLogEntry struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Home    string `json:"home"`
	Message string `json:"message"`
}

var localLogURLs = regexp.MustCompile(`https?://[^\s"<>]+`)
var localLogSecrets = regexp.MustCompile(`(?i)(authorization|bearer|cookie|password|secret|token|magic[_-]?link|access[_-]?key|refresh[_-]?token)(["']?\s*[:=]\s*["']?|\s+)[^\s,;"']+`)
var localLogTokens = regexp.MustCompile(`mql_(?:pat|wkr|va)_[A-Za-z0-9_-]+`)

func sanitizeLocalLog(value string) string {
	value = crash.SanitizeForCrashLog(value)
	value = localLogTokens.ReplaceAllString(value, "<REDACTED>")
	value = localLogSecrets.ReplaceAllString(value, "${1}=<REDACTED>")
	return localLogURLs.ReplaceAllStringFunc(value, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return "<URL-REDACTED>"
		}
		return u.Scheme + "://" + u.Host + "/<path-redacted>"
	})
}

// The caller cannot choose a file. Read only this user's fixed worker log,
// cap memory and output, and redact before crossing the local control socket.
func readLocalWorkerLogs(stateDir string) ([]LocalLogEntry, error) {
	if stateDir == "" {
		stateDir = Defaults().StateDir
	}
	path := filepath.Join(stateDir, "worker.log")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return []LocalLogEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !ownedByCurrentUser(info) {
		return nil, errors.New("worker log must be a regular file owned by this user")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, errors.New("worker log changed while opening")
	}
	const limit = 256 * 1024
	offset := info.Size() - limit
	if offset < 0 {
		offset = 0
	}
	if _, err = file.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	if offset > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	if len(lines) > 300 {
		lines = lines[len(lines)-300:]
	}
	entries := make([]LocalLogEntry, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		entry := LocalLogEntry{Level: "INFO", Message: sanitizeLocalLog(line)}
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil {
			entry.Time, _ = record["time"].(string)
			entry.Level, _ = record["level"].(string)
			entry.Home, _ = record["home"].(string)
			message, _ := record["msg"].(string)
			// Diagnostics only: whitelist fields; arbitrary tool output, credentials,
			// prompts and request payloads never enter this viewer.
			var attrs []string
			for _, key := range []string{"error", "code", "reason", "backoff_seconds", "registration_id", "enabled", "signal"} {
				if val, ok := record[key]; ok {
					attrs = append(attrs, key+"="+fmt.Sprint(val))
				}
			}
			sort.Strings(attrs)
			entry.Message = sanitizeLocalLog(strings.TrimSpace(message + " " + strings.Join(attrs, " ")))
		}
		entry.Time = sanitizeLocalLog(entry.Time)
		entry.Level = sanitizeLocalLog(entry.Level)
		entry.Home = sanitizeLocalLog(entry.Home)
		if len(entry.Message) > 4096 {
			entry.Message = entry.Message[:4096] + "…"
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
