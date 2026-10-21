package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/id"
)

func TestNormalizeAskAttachmentIDs(t *testing.T) {
	got, err := normalizeAskAttachmentIDs([]string{" artifact-a ", "", "v1:library:artifact:artifact-b", "artifact-a"})
	require.NoError(t, err)
	require.Equal(t, []string{"artifact-a", "artifact-b"}, got, "blank and repeated ids drop, canonical ids go bare, order holds")

	_, err = normalizeAskAttachmentIDs([]string{"has space"})
	var refusal *AskAttachmentError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, AskAttachmentInvalid, refusal.Code)

	many := make([]string, 0, MaxAskAttachments+1)
	for i := range MaxAskAttachments + 1 {
		many = append(many, fmt.Sprintf("artifact-%d", i))
	}
	_, err = normalizeAskAttachmentIDs(many)
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, AskAttachmentTooMany, refusal.Code)

	none, err := normalizeAskAttachmentIDs(nil)
	require.NoError(t, err)
	require.Empty(t, none)
}

func TestAskAttachmentFromAnArtifactRow(t *testing.T) {
	file := askAttachmentFrom("artifact-a", map[string]any{"title": "Q3 plan.pdf", "mimeType": "application/pdf", "kind": "file", "sourceConceptRef": "v1:library:file:file-a"})
	require.Equal(t, AskAttachment{ArtifactID: "artifact-a", FileID: "file-a", Name: "Q3 plan.pdf", MimeType: "application/pdf", Kind: "file"}, file)

	note := askAttachmentFrom("artifact-b", map[string]any{"kind": "note", "sourceConceptRef": "v1:notes:note:n1"})
	require.Empty(t, note.FileID, "only a Library file has a file id the read capability takes")
	require.Equal(t, "Untitled file", note.Name)

	input := askAttachmentsInput([]AskAttachment{file, note})
	raw, err := json.Marshal(input)
	require.NoError(t, err)
	require.JSONEq(t, `[{"artifactId":"artifact-a","fileId":"file-a","name":"Q3 plan.pdf","mimeType":"application/pdf","kind":"file"},{"artifactId":"artifact-b","name":"Untitled file","kind":"note"}]`, string(raw))
}

// The files a person attaches are resolved UNDER THEM before anything is
// written, recorded on the transcript turn, and carried on the goal's
// conversation input -- the run row the planner and agent rehydrate on
// whichever replica runs them. Somebody else's artifact refuses the turn.
func TestAskAttachmentsResolveUnderTheCallerAndRideTheGoal(t *testing.T) {
	e, _, _ := sharedReadMergeEngine(t)
	person := func(user string) context.Context {
		return auth.ContextWithAccess(auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: user}), &auth.AccessContext{UserId: user, Role: auth.RoleWriter})
	}
	mineUser := "v1:identity:user:" + id.NewShortId()
	theirsUser := "v1:identity:user:" + id.NewShortId()
	mine := person(mineUser)
	artifactFor := func(owner, title string) string {
		call, err := parser.RenderCall("createArtifact", map[string]any{
			"sourceConceptRef": "v1:library:file:" + id.NewShortId(), "ownerUserId": owner,
			"lens": "artifact", "kind": "file", "source": "uploaded", "title": title, "mimeType": "text/markdown",
		})
		require.NoError(t, err)
		result, err := e.Execute(auth.ContextWithInternalOrigin(person(owner)), "mutation "+call)
		require.NoError(t, err)
		rows := MaterializeRows(result.OutputPayload())
		require.Len(t, rows, 1)
		return BareShortId(fmt.Sprint(rows[0]["id"]))
	}
	myFile := artifactFor(mineUser, "notes.md")
	theirFile := artifactFor(theirsUser, "private.md")

	var goalInput map[string]any
	calls := 0
	e.builtinExecutorHandlers["integration.work.createGoal"] = func(callCtx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		calls++
		goalInput, _ = args["input"].(map[string]any)
		runID := "v1:work:run:" + id.NewShortId()
		call, err := parser.RenderCall("createWorkRun", map[string]any{"runId": runID, "goalId": "goal", "automationName": "shared", "templateFingerprint": "test", "triggeredBy": "manual", "status": "succeeded", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339)})
		require.NoError(t, err)
		_, err = e.Execute(auth.ContextWithInternalOrigin(callCtx), "mutation "+call)
		require.NoError(t, err)
		raw, _ := json.Marshal(map[string]any{"goalId": "goal", "runId": runID})
		return []memorynodes.MemoryNode{{ID: "intake", Payload: raw}}, nil
	}
	result, err := e.Execute(mine, `mutation createAskConversation(requestId: "attached", title: "New conversation")`)
	require.NoError(t, err)
	conversationID := fmt.Sprint(MaterializeRows(result)[0]["id"])

	_, err = e.RunAsk(mine, conversationID, "refused", "read this", "", AskRoute{}, []string{theirFile}, nil, nil)
	var refusal *AskAttachmentError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, AskAttachmentNotFound, refusal.Code)
	require.Zero(t, calls, "an attachment the person cannot read must not open a goal")
	row, err := e.askRead(mine, conversationID)
	require.NoError(t, err)
	require.NotContains(t, fmt.Sprint(row), "refused", "a refused attachment must not leave a turn behind")

	_, err = e.RunAsk(mine, conversationID, "turn", "read this", "", AskRoute{}, []string{myFile, myFile}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	conversation, _ := goalInput["conversation"].(map[string]any)
	attached, _ := conversation["attachments"].([]any)
	require.Len(t, attached, 1, "a repeated id is one attachment")
	require.Equal(t, "notes.md", attached[0].(map[string]any)["name"])
	require.NotEmpty(t, attached[0].(map[string]any)["fileId"])
	row, err = e.askRead(mine, conversationID)
	require.NoError(t, err)
	require.True(t, strings.Contains(fmt.Sprint(row["transcript"]), "notes.md"), "the turn records what was attached")
}
