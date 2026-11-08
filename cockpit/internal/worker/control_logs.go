package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	"syscall"

	"github.com/znasllc-io/memql/cockpit/internal/crash"
)

type LocalLogEntry struct {
	ID      string `json:"id"`
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

const (
	localLogReadLimit    = 256 * 1024
	localLogHistoryLimit = 1024 * 1024
	localLogEntryLimit   = 300
	localLogOutputLimit  = 256 * 1024
)

type localLogPage struct {
	Entries []LocalLogEntry
	Cursor  string
	Reset   string
	HasMore bool
}

// A cursor describes a byte boundary in the one fixed log, never a path. The
// preceding-byte hash detects copy-truncate/regrow as well as a smaller file;
// inode identity handles replacement. Skip only tracks an oversized record
// whose remaining bytes must be discarded through its next newline.
type localLogCursor struct {
	Version int    `json:"v"`
	File    string `json:"f"`
	Epoch   string `json:"g"`
	Offset  int64  `json:"o"`
	Anchor  string `json:"a"`
	Skip    bool   `json:"s,omitempty"`
}

func decodeLocalLogCursor(raw string) (localLogCursor, error) {
	var cursor localLogCursor
	if len(raw) > 512 {
		return cursor, errors.New("invalid log cursor")
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return cursor, errors.New("invalid log cursor")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&cursor)
	validHash := func(s string) bool { b, e := hex.DecodeString(s); return e == nil && len(b) == sha256.Size }
	if err != nil || cursor.Version != 1 || cursor.Offset < 0 || !validHash(cursor.File) || !validHash(cursor.Epoch) || !validHash(cursor.Anchor) {
		return localLogCursor{}, errors.New("invalid log cursor")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return localLogCursor{}, errors.New("invalid log cursor")
	}
	return cursor, nil
}

func localLogAnchor(file *os.File, offset int64) (string, error) {
	size := min(offset, 256)
	data := make([]byte, size)
	if _, err := file.ReadAt(data, offset-size); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

// Keep the legacy snapshot consumer intact; richer consumers advance only a
// confirmed cursor and retain their own bounded history across read failures.
func readLocalWorkerLogs(stateDir string) ([]LocalLogEntry, error) {
	page, err := readLocalWorkerLogPage(stateDir, "")
	return page.Entries, err
}

// The caller cannot choose a file. Read only this user's fixed regular log,
// bound both input and encoded output, and redact before crossing the socket.
// Incomplete trailing records are held for the next read, not rendered twice.
func readLocalWorkerLogPage(stateDir, rawCursor string) (localLogPage, error) {
	return readLocalWorkerLogPageWithRead(stateDir, rawCursor, func(file *os.File, data []byte, offset int64) (int, error) {
		return file.ReadAt(data, offset)
	})
}

// The read seam keeps snapshot/replacement races reproducible without sleeps.
func readLocalWorkerLogPageWithRead(stateDir, rawCursor string, readAt func(*os.File, []byte, int64) (int, error)) (localLogPage, error) {
	page := localLogPage{Entries: []LocalLogEntry{}}
	var cursor localLogCursor
	var err error
	if rawCursor != "" {
		cursor, err = decodeLocalLogCursor(rawCursor)
		if err != nil {
			return page, err
		}
	}
	if stateDir == "" {
		stateDir = Defaults().StateDir
	}
	path := filepath.Join(stateDir, "worker.log")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if rawCursor != "" {
			page.Reset = "missing"
		}
		return page, nil
	}
	if err != nil {
		return page, err
	}
	if !info.Mode().IsRegular() || !ownedByCurrentUser(info) {
		return page, errors.New("worker log must be a regular file owned by this user")
	}
	file, err := os.Open(path)
	if err != nil {
		return page, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return page, err
	}
	if !os.SameFile(info, opened) {
		return page, errors.New("worker log changed while opening")
	}
	stat := opened.Sys().(*syscall.Stat_t) // Ownership validation checked this type.
	identity := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%d:%d", stat.Dev, stat.Ino))))
	size := opened.Size()
	initial := rawCursor == ""
	if !initial {
		switch {
		case cursor.File != identity:
			page.Reset = "rotated"
		case cursor.Offset > size:
			page.Reset = "truncated"
		default:
			anchor, err := localLogAnchor(file, cursor.Offset)
			if err != nil {
				return page, errors.New("worker log changed while reading")
			}
			if anchor != cursor.Anchor {
				page.Reset = "truncated"
			}
		}
		if page.Reset == "" && size-cursor.Offset > localLogHistoryLimit {
			page.Reset = "expired"
		}
		initial = page.Reset != ""
	}
	start, skip := cursor.Offset, cursor.Skip
	if initial {
		start = max(0, size-localLogReadLimit)
	}
	readStart := max(0, start-256)
	prefixSize := int(start - readStart)
	captured := make([]byte, int(min(size-start, localLogReadLimit))+prefixSize)
	if _, err = readAt(file, captured, readStart); err != nil {
		return page, errors.New("worker log changed while reading")
	}
	prefix, data := captured[:prefixSize], captured[prefixSize:]
	if initial {
		skip = prefixSize > 0 && prefix[prefixSize-1] != '\n'
	} else {
		// Validate the preceding bytes in the SAME snapshot as the records.
		// Otherwise copy-truncate/regrow between reads could acknowledge new
		// file bytes while displaying old records and permanently skip them.
		if fmt.Sprintf("%x", sha256.Sum256(prefix)) != cursor.Anchor {
			return page, errors.New("worker log changed while reading")
		}
		if prefixSize > 0 && !skip && prefix[prefixSize-1] != '\n' {
			return page, errors.New("invalid log cursor boundary")
		}
	}
	epoch := cursor.Epoch
	if rawCursor == "" || page.Reset == "rotated" {
		epoch = identity
	}
	if page.Reset == "truncated" {
		// Rewritten logs can repeat the same message at the same offset. A
		// new observed generation keeps those events distinct from history;
		// later cursor pages keep this epoch as their file grows normally.
		epoch = fmt.Sprintf("%x", sha256.Sum256(append([]byte(cursor.Epoch+":"), captured...)))
	}
	position, outputBytes := 0, 0
	if skip {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			position = len(data)
		} else {
			position, skip = newline+1, false
		}
	}
	for position < len(data) {
		newline := bytes.IndexByte(data[position:], '\n')
		if newline < 0 {
			// One record larger than the read bound cannot pin the cursor forever.
			// Discard it through its newline on later reads and report the gap once.
			if position == 0 && len(data) == localLogReadLimit {
				position, skip = len(data), true
				if page.Reset == "" {
					page.Reset = "oversized"
				}
			}
			break
		}
		end := position + newline + 1
		line := strings.TrimSuffix(string(data[position:end-1]), "\r")
		if strings.TrimSpace(line) == "" {
			position = end
			continue
		}
		entry := parseLocalLogEntry(line)
		entry.ID = fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%s", epoch, start+int64(position), line))))
		encoded, _ := json.Marshal(entry)
		if !initial && (len(page.Entries) == localLogEntryLimit || outputBytes+len(encoded) > localLogOutputLimit) {
			break
		}
		page.Entries = append(page.Entries, entry)
		outputBytes += len(encoded)
		if initial {
			for len(page.Entries) > localLogEntryLimit || outputBytes > localLogOutputLimit {
				encoded, _ := json.Marshal(page.Entries[0])
				outputBytes -= len(encoded)
				page.Entries = page.Entries[1:]
			}
		}
		position = end
	}
	offset := start + int64(position)
	// The next cursor is anchored to the displayed snapshot, never a later
	// re-read that might already belong to a replacement log.
	anchorEnd := prefixSize + position
	anchor := fmt.Sprintf("%x", sha256.Sum256(captured[max(0, anchorEnd-256):anchorEnd]))
	encoded, _ := json.Marshal(localLogCursor{Version: 1, File: identity, Epoch: epoch, Offset: offset, Anchor: anchor, Skip: skip})
	page.Cursor = base64.RawURLEncoding.EncodeToString(encoded)
	// No-progress or a trailing partial line waits for the normal polling cadence.
	page.HasMore = offset < size && position > 0 && (start+int64(len(data)) < size || position == len(data) || bytes.Contains(data[position:], []byte{'\n'}))
	return page, nil
}

func parseLocalLogEntry(line string) LocalLogEntry {
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
	} else if strings.HasPrefix(strings.TrimSpace(line), "{") {
		// A damaged JSON record must not turn non-allowlisted payload fields
		// into visible plaintext. Legacy plain-text diagnostics remain useful.
		entry.Level = "WARN"
		entry.Message = "Unreadable structured log entry"
	}
	entry.Time = sanitizeLocalLog(entry.Time)
	entry.Level = sanitizeLocalLog(entry.Level)
	entry.Home = sanitizeLocalLog(entry.Home)
	if len(entry.Message) > 4096 {
		entry.Message = truncateLocalLogField(entry.Message, 4096)
	}
	// Non-message fields are also untrusted file content. Bound them before the
	// JSON output budget so even a malformed diagnostic remains a small record.
	entry.Time = truncateLocalLogField(entry.Time, 128)
	entry.Level = truncateLocalLogField(entry.Level, 32)
	entry.Home = truncateLocalLogField(entry.Home, 256)
	return entry
}

func truncateLocalLogField(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return strings.ToValidUTF8(value[:limit], "") + "…"
}

func localLogsControlResponse(stateDir, cursor string) (controlResponse, error) {
	page, err := readLocalWorkerLogPage(stateDir, cursor)
	if err != nil {
		return controlResponse{Error: sanitizeLocalLog(err.Error())}, err
	}
	return controlResponse{OK: true, Logs: page.Entries, LogCursor: page.Cursor, LogReset: page.Reset, LogHasMore: page.HasMore}, nil
}
