package memoryNodes

import (
	"strings"
	"testing"
)

func TestParseConcept_DisplayCard_FullCard(t *testing.T) {
	src := []byte(`
@description("Example concept with a full displayCard.")
@displayCard(primary="name", secondary="role", tertiary="ownerUserId", status="active")
concept agent {
  name         string  @required
  role         enum("owner", "admin", "writer")
  ownerUserId  string
  active       bool    @default("true")
}
`)
	c, err := ParseConceptMemQL(src, "v1/agents/agent")
	if err != nil {
		t.Fatalf("ParseConceptMemQL: %v", err)
	}
	if c.DisplayCard == nil {
		t.Fatal("expected DisplayCard to be populated")
	}
	if got := c.DisplayCard.Primary; got != "name" {
		t.Errorf("Primary = %q, want name", got)
	}
	if got := c.DisplayCard.Secondary; got != "role" {
		t.Errorf("Secondary = %q, want role", got)
	}
	if got := c.DisplayCard.Tertiary; got != "ownerUserId" {
		t.Errorf("Tertiary = %q, want ownerUserId", got)
	}
	if got := c.DisplayCard.Status; got != "active" {
		t.Errorf("Status = %q, want active", got)
	}
}

func TestParseConcept_DisplayCard_PrimaryOnly(t *testing.T) {
	src := []byte(`
@displayCard(primary="title")
concept space {
  title  string  @required
}
`)
	c, err := ParseConceptMemQL(src, "v1/cognition/space")
	if err != nil {
		t.Fatalf("ParseConceptMemQL: %v", err)
	}
	if c.DisplayCard == nil {
		t.Fatal("DisplayCard must be set even when only primary is supplied")
	}
	if c.DisplayCard.Primary != "title" {
		t.Errorf("Primary = %q, want title", c.DisplayCard.Primary)
	}
	if c.DisplayCard.Secondary != "" || c.DisplayCard.Tertiary != "" || c.DisplayCard.Status != "" {
		t.Errorf("optional slots must be empty when omitted; got %+v", c.DisplayCard)
	}
}

func TestParseConcept_DisplayCard_AbsentLeavesNilCard(t *testing.T) {
	src := []byte(`
@description("No displayCard annotation.")
concept curriculum {
  slug  string  @required
}
`)
	c, err := ParseConceptMemQL(src, "v1/curriculum/curriculum")
	if err != nil {
		t.Fatalf("ParseConceptMemQL: %v", err)
	}
	if c.DisplayCard != nil {
		t.Errorf("expected nil DisplayCard when annotation absent; got %+v", c.DisplayCard)
	}
}

func TestParseConcept_DisplayCard_RejectsMissingPrimary(t *testing.T) {
	src := []byte(`
@displayCard(secondary="role")
concept agent {
  role  string
}
`)
	_, err := ParseConceptMemQL(src, "v1/agents/agent")
	if err == nil {
		t.Fatal("expected error when @displayCard omits primary")
	}
	if !strings.Contains(err.Error(), "primary") {
		t.Errorf("error should mention the missing primary arg; got %q", err)
	}
}

func TestParseConcept_DisplayCard_RejectsUnknownArg(t *testing.T) {
	src := []byte(`
@displayCard(primary="name", quaternary="oops")
concept agent {
  name  string  @required
}
`)
	_, err := ParseConceptMemQL(src, "v1/agents/agent")
	if err == nil {
		t.Fatal("expected error on unknown arg")
	}
	if !strings.Contains(err.Error(), "quaternary") {
		t.Errorf("error should name the unknown arg; got %q", err)
	}
}

func TestParseConcept_DisplayCard_RejectsNonexistentField(t *testing.T) {
	src := []byte(`
@displayCard(primary="missing_field")
concept agent {
  name  string  @required
}
`)
	_, err := ParseConceptMemQL(src, "v1/agents/agent")
	if err == nil {
		t.Fatal("expected error when displayCard references a field that doesn't exist on the concept")
	}
	if !strings.Contains(err.Error(), "missing_field") {
		t.Errorf("error should name the missing field; got %q", err)
	}
}

func TestParseConcept_DisplayCard_RejectsObjectTypeField(t *testing.T) {
	// Object / nested types don't reduce to a single sensible cell;
	// pointing a slot at one must error.
	src := []byte(`
@displayCard(primary="capabilities")
concept agent {
  capabilities {
    avatar  bool  @default("false")
  }
}
`)
	_, err := ParseConceptMemQL(src, "v1/agents/agent")
	if err == nil {
		t.Fatal("expected error when displayCard references an object field")
	}
	if !strings.Contains(err.Error(), "not displayable") && !strings.Contains(err.Error(), "displayable") {
		t.Errorf("error should call out the type as not displayable; got %q", err)
	}
}

func TestParseConcept_DisplayCard_AcceptsEveryDisplayableType(t *testing.T) {
	// One displayCard with each accepted type in a different slot
	// pins the displayable-types allowlist.
	src := []byte(`
@displayCard(primary="title", secondary="kind", tertiary="createdMillis", status="active")
concept thing {
  title          string  @required
  kind           enum("a", "b")
  createdMillis  int
  active         bool
}
`)
	c, err := ParseConceptMemQL(src, "v1/test/thing")
	if err != nil {
		t.Fatalf("ParseConceptMemQL: %v", err)
	}
	if c.DisplayCard == nil {
		t.Fatal("expected DisplayCard to be populated")
	}
}

func TestIsDisplayableType(t *testing.T) {
	// Pin the allowlist directly so a future "let's also accept
	// array of strings" patch surfaces as a red test.
	cases := []struct {
		typ string
		ok  bool
	}{
		{"string", true},
		{"enum", true},
		{"bool", true},
		{"boolean", true},
		{"datetime", true},
		{"int", true},
		{"integer", true},
		{"float", true},
		{"number", true},
		{"STRING", true}, // case-insensitive
		{"  string  ", true},
		{"object", false},
		{"array", false},
		{"map", false},
		{"", false},
		{"unknown_type", false},
	}
	for _, tc := range cases {
		got := isDisplayableType(tc.typ)
		if got != tc.ok {
			t.Errorf("isDisplayableType(%q) = %v, want %v", tc.typ, got, tc.ok)
		}
	}
}
