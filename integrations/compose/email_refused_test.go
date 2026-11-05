package compose

import (
	"testing"
)

// emailsAreMadeElsewhere is the one refusal every way of asking the
// Materializer for an email gets.
const emailsAreMadeElsewhere = "Emails are made in the Email app."

// A request for an email is refused where every entry point arrives, whatever
// else it names, and before anything is written or read.
func TestAnEmailRequestIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"a blank email":                 {"name": "Welcome", "format": "json", "outputKind": "email_template", "accountIds": []any{emailAccount}},
		"a described email":             {"name": "Welcome", "format": "json", "outputKind": "email_template", "accountIds": []any{emailAccount}, "statement": "A welcome"},
		"an email with a layout":        {"name": "Welcome", "format": "json", "outputKind": "email_template", "accountIds": []any{emailAccount}, "layoutId": "builtin:letter"},
		"an email with no organization": {"name": "Welcome", "format": "json", "outputKind": "email_template"},
		"an email in another format":    {"name": "Welcome", "format": "markdown", "outputKind": "email_template", "accountIds": []any{emailAccount}},
	} {
		t.Run(name, func(t *testing.T) {
			i, e, _ := emailFixture(t)
			if _, err := i.handleMaterialize(nestedMaterializeContext(), args, 0); err == nil || err.Error() != emailsAreMadeElsewhere {
				t.Fatalf("refusal = %v, want %q", err, emailsAreMadeElsewhere)
			}
			if len(e.calls) != 0 || len(e.compositions) != 0 || len(e.inputs) != 0 {
				t.Fatalf("a refused email reached the engine: %v %v %v", e.calls, e.compositions, e.inputs)
			}
		})
	}
}

// A recipe that was saved to make emails is refused the same way when it is run.
func TestARecipeThatMakesEmailsIsRefusedWhenRun(t *testing.T) {
	i, e, _ := emailFixture(t)
	opener := &directMaterializeGoal{}
	i.SetGoalOpener(opener)
	e.recipe = map[string]any{"id": "welcome-recipe", "name": "Welcome", "format": "json", "outputKind": "email_template", "layoutId": "builtin:letter",
		"description": "A welcome for new subscribers.", "accountIds": []any{emailAccount}}
	if _, err := i.handleRunRecipe(materializeContext(), map[string]any{"recipeId": "welcome-recipe"}, 0); err == nil || err.Error() != emailsAreMadeElsewhere {
		t.Fatalf("refusal = %v, want %q", err, emailsAreMadeElsewhere)
	}
	if opener.calls != 0 || len(e.compositions) != 0 || len(e.inputs) != 0 {
		t.Fatalf("a refused recipe run wrote rows: opens=%d %v %v", opener.calls, e.compositions, e.inputs)
	}
}

// A layout is an email's, and no longer a Materializer argument at all.
func TestALayoutIdIsNoLongerAMaterializerArgument(t *testing.T) {
	i, e, _ := materializeFixture(t)
	if _, err := i.handleMaterialize(nestedMaterializeContext(), map[string]any{"name": "Notes", "format": "markdown", "layoutId": "builtin:letter"}, 0); err == nil || err.Error() != emailsAreMadeElsewhere {
		t.Fatalf("refusal = %v, want %q", err, emailsAreMadeElsewhere)
	}
	if len(e.calls) != 0 {
		t.Fatalf("the refusal reached the engine: %v", e.calls)
	}
}
