package main

import (
	"strings"
	"testing"
)

func TestRewriteAttributes(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{
			name: "mutate becomes mutation",
			in:   "mutate folder createFolder {\n  insert { id: args.id }\n}\n",
			want: "mutation folder createFolder {\n  insert { id: args.id }\n}\n",
		},
		{
			name: "nocache becomes cache zero",
			in:   "@nocache\nquery user q {\n  filter active==true\n}\n",
			want: "@cache(0)\nquery user q {\n  filter active==true\n}\n",
		},
		{
			name: "cache keyword becomes positional",
			in:   "@cache(ttl=\"300\")\nquery user q {\n  filter active==true\n}\n",
			want: "@cache(300)\nquery user q {\n  filter active==true\n}\n",
		},
		{
			name: "schedule becomes trigger schedule",
			in:   "@schedule(cron=\"0 0 * * * *\")\nautomation sweep {\n  body { }\n}\n",
			want: "@trigger(schedule=\"0 0 * * * *\")\nautomation sweep {\n  body { }\n}\n",
		},
		{
			name: "provider type becomes vendor",
			in:   "@base\n@type(\"OpenAI\")\nprovider openai {\n  auth { }\n}\n",
			want: "@base\n@vendor(\"OpenAI\")\nprovider openai {\n  auth { }\n}\n",
		},
		{
			name: "a concept's @type is the row kind and survives",
			in:   "@type(\"collection\")\nconcept bag {\n  name string\n}\n",
			want: "@type(\"collection\")\nconcept bag {\n  name string\n}\n",
		},
		{
			name: "the dead function attributes are stripped",
			in:   "@deprecated(\"old\")\n@timeout(\"30s\")\n@audit\n@idempotent\n@retry(count=3)\n@description(\"keep me\")\nquery user q {\n  filter active==true\n}\n",
			want: "@description(\"keep me\")\nquery user q {\n  filter active==true\n}\n",
		},
		{
			name: "enabled is stripped and disabled is kept",
			in:   "@enabled\n@disabled\nquery user q {\n  filter active==true\n}\n",
			want: "@disabled\nquery user q {\n  filter active==true\n}\n",
		},
		{
			name: "latestMode, rateLimit and scopes are stripped",
			in:   "@latestMode\n@rateLimit(maxCalls=100, periodSeconds=3600)\n@scopes(\"operator\")\n@description(\"keep\")\ntool t {\n  limit integer\n}\n",
			want: "@description(\"keep\")\ntool t {\n  limit integer\n}\n",
		},
		{
			name: "concept field annotations are stripped and description kept",
			in:   "concept thing {\n  email string @unique @description(\"keep\")\n  active bool @default(\"true\")\n  createdBy string @immutable\n}\n",
			want: "concept thing {\n  email string @description(\"keep\")\n  active bool\n  createdBy string\n}\n",
		},
		{
			name: "a tool field keeps its default -- the body IS the schema",
			in:   "@handler(type=\"query\", query=\"concept==v1:ns:c\")\ntool searchUsers {\n  limit integer @default(\"10\")\n}\n",
			want: "@handler(type=\"query\", query=\"concept==v1:ns:c\")\ntool searchUsers {\n  limit integer @default(\"10\")\n}\n",
		},
		{
			name: "a prompt field keeps its default too",
			in:   "@level(\"fast\")\nprompt agentReply {\n  tone string @default(\"neutral\")\n}\n",
			want: "@level(\"fast\")\nprompt agentReply {\n  tone string @default(\"neutral\")\n}\n",
		},
		{
			name: "a provider's @default marker survives",
			in:   "@extends(\"openai\")\n@default\nprovider chat {\n  params { }\n}\n",
			want: "@extends(\"openai\")\n@default\nprovider chat {\n  params { }\n}\n",
		},
		{
			name: "allowedRoles is untouched -- it has a live reader",
			in:   "@allowedRoles(\"assistant\")\n@handler(type=\"function\", name=\"x\")\ntool ensureAgent {\n  q string\n}\n",
			want: "@allowedRoles(\"assistant\")\n@handler(type=\"function\", name=\"x\")\ntool ensureAgent {\n  q string\n}\n",
		},
		{
			name: "displayCard and composable are untouched -- clients/os reads them",
			in:   "@displayCard(primary=\"name\")\n@composable(fields=\"name\")\nconcept thing {\n  name string\n}\n",
			want: "@displayCard(primary=\"name\")\n@composable(fields=\"name\")\nconcept thing {\n  name string\n}\n",
		},
		{
			name: "a concept's @version is id-bearing and survives",
			in:   "@version(\"1.0.0\")\nconcept thing {\n  name string\n}\n",
			want: "@version(\"1.0.0\")\nconcept thing {\n  name string\n}\n",
		},
		{
			name: "a function's @version is dead and goes",
			in:   "@version(\"2.0.0\")\n@description(\"keep\")\nquery user q {\n  filter active==true\n}\n",
			want: "@description(\"keep\")\nquery user q {\n  filter active==true\n}\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rewriteAttributes([]byte(tc.in))
			if err != nil {
				t.Fatalf("rewriteAttributes: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("mismatch\n--- got ---\n%s--- want ---\n%s", got, tc.want)
			}
		})
	}
}

// TestRewriteAttributesLeavesCommentProseAlone is the guard that matters most
// for a repo whose doc comments discuss the annotations by name. Deciding
// against the raw source instead of the comment-blanked view would rewrite
// every one of these mentions, quietly editing documentation into nonsense.
func TestRewriteAttributesLeavesCommentProseAlone(t *testing.T) {
	src := "// @enabled is an explicit no-op; use @disabled instead.\n" +
		"// A field's @default was never applied, and @unique had no check.\n" +
		"/* @nocache and @cache(ttl=\"0\") said the same thing. */\n" +
		"@description(\"live\")\nquery user q {\n  filter active==true\n}\n"
	got, err := rewriteAttributes([]byte(src))
	if err != nil {
		t.Fatalf("rewriteAttributes: %v", err)
	}
	if string(got) != src {
		t.Errorf("comment prose was rewritten\n--- got ---\n%s--- want ---\n%s", got, src)
	}
}

// TestRewriteAttributesIsIdempotent: a codemod run twice must produce the
// same bytes as running it once, or a repeated `-w` pass over a tree that is
// already migrated starts corrupting it.
func TestRewriteAttributesIsIdempotent(t *testing.T) {
	src := "@enabled\n@nocache\n@cache(ttl=\"300\")\nmutate folder f {\n  insert { id: args.id }\n}\n"
	once, err := rewriteAttributes([]byte(src))
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	twice, err := rewriteAttributes(once)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if string(once) != string(twice) {
		t.Errorf("not idempotent\n--- once ---\n%s--- twice ---\n%s", once, twice)
	}
	if strings.Contains(string(once), "@enabled") || strings.Contains(string(once), "mutate ") {
		t.Errorf("first pass did not migrate: %s", once)
	}
}
