package releasecatalog

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type searchPosition struct {
	Version int    `json:"version"`
	Source  string `json:"source"`
	Search  string `json:"search"`
	Cursor  string `json:"cursor"`
	Offset  int    `json:"offset"`
}

// Remote publisher protocols need not implement search. Scan their bounded,
// independently verified pages on the server, retaining only ten matches and
// an opaque continuation. No unverified hint can become a search result.
func (r *Reader) Search(ctx context.Context, source, cursor, search string, limit int) (Page, error) {
	search = strings.ToLower(strings.TrimSpace(search))
	if search == "" {
		return r.List(ctx, source, cursor, limit)
	}
	if err := admit(ctx); err != nil {
		return Page{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return searchPages(ctx, source, cursor, search, limit, func(ctx context.Context, cursor string) (Page, error) { return r.List(ctx, source, cursor, maxPage) })
}

func searchPages(ctx context.Context, source, cursor, search string, limit int, read func(context.Context, string) (Page, error)) (Page, error) {
	if len(search) > 512 || len(cursor) > 4096 || limit < 1 || limit > maxPage {
		return Page{}, errors.New("release search requires bounded text, cursor and a limit from 1 to 10")
	}
	position := searchPosition{Version: 1, Source: source, Search: search}
	if cursor != "" {
		data, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || decode(data, &position) != nil || position.Version != 1 || position.Source != source || position.Search != search || position.Offset < 0 || position.Offset >= maxPage || len(position.Cursor) > 1024 {
			return Page{}, errors.New("invalid release search cursor")
		}
	}
	out := Page{SourceID: source, Releases: []ReleaseSummary{}}
	seen := map[string]bool{}
	for pages := 0; pages < 1000; pages++ {
		if err := ctx.Err(); err != nil {
			return Page{}, err
		}
		if seen[position.Cursor] {
			return Page{}, errors.New("release search encountered a repeated publisher cursor")
		}
		seen[position.Cursor] = true
		page, err := read(ctx, position.Cursor)
		if err != nil {
			return Page{}, err
		}
		if page.SourceID != source || position.Offset > len(page.Releases) {
			return Page{}, errors.New("release search page changed; start the search again")
		}
		out.Publisher = page.Publisher
		for n := position.Offset; n < len(page.Releases); n++ {
			release := page.Releases[n]
			fields := []string{release.CandidateID}
			for _, c := range release.Components {
				fields = append(fields, c.Name, c.Version, c.Repository, c.Commit)
			}
			if strings.Contains(strings.ToLower(strings.Join(fields, "\n")), search) {
				out.Releases = append(out.Releases, release)
			}
			if len(out.Releases) == limit {
				position.Offset = n + 1
				if position.Offset == len(page.Releases) {
					position.Cursor = page.NextCursor
					position.Offset = 0
					if position.Cursor == "" {
						return out, nil
					}
				}
				data, err := json.Marshal(position)
				if err != nil {
					return Page{}, err
				}
				out.NextCursor = base64.RawURLEncoding.EncodeToString(data)
				return out, nil
			}
		}
		if page.NextCursor == "" {
			return out, nil
		}
		position.Cursor = page.NextCursor
		position.Offset = 0
	}
	return Page{}, errors.New("release search exceeded its inspection window; narrow the search")
}
