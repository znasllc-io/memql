package emailrules

import (
	"context"
	"fmt"
	"github.com/znasllc-io/memql/component/memql"
	"testing"
)

type recipientPageEngine struct {
	calls int
	bad   string
}

func (e *recipientPageEngine) Execute(ctx context.Context, _ string) (any, error) {
	e.calls++
	cursor := memql.CursorFromContext(ctx)
	if e.calls == 1 && cursor != "" {
		return nil, fmt.Errorf("inherited unrelated cursor")
	}
	if !memql.FreshReadFromContext(ctx) {
		return nil, fmt.Errorf("not fresh")
	}
	if e.bad == "missing" {
		return map[string]any{"output": []any{}, "meta": map[string]any{"hasMore": true}}, nil
	}
	if e.bad == "repeat" || cursor == "" {
		return map[string]any{"output": []any{map[string]any{"id": "first"}}, "cursor": "page2"}, nil
	}
	return map[string]any{"output": []any{map[string]any{"id": "last"}}}, nil
}
func TestAudienceRuleRecipientWalkFollowsAndValidatesContinuation(t *testing.T) {
	for _, bad := range []string{"", "missing", "repeat"} {
		t.Run(bad, func(t *testing.T) {
			e := &recipientPageEngine{bad: bad}
			ids := []string{}
			err := NewStore(e).EachSendableRecipient(memql.ContextWithCursor(context.Background(), "unrelated"), "audience", func(r map[string]any) bool { ids = append(ids, str(r, "id")); return true })
			if bad != "" {
				if err == nil {
					t.Fatal("incomplete traversal accepted")
				}
				return
			}
			if err != nil || e.calls != 2 || len(ids) != 2 || ids[1] != "last" {
				t.Fatalf("pages=%d ids=%v err=%v", e.calls, ids, err)
			}
		})
	}
}
