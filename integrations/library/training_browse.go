package library

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

type lessonBrowseCursor struct {
	AsOf    string `json:"asOf"`
	AfterAt string `json:"afterAt"`
	AfterID string `json:"afterId"`
	Search  string `json:"search"`
}

// Current-head folding must precede search: matching an old revision must never
// resurrect an enabled lesson that the owner subsequently rejected or retired.
// Every source page uses the caller's normal query admission. The browser gets
// ten heads, while the server still scans this owner's immutable revision log.
func (i *Integration) handleTrainingLessonsPage(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil || ac.UserId == "" {
		return nil, fmt.Errorf("lesson browsing requires an authenticated caller")
	}
	if i.engine == nil {
		return nil, fmt.Errorf("library engine is unavailable")
	}
	search := strings.ToLower(strings.TrimSpace(asString(args["search"])))
	cursor := lessonBrowseCursor{AsOf: time.Now().UTC().Format(time.RFC3339Nano), Search: search}
	if raw := asString(args["cursor"]); raw != "" {
		if len(raw) > 4096 {
			return nil, fmt.Errorf("invalid lesson cursor")
		}
		data, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.Search != search || cursor.AfterID == "" {
			return nil, fmt.Errorf("invalid lesson cursor")
		}
		asOf, err := time.Parse(time.RFC3339Nano, cursor.AsOf)
		if err != nil || asOf.After(time.Now().UTC()) {
			return nil, fmt.Errorf("invalid lesson cursor timestamp")
		}
		if _, err = time.Parse(time.RFC3339Nano, cursor.AfterAt); err != nil {
			return nil, fmt.Errorf("invalid lesson cursor position")
		}
	}
	call, err := parser.RenderCall("workTrainingLessonIndexRows", map[string]any{"asOf": cursor.AsOf})
	if err != nil {
		return nil, err
	}
	heads := map[string]trainingLesson{}
	err = memql.WalkQueryPages(ctx, i.engine.Execute, "query "+call, 10000, func(result *memql.ExecuteResult) error {
		for _, row := range memql.MaterializeRows(result.OutputPayload()) {
			lesson, err := lessonFromRow(row)
			if err != nil {
				return err
			}
			if held, ok := heads[lesson.ID]; !ok || lesson.Sequence > held.Sequence {
				heads[lesson.ID] = lesson
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read lesson heads: %w", err)
	}
	page, next, err := lessonHeadPage(heads, search, cursor)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{"lessons": page, "nextCursor": next})
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: "training-lessons-page", Concept: resultConcept, Type: memorynodes.NodeTypeObject, CreatedAt: time.Now().UTC(), Payload: payload}}, nil
}

func lessonHeadPage(heads map[string]trainingLesson, search string, cursor lessonBrowseCursor) ([]trainingLesson, string, error) {
	rows := make([]trainingLesson, 0, len(heads))
	for _, lesson := range heads {
		if _, err := time.Parse(time.RFC3339Nano, lesson.At); err != nil {
			return nil, "", fmt.Errorf("lesson receipt has no valid timestamp")
		}
		if search != "" && !strings.Contains(strings.ToLower(strings.Join([]string{lesson.Draft.Title, lesson.Draft.Text, lesson.Draft.Applies, lesson.Draft.Exceptions}, "\n")), search) {
			continue
		}
		if cursor.AfterID != "" {
			after, _ := time.Parse(time.RFC3339Nano, cursor.AfterAt)
			at, _ := time.Parse(time.RFC3339Nano, lesson.At)
			if at.After(after) || at.Equal(after) && lesson.ID <= cursor.AfterID {
				continue
			}
		}
		rows = append(rows, lesson)
	}
	sort.Slice(rows, func(a, b int) bool {
		at, _ := time.Parse(time.RFC3339Nano, rows[a].At)
		bt, _ := time.Parse(time.RFC3339Nano, rows[b].At)
		if at.Equal(bt) {
			return rows[a].ID < rows[b].ID
		}
		return at.After(bt)
	})
	next := ""
	if len(rows) > 10 {
		rows = rows[:10]
		last := rows[len(rows)-1]
		cursor.AfterAt = last.At
		cursor.AfterID = last.ID
		data, err := json.Marshal(cursor)
		if err != nil {
			return nil, "", err
		}
		next = base64.RawURLEncoding.EncodeToString(data)
	}
	return rows, next, nil
}
