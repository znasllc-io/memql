package reviewspack

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type directoryCursor struct {
	Version int       `json:"v"`
	Key     string    `json:"key"`
	At      time.Time `json:"at"`
	ID      string    `json:"id"`
}

func reviewDirectoryKey(kind, store, site, state, search string) string {
	raw, _ := json.Marshal([]string{kind, store, site, state, search})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func readDirectoryCursor(after, key string) (*directoryCursor, int, error) {
	if after == "" {
		return nil, 0, nil
	}
	// Accept cursors already issued by the old offset API. New pages always
	// return a keyset cursor so inserts/removals do not shift later pages.
	if n, err := strconv.Atoi(after); err == nil && n >= 0 {
		return nil, n, nil
	}
	fail := fmt.Errorf("reviews: this page cursor does not match the directory")
	if len(after) > 2048 {
		return nil, 0, fail
	}
	raw, err := base64.RawURLEncoding.DecodeString(after)
	var cursor directoryCursor
	if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Version != 1 || cursor.Key != key || cursor.ID == "" {
		return nil, 0, fail
	}
	return &cursor, 0, nil
}
func writeDirectoryCursor(key string, at time.Time, id string) string {
	raw, _ := json.Marshal(directoryCursor{Version: 1, Key: key, At: at, ID: id})
	return base64.RawURLEncoding.EncodeToString(raw)
}
func afterDirectoryCursor(at time.Time, id string, cursor *directoryCursor, oldestFirst, idDescending bool) bool {
	if cursor == nil {
		return true
	}
	if at.Equal(cursor.At) {
		if idDescending {
			return id < cursor.ID
		}
		return id > cursor.ID
	}
	if oldestFirst {
		return at.After(cursor.At)
	}
	return at.Before(cursor.At)
}
func reviewSearchMatches(search string, values ...string) bool {
	return search == "" || strings.Contains(strings.ToLower(strings.Join(values, " ")), search)
}
