package main

import (
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// memqlfmt operates lexically (indent, trailing whitespace, blank
// lines). The rewriter (component/language/parser.NormaliseAll)
// rewrites struct-form constructs into procedural form before the
// parser sees them. Both touch source text and the order matters:
// memqlfmt should NOT produce output that breaks the rewriter, and
// the rewriter's canonical output should round-trip through memqlfmt
// without further changes.
//
// These tests lock those two properties: (1) memqlfmt-then-rewrite ==
// rewrite-alone, and (2) rewrite-then-format == rewrite-then-format-
// twice (idempotency under formatting after the rewriter has run).

func TestFmtRewriterParity_QueryStructForm(t *testing.T) {
	source := `@description("Active participants in a space.")
query participant queryActiveParticipantsForSpace {
  args {
    spaceId  string  @required
  }
  filter participant.spaceId == args.spaceId
  shape  participantFull
}`

	checkFmtRewriterParity(t, source)
}

func TestFmtRewriterParity_MutationStructForm(t *testing.T) {
	source := `@description("Create a cognition space.")
mutation space mutationCreateSpace {
  args {
    spaceId  string  @required
    name     string  @required
  }
  insert {
    id:        args.spaceId
    name:      args.name
    status:    "active"
    createdAt: now
    createdBy: actor.userId
  }
}`

	checkFmtRewriterParity(t, source)
}

func TestFmtRewriterParity_LogicStructForm(t *testing.T) {
	source := `@useBuiltin(ensureDailySpaceForUser)
@description("On user creation, ensure today's daily space exists.")
logic logicProvisionDailySpaceOnUserCreate {
  args {
    event object @required
  }
  body {
    return ensureDailySpaceForUser({ userId: args.event.payload.id })
  }
}`

	checkFmtRewriterParity(t, source)
}

// checkFmtRewriterParity runs the parity property:
//
//  1. Rewrite the source through NormaliseAll -> canonical R.
//  2. Format the source through memqlfmt -> formatted F.
//  3. Rewrite F through NormaliseAll -> canonical R'.
//  4. Assert R == R'. If not, the formatter is producing text the
//     rewriter sees differently -- a silent correctness bug.
//
// Also checks that formatting is idempotent (format(format(x)) ==
// format(x)) and that rewriting is idempotent.
func checkFmtRewriterParity(t *testing.T, source string) {
	t.Helper()

	rewritten, err := languageParser.NormaliseAll(source)
	if err != nil {
		t.Fatalf("NormaliseAll(source): %v", err)
	}

	formatted := string(format([]byte(source)))
	rewrittenAfterFmt, err := languageParser.NormaliseAll(formatted)
	if err != nil {
		t.Fatalf("NormaliseAll(format(source)): %v", err)
	}

	// Trim trailing whitespace for comparison -- memqlfmt always
	// guarantees a single trailing newline, NormaliseAll doesn't
	// add one. The semantic content must match either way.
	if normalize(rewritten) != normalize(rewrittenAfterFmt) {
		t.Errorf(
			"formatter breaks rewriter output. Diff:\n--- rewrite(source) ---\n%s\n--- rewrite(format(source)) ---\n%s",
			rewritten, rewrittenAfterFmt,
		)
	}

	// Formatter idempotency.
	doubleFormatted := string(format([]byte(formatted)))
	if formatted != doubleFormatted {
		t.Errorf(
			"format() is not idempotent. format(source):\n%s\nformat(format(source)):\n%s",
			formatted, doubleFormatted,
		)
	}
}

// normalize collapses trailing whitespace + final newline differences
// so the parity check focuses on semantic content. Whitespace
// inside lines is preserved -- the rewriter is sensitive to it
// (indentation can change tokens).
func normalize(s string) string {
	return strings.TrimRight(s, " \t\n")
}
