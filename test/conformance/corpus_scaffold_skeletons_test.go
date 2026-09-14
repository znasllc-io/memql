package conformance

// corpus_scaffold_skeletons_test.go -- the per-receiver skeletons the cell
// scaffolder (corpus_scaffold_test.go) writes from. Each is the smallest
// construct of its receiver that loads through MemQLEngine.Init as an overlay
// domain -- and works at runtime, so an author who copies a cell gets a
// construct that does what its doc says -- with the placement's annotation
// where the receiver reads it, and a fixture holding what the construct leans
// on.
//
// Every construct name carries the annotation's name as a suffix, because the
// runner loads every case in shared boots and a bare name two domains declare
// is ambiguous to every lookup by bare name (a tool's handler, an automation's
// step, an auto-registered function tool, a seed's create<Concept>).

import (
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/language/annotations"
)

// scaffoldTicket is the concept most cells read and write.
const scaffoldTicket = `/// A support ticket: the row this cell's cases read and write.
concept ticket {
  ownerUserId  string
  title        string
  status       string
}
`

// scaffoldTicketWith is scaffoldTicket with extra fields, as name/type pairs.
func scaffoldTicketWith(extra ...[2]string) string {
	fields := append([][2]string{{"ownerUserId", "string"}, {"title", "string"}, {"status", "string"}}, extra...)
	return "/// A support ticket: the row this cell's cases read and write.\nconcept ticket {\n" + scaffoldColumns("  ", fields) + "}\n"
}

// scaffoldColumns renders name/rest pairs as aligned lines, the way the
// shipped tree lays out a field list.
func scaffoldColumns(indent string, pairs [][2]string) string {
	width := 0
	for _, p := range pairs {
		if len(p[0]) > width {
			width = len(p[0])
		}
	}
	var b strings.Builder
	for _, p := range pairs {
		b.WriteString(indent + p[0] + strings.Repeat(" ", width-len(p[0])+2) + p[1] + "\n")
	}
	return b.String()
}

// scaffoldArgs renders an args block.
func scaffoldArgs(pairs ...[2]string) string {
	if len(pairs) == 0 {
		return ""
	}
	return "  args {\n" + scaffoldColumns("    ", pairs) + "  }\n"
}

// scaffoldAnnotation is the accepted annotation a cell's first case carries:
// the registry's example, or -- where the example names something a ticket
// does not have -- the same form over the ticket's own fields.
func scaffoldAnnotation(p annotations.Placement) string {
	if s, ok := scaffoldAnnotations[string(p.Receiver)+"/"+p.Name]; ok {
		return s
	}
	return p.Example
}

var scaffoldAnnotations = map[string]string{
	"Query/description":        `@description("The open tickets, newest first.")`,
	"Query/unbounded":          `@unbounded("the open-ticket queue is small enough to read whole")`,
	"Mutation/description":     `@description("Retitle one ticket.")`,
	"Mutation/addToSet":        `@addToSet("labels")`,
	"Mutation/removeFromSet":   `@removeFromSet("labels")`,
	"Mutation/noUnset":         `@noUnset("closedAt")`,
	"Logic/description":        `@description("The headline a ticket shows: its title, or a placeholder when it has none.")`,
	"Automation/description":   `@description("When a node joins the cluster, open a ticket to welcome it.")`,
	"Automation/trigger":       `@trigger(event="node.created", concept="v1:cluster:node", partition="*")`,
	"Automation/filter":        `@filter(payload.nodeType == "agent")`,
	"Action/description":       `@description("Read a ticket's attachment from the runner's workspace.")`,
	"Capability/description":   `@description("List the entries of a directory on the runner's workspace.")`,
	"Spec/description":         `@description("Matches the tickets that are still open.")`,
	"Tool/description":         `@description("Open a support ticket for the caller.")`,
	"Tool/handler":             `@handler(type="function", name="openTicketHandler")`,
	"Builtin/description":      `@description("Today's date key in a timezone, for naming a daily ticket.")`,
	"Builtin/alias":            `@alias("ticketDayKey")`,
	"Builtin/executor":         `@executor("integration.timeutil.dateKeyInTimezone")`,
	"Prompt/description":       `@description("Summarise a ticket in one sentence.")`,
	"Prompt/templateFile":      `@templateFile("summariseTicket.tmpl")`,
	"Provider/description":     `@description("OpenAI GPT-5.4 Nano under a bundle's own name.")`,
	"Provider/model":           `@model("gpt-5.4-nano")`,
	"Provider/type":            `@type("OpenAIEmbedding")`,
	"Shape/description":        `@description("A ticket as a card: its title and status.")`,
	"Policy/description":       `@description("The strongest local model first, then any signed-in app.")`,
	"Rule/description":         `@description("Background calls resolve through the local-first chain.")`,
	"Rule/when":                `@when(tag="background")`,
	"Seed/description":         `@description("The billing category every cluster starts with.")`,
	"Seed/templateFile":        `@templateFile("triageAssistant.tmpl")`,
	"Seed/namespace":           `@namespace("support")`,
	"Concept/composable":       `@composable(as="ticket", fields="title,status", list="ticketsToCompose")`,
	"Concept/description":      `@description("A support ticket raised by a customer.")`,
	"ConceptField/default":     `@default("normal")`,
	"ConceptField/description": `@description("The ticket's one-line summary.")`,
	"ConceptField/maximum":     `@maximum(100)`,
	"ConceptField/pattern":     `@pattern("^[a-z][a-z0-9-]*$")`,
	"ConceptField/variant":     `@variant(discriminator="replyVia")`,
	"ArgsField/maximum":        `@maximum(5)`,
	"ArgsField/minimum":        `@minimum(1)`,
	"ArgsField/pattern":        `@pattern("^[A-Z]{2}$")`,
	"ToolField/default":        `@default("20")`,
	"ToolField/description":    `@description("The most tickets to return.")`,
	"ToolField/enum":           `@enum("open", "closed")`,
	"PromptField/default":      `@default("en")`,
	"PromptField/description":  `@description("The language to answer in.")`,
	"PromptField/enum":         `@enum("short", "long")`,
	"BuiltinField/description": `@description("An IANA timezone; UTC when empty.")`,
}

// scaffoldDocs is the doc comment a cell's construct carries, keyed
// "Receiver/annotation": what the construct does, and -- where the annotation
// changes that -- how.
var scaffoldDocs = map[string]string{
	"Query/actor":              "The caller's own open tickets, newest first.",
	"Query/cache":              "The open tickets, newest first. A read is cached for five minutes.",
	"Query/disabled":           "The open tickets, newest first. Switched off: not loaded while @disabled stays.",
	"Query/enabled":            "The open tickets, newest first. @enabled restates the default.",
	"Query/latestMode":         "The live tip of every open ticket, newest first: what it returns depends on when it is read.",
	"Query/mcp":                "The open tickets, newest first, exposed as their own MCP tool.",
	"Query/nocache":            "The open tickets, newest first, read live on every call.",
	"Query/public":             "The open tickets, newest first. Deliberately callable with no caller check: the queue is public.",
	"Query/requiresCapability": "The open tickets, newest first, for a caller holding read on principal.",
	"Query/requiresRank":       "The open tickets, newest first, for developers and anyone ranked above them.",
	"Query/serverOnly":         "The open tickets, newest first, for server-side callers only.",
	"Query/unbounded":          "Every open ticket at once, with no page.",

	"Mutation/actor":              "Assign a ticket to the caller.",
	"Mutation/addToSet":           "Add labels to a ticket; a label it already carries is not added twice.",
	"Mutation/appendFields":       "Attach files to a ticket, keeping the ones already attached.",
	"Mutation/createOnly":         "Open a ticket. Staging the same id again keeps the status and attempt count another writer moved on.",
	"Mutation/disabled":           "Retitle one ticket. Switched off: not loaded while @disabled stays.",
	"Mutation/enabled":            "Retitle one ticket. @enabled restates the default.",
	"Mutation/mcp":                "Retitle one ticket, exposed as its own MCP tool.",
	"Mutation/mergeFields":        "Change some of a ticket's preferences, keeping the ones the caller did not name.",
	"Mutation/noUnset":            "Close a ticket. Once set, closedAt is never taken back to empty.",
	"Mutation/public":             "Retitle one ticket. Deliberately callable with no caller check: the queue is public.",
	"Mutation/removeFromSet":      "Remove labels from a ticket; removing one it does not carry changes nothing.",
	"Mutation/requiresCapability": "Retitle one ticket, for a caller holding execute on app:deployables/publish.",
	"Mutation/requiresRank":       "Retitle one ticket, for admins and anyone ranked above them.",
	"Mutation/scrubPii":           "Forget who reported a ticket: every field the concept marks @pii is zeroed.",
	"Mutation/serverOnly":         "Retitle one ticket, for server-side callers only.",

	"Logic/actor":              "The user a ticket taken now is assigned to: the caller.",
	"Logic/disabled":           "The headline a ticket shows. Switched off: not loaded while @disabled stays.",
	"Logic/enabled":            "The headline a ticket shows: its title, or a placeholder. @enabled restates the default.",
	"Logic/eventField":         "The status the ticket in the triggering event moved to.",
	"Logic/requiresCapability": "The headline a ticket shows, for a caller holding execute on app:deployables/deploy.",
	"Logic/requiresRank":       "The headline a ticket shows, for admins and anyone ranked above them.",

	"Automation/actor":    "When a node joins the cluster, open a ticket owned by the user the automation runs as.",
	"Automation/disabled": "When a node joins the cluster, open a ticket. Switched off: not loaded while @disabled stays.",
	"Automation/enabled":  "When a node joins the cluster, open a ticket to welcome it. @enabled restates the default.",
	"Automation/filter":   "When an agent node joins the cluster, open a ticket to welcome it.",
	"Automation/mcp":      "When a node joins the cluster, open a ticket; also exposed as its own MCP tool.",
	"Automation/schedule": "Every hour, open a ticket to review the queue.",
	"Automation/template": "A work-spine template: opens a ticket with the title the run names.",
	"Automation/trigger":  "When a node joins the cluster, open a ticket to welcome it.",

	"Action/disabled": "Read a ticket's attachment from the runner's workspace. Switched off: not loaded while @disabled stays.",
	"Action/enabled":  "Read a ticket's attachment from the runner's workspace. @enabled restates the default.",

	"Capability/disabled":   "The paths on the runner's workspace that match a pattern. Switched off: declared, and refused to any action that calls it.",
	"Capability/enabled":    "Whether a path exists on the runner's workspace. @enabled restates the default.",
	"Capability/sideEffect": "Create a directory on the runner's workspace: a write, and the class says so.",

	"Spec/disabled": "Matches the tickets that are still open. Switched off: not loaded while @disabled stays.",
	"Spec/enabled":  "Matches the tickets that are still open. @enabled restates the default.",

	"Tool/allowedRoles":         "Open a support ticket; offered to assistant and specialist agents only.",
	"Tool/destructive":          "Close a ticket for good. Destructive: the tool loop confirms before it runs.",
	"Tool/disabled":             "Open a support ticket. Switched off: not loaded while @disabled stays.",
	"Tool/enabled":              "Open a support ticket for the caller. @enabled restates the default.",
	"Tool/executionTime":        "Open a support ticket; it answers quickly.",
	"Tool/handler":              "Open a support ticket for the caller.",
	"Tool/mcp":                  "Open a support ticket, on the curated MCP connector surface.",
	"Tool/rateLimit":            "Open a support ticket, at most ten times a minute.",
	"Tool/requiresConfirmation": "Close a ticket; the caller confirms before it runs.",
	"Tool/scopes":               "Open a support ticket; the caller needs the operator scope.",

	"Builtin/alias":              "Today's date key in a timezone, for naming a daily ticket; also callable as ticketDayKey.",
	"Builtin/args":               "Today's date key in a timezone, for naming a daily ticket; called with one object argument.",
	"Builtin/disabled":           "Today's date key in a timezone. Switched off: not loaded while @disabled stays.",
	"Builtin/enabled":            "Today's date key in a timezone, for naming a daily ticket. @enabled restates the default.",
	"Builtin/executor":           "Today's date key in a timezone, for naming a daily ticket.",
	"Builtin/requiresCapability": "Today's date key in a timezone, for a caller holding execute on app:deployables/deploy.",
	"Builtin/sdk":                "Today's date key in a timezone, for naming a daily ticket; generated into the SDKs.",

	"Prompt/defaultProvider": "Summarise a ticket in one sentence, pinned to the fleet provider.",
	"Prompt/disabled":        "Summarise a ticket in one sentence. Switched off: not loaded while @disabled stays.",
	"Prompt/enabled":         "Summarise a ticket in one sentence. @enabled restates the default.",
	"Prompt/level":           "Summarise a ticket in one sentence: a fast call.",
	"Prompt/templateFile":    "Summarise a ticket in one sentence, from the template beside this file.",

	"Provider/base":     "A vendor-level base for models on the user's own machines: a type, and no model of its own.",
	"Provider/default":  "OpenAI GPT-5.4 Nano under a bundle's own name, marked the default for its modality.",
	"Provider/disabled": "OpenAI GPT-5.4 Nano under a bundle's own name. Switched off: not registered while @disabled stays.",
	"Provider/enabled":  "OpenAI GPT-5.4 Nano under a bundle's own name. @enabled restates the default.",
	"Provider/extends":  "OpenAI GPT-5.4 Nano under a bundle's own name; the base supplies the vendor and the credential.",
	"Provider/modality": "OpenAI text-embedding-3-small under a bundle's own name, for embeddings.",
	"Provider/model":    "OpenAI GPT-5.4 Nano under a bundle's own name.",
	"Provider/type":     "OpenAI text-embedding-3-small under a bundle's own name: the embedding client, not the chat one.",

	"Shape/actor": "The caller as a card: who they are and their role.",
	"Shape/row":   "A ticket as a card: its title and status.",

	"Policy/fallback": "The strongest local model first, then any signed-in app.",
	"Policy/primary":  "The strongest local model, and nothing else.",

	"Rule/disabled":      "Background calls resolve through the local-first chain. Switched off: not loaded while @disabled stays.",
	"Rule/enabled":       "Background calls resolve through the local-first chain. @enabled restates the default.",
	"Rule/exclude":       "Background calls resolve through the local-first chain, never on the qwen3.5:7b fleet model.",
	"Rule/level":         "Background calls resolve through the local-first chain at the strong level.",
	"Rule/onUnavailable": "Background calls resolve through the local-first chain; an exhausted chain degrades a level.",
	"Rule/policy":        "Background calls resolve through the local-first chain.",
	"Rule/precedence":    "Background calls resolve through the local-first chain, ordered at precedence 60.",
	"Rule/when":          "Background calls resolve through the local-first chain.",

	"Seed/disabled":     "The maintenance banner, written and switched off: not seeded while @disabled stays.",
	"Seed/enabled":      "The canned thank-you reply every cluster starts with. @enabled restates the default.",
	"Seed/namespace":    "The getting-started article every cluster starts with.",
	"Seed/scope":        "Every user's own queue of the tickets they raised, seeded once per user.",
	"Seed/templateFile": "The triage assistant every cluster starts with; its system prompt is the template beside this file.",
	"Seed/version":      "The urgent escalation rule every cluster starts with, at version 1.0.0.",
}

// scaffoldDocFor is the construct doc of a placement: its own, or the
// receiver's default.
func scaffoldDocFor(p annotations.Placement, fallback string) string {
	if d, ok := scaffoldDocs[string(p.Receiver)+"/"+p.Name]; ok {
		return d
	}
	return fallback
}

// scaffoldDoc is the construct's doc comment, or nothing on a @description
// cell: the /// block and @description are two spellings of one thing, and a
// case about the annotation must not also carry the comment.
func scaffoldDoc(p annotations.Placement, fallback string) string {
	if p.Name == "description" && !scaffoldIsField(p.Receiver) {
		return ""
	}
	return "/// " + scaffoldDocFor(p, fallback) + "\n"
}

func scaffoldIsField(r annotations.Receiver) bool {
	switch r {
	case annotations.ConceptField, annotations.ArgsField, annotations.ToolField,
		annotations.PromptField, annotations.BuiltinField:
		return true
	}
	return false
}

// scaffoldSkeleton renders the fixture and the case for placement p carrying
// ann, plus any sidecar file the case reads (a prompt's or a seed's template).
func scaffoldSkeleton(p annotations.Placement, ann string) (fixture, src string, sidecars map[string]string) {
	s := upperFirst(p.Name)
	line := ann + "\n"
	switch p.Receiver {
	case annotations.Query:
		filter, tail := `status == "open"`, "  sort \"row.createdAt\", \"desc\"\n  paginate 20\n"
		switch p.Name {
		case "actor":
			filter = `ownerUserId == actor.userId && status == "open"`
		case "unbounded":
			tail = ""
		case "latestMode":
			tail += "  asOf latest\n"
		}
		return scaffoldTicket, scaffoldDoc(p, "The open tickets, newest first.") + line +
			"query ticket openTickets" + s + " {\n  filter " + filter + "\n" + tail + "}\n", nil

	case annotations.Mutation:
		return scaffoldMutation(p, s, line)

	case annotations.Logic:
		switch p.Name {
		case "actor":
			return "", scaffoldDoc(p, "") + line + "logic ticketAssignee" + s + " {\n  body {\n    return actor.userId\n  }\n}\n", nil
		case "eventField":
			return "", scaffoldDoc(p, "") + line + "logic ticketEventStatus" + s + " {\n" + scaffoldArgs([2]string{"event", "object!"}) +
				"  body {\n    return args.event.payload.status\n  }\n}\n", nil
		}
		return "", scaffoldDoc(p, "The headline a ticket shows: its title, or a placeholder when it has none.") + line +
			"logic ticketHeadline" + s + " {\n" + scaffoldArgs([2]string{"title", "string"}) +
			"  body {\n    return args.title ?? \"(untitled)\"\n  }\n}\n", nil

	case annotations.Automation:
		return scaffoldAutomation(p, s, ann)

	case annotations.Action:
		return "", "use capabilities.fs.{ readFile }\n\n" + scaffoldDoc(p, "Read a ticket's attachment from the runner's workspace.") + line +
			"action readTicketAttachment" + s + " {\n" + scaffoldArgs([2]string{"path", "string!"}) +
			"  capability readFile(path: args.path)\n}\n", nil

	case annotations.Capability:
		// A verb the Go vocabulary classifies and the embedded catalog does
		// not declare yet: the catalog refuses a name declared twice.
		v := scaffoldCapabilityVerbs[p.Name]
		verb, class, arg := v[0], v[1], v[2]
		se := `@sideEffect("` + class + `")` + "\n"
		if p.Name == "sideEffect" {
			se = ""
		}
		return "", scaffoldDoc(p, "") + se + line + "capability " + verb + " {\n" + scaffoldArgs([2]string{arg, "string!"}) + "}\n", nil

	case annotations.Spec:
		return scaffoldTicket, scaffoldDoc(p, "Matches the tickets that are still open.") + line +
			"spec ticket isOpenTicket" + s + " {\n  return status == \"open\"\n}\n", nil

	case annotations.Tool:
		if p.Name == "destructive" || p.Name == "requiresConfirmation" {
			fixture := scaffoldTicket + "\n/// Close a ticket.\nmutate ticket closeTicket" + s + " {\n" + scaffoldArgs([2]string{"ticketId", "string!"}) +
				"  update {\n    id: args.ticketId\n    status: \"closed\"\n  }\n}\n"
			return fixture, scaffoldDoc(p, "") + `@handler(type="function", name="closeTicket` + s + `")` + "\n" + line +
				"tool closeSupportTicket" + s + " {\n  ticketId  string!  @description(\"The ticket to close.\")\n}\n", nil
		}
		fixture := scaffoldTicket + "\n/// Open a ticket with a title.\nmutate ticket openTicket" + s + " {\n" + scaffoldArgs([2]string{"title", "string!"}) +
			"  insert {\n    title: args.title\n    status: \"open\"\n  }\n}\n"
		handler := `@handler(type="function", name="openTicket` + s + `")` + "\n"
		if p.Name == "handler" {
			handler = "" // the annotation under test is the handler
		}
		return fixture, scaffoldDoc(p, "Open a support ticket for the caller.") + handler + line +
			"tool openSupportTicket" + s + " {\n  title  string!  @description(\"The ticket's one-line title.\")\n}\n", nil

	case annotations.Builtin:
		exec := `@executor("integration.timeutil.dateKeyInTimezone")` + "\n"
		if p.Name == "executor" {
			exec = ""
		}
		return "", scaffoldDoc(p, "Today's date key in a timezone, for naming a daily ticket.") + exec + line +
			"builtin ticketDateKey" + s + " {\n" + scaffoldDateKeyFields("") + "}\n", nil

	case annotations.Prompt:
		head := `@level("fast")` + "\n" + `@templateFile("summariseTicket.tmpl")` + "\n"
		switch p.Name {
		case "level":
			head = `@templateFile("summariseTicket.tmpl")` + "\n"
		case "templateFile":
			head = `@level("fast")` + "\n"
		}
		return "", scaffoldDoc(p, "Summarise a ticket in one sentence.") + head + line +
				"prompt summariseTicket" + s + " {\n  title  string!  @description(\"The ticket's title.\")\n}\n",
			map[string]string{"summariseTicket.tmpl": "Summarise the support ticket titled \"{{.title}}\" in one sentence.\n"}

	case annotations.Provider:
		head := `@extends("openai")` + "\n" + `@model("gpt-5.4-nano")` + "\n"
		name := "bundleNano"
		switch p.Name {
		case "extends":
			head = `@model("gpt-5.4-nano")` + "\n"
		case "model":
			head = `@extends("openai")` + "\n"
		case "modality":
			head, name = `@extends("openai")`+"\n"+`@type("OpenAIEmbedding")`+"\n"+`@model("text-embedding-3-small")`+"\n", "bundleEmbedding"
		case "type":
			// The annotation under test sits where the vendor-level type is
			// read: between the base and the model it types.
			return "", scaffoldDoc(p, "") + `@extends("openai")` + "\n" + line + `@model("text-embedding-3-small")` + "\n" + `@modality("embedding")` + "\n" +
				"provider bundleEmbedding" + s + " { }\n", nil
		case "base":
			head, name = `@type("Fleet")`+"\n", "bundleFleet"
		}
		return "", scaffoldDoc(p, "OpenAI GPT-5.4 Nano under a bundle's own name.") + head + line + "provider " + name + s + " { }\n", nil

	case annotations.Shape:
		if p.Name == "actor" {
			return "", scaffoldDoc(p, "") + line + "shape callerCard" + s + " {\n  actor.userId\n  actor.role\n}\n", nil
		}
		head := "@row\n"
		if p.Name == "row" {
			head = ""
		}
		return scaffoldTicket, scaffoldDoc(p, "A ticket as a card: its title and status.") + head + line +
			"shape ticket ticketCard" + s + " {\n  row.id\n  title\n  status\n}\n", nil

	case annotations.Policy:
		head := `@primary("fleet:strongest")` + "\n"
		tail := `@fallback("app:*")` + "\n"
		switch p.Name {
		case "primary":
			head, tail = "", ""
		case "fallback":
			tail = ""
		}
		name := "localThenApp"
		if p.Name == "primary" {
			name = "strongestLocalOnly" // no fallback: the name says so
		}
		return "", scaffoldDoc(p, "The strongest local model first, then any signed-in app.") + head + line + tail +
			"policy " + name + s + " { }\n", nil

	case annotations.Rule:
		// Every rule a boot loads is ordered against every other of the same
		// locked-ness, and a tie is a load error: each cell has its own.
		precedence := scaffoldRulePrecedence[p.Name]
		head := `@when(tag="background")` + "\n" + `@policy("localFirst")` + "\n" + "@precedence(" + precedence + ")\n"
		switch p.Name {
		case "when":
			head = `@policy("localFirst")` + "\n" + "@precedence(" + precedence + ")\n"
		case "policy":
			head = `@when(tag="background")` + "\n" + "@precedence(" + precedence + ")\n"
		case "precedence":
			head = `@when(tag="background")` + "\n" + `@policy("localFirst")` + "\n"
		}
		return "", scaffoldDoc(p, "Background calls resolve through the local-first chain.") + head + line +
			"rule backgroundLocal" + s + " { }\n", nil

	case annotations.Seed:
		return scaffoldSeed(p, line)

	case annotations.Concept:
		src := scaffoldDoc(p, "A support ticket raised by a customer.") + line + strings.TrimPrefix(scaffoldTicket, "/// A support ticket: the row this cell's cases read and write.\n")
		switch p.Name {
		case "composable":
			src += "\n/// The tickets a composed file is made from.\nquery ticket ticketsToCompose {\n  filter status == \"open\"\n  sort \"row.createdAt\", \"desc\"\n  paginate 20\n}\n"
		case "namespace":
			// A namespace that is not the directory's needs the directory's
			// pin to agree (#2614); the pin sits beside the case.
			return "", src, map[string]string{"namespace.pin": "support\n"}
		}
		return "", src, nil

	case annotations.ConceptBody:
		return "", "use identity.concepts.{ user }\n\n/// A support ticket, owned by the user who raised it.\nconcept ticket {\n  ownerUserId  string\n  title        string\n\n  " + ann + "\n}\n", nil

	case annotations.ConceptField:
		doc := "A support ticket."
		if p.Name == "variant" {
			doc = "A support ticket, and where to reply: an email address or a phone number, whichever replyVia names."
		}
		fields := append([][2]string{{"title", "string"}, {"status", "string"}}, scaffoldConceptField(p.Name, ann)...)
		return "", "/// " + doc + "\nconcept ticket {\n" + scaffoldColumns("  ", fields) + "}\n", nil

	case annotations.ArgsField:
		arg, filter := [2]string{"status", "string  " + ann}, "status == args.status"
		switch p.Name {
		case "maxLength":
			arg, filter = [2]string{"title", "string  " + ann}, "title == args.title"
		case "maximum":
			arg, filter = [2]string{"priority", "int  " + ann}, "priority <= args.priority"
		case "minimum":
			arg, filter = [2]string{"priority", "int  " + ann}, "priority >= args.priority"
		case "pattern":
			arg, filter = [2]string{"region", "string  " + ann}, "region == args.region"
		}
		return scaffoldTicketWith([2]string{"priority", "int"}, [2]string{"region", "string"}),
			"/// The tickets that match, newest first.\nquery ticket matchingTickets" + s + " {\n" + scaffoldArgs(arg) + "  filter " + filter +
				"\n  sort \"row.createdAt\", \"desc\"\n  paginate 20\n}\n", nil

	case annotations.ToolField:
		// A query handler reads a tool field only through a $args.<field>
		// placeholder, and a function handler receives every field as an
		// argument, so each skeleton forwards every field its tool declares.
		// The list handler is the shipped form: an outer paginate() driven
		// by the caller, over a query that sorts and does not paginate itself
		// (dsl/memql/tools.memql, searchUsers).
		fixture := scaffoldTicket + "\n/// The tickets in one status, newest first; every status when none is named.\nquery ticket ticketsToList" + s + " {\n" +
			scaffoldArgs([2]string{"status", "string"}) + "  filter when(args.status) { status == args.status }\n  sort \"row.createdAt\", \"desc\"\n}\n"
		field := [2]string{"limit", "integer  " + ann + `  @description("The most tickets to return.")`}
		handler := "paginate(query ticketsToList" + s + "(), $args.limit)"
		switch p.Name {
		case "description":
			field = [2]string{"limit", "integer  " + ann}
		case "enum", "required":
			field = [2]string{"status", "string  " + ann + `  @description("Which tickets to list.")`}
			handler = "paginate(query ticketsToList" + s + "(status: $args.status), 20)"
		case "autoInjected":
			// The runtime stamps agentId (with ownerUserId and partitionId)
			// over whatever the model sent; the function handler receives it
			// as an argument like any other field.
			fixture = scaffoldTicketWith([2]string{"openedByAgentId", "string"}) + "\n/// Open a ticket, recording which agent opened it.\nmutate ticket openTicket" + s + " {\n" +
				scaffoldArgs([2]string{"title", "string!"}, [2]string{"agentId", "string"}) +
				"  insert {\n    title: args.title\n    status: \"open\"\n    openedByAgentId: args.agentId\n  }\n}\n"
			return fixture, "/// Open a support ticket; the runtime records which agent opened it.\n@handler(type=\"function\", name=\"openTicket" + s + "\")\ntool openSupportTicket" + s + " {\n" +
				scaffoldColumns("  ", [][2]string{
					{"title", `string!  @description("The ticket's one-line title.")`},
					{"agentId", "string   " + ann + `  @description("The agent opening the ticket; stamped by the runtime, never by the model.")`},
				}) + "}\n", nil
		}
		return fixture, "/// List the tickets, newest first.\n@handler(type=\"query\", query=\"" + handler + "\")\ntool listTickets" + s +
			" {\n" + scaffoldColumns("  ", [][2]string{field}) + "}\n", nil

	case annotations.PromptField:
		field := [2]string{"language", "string  " + ann + `  @description("The language to answer in.")`}
		switch p.Name {
		case "description":
			field = [2]string{"language", "string  " + ann}
		case "enum":
			field = [2]string{"length", "string  " + ann + `  @description("How long the digest is.")`}
		}
		return "", "/// A one-sentence digest of a ticket, for a reader who asked for one.\n@level(\"fast\")\n@templateFile(\"ticketDigest.tmpl\")\nprompt ticketDigest" + s +
				" {\n" + scaffoldColumns("  ", [][2]string{{"title", `string!  @description("The ticket's title.")`}, field}) + "}\n",
			map[string]string{"ticketDigest.tmpl": "Write a one-sentence digest of the support ticket titled \"{{.title}}\".\n"}

	case annotations.BuiltinField:
		return "", "/// Today's date key in a timezone, for naming a daily ticket.\n@executor(\"integration.timeutil.dateKeyInTimezone\")\nbuiltin dailyTicketKey" + s +
			" {\n" + scaffoldDateKeyFields(ann) + "}\n", nil
	}
	return "", "", nil
}

// scaffoldDateKeyFields is the date-key builtin's body; ann, when set, goes on
// the timezone field (a builtin field cell's annotation under test).
func scaffoldDateKeyFields(ann string) string {
	tz := `string  @description("An IANA timezone; UTC when empty.")`
	switch {
	case strings.HasPrefix(ann, "@description"):
		tz = "string  " + ann
	case strings.HasPrefix(ann, "@required"):
		tz = "string  " + ann + `  @description("An IANA timezone; the caller must name one.")`
	case ann != "":
		tz = "string  " + ann + `  @description("An IANA timezone; UTC when empty.")`
	}
	return scaffoldColumns("  ", [][2]string{{"timezone", tz}, {"now", `string  @description("The instant to key, RFC3339.")`}})
}

// scaffoldMutation is the mutation skeleton: a retitle by default, and for the
// write-shaping annotations the write each one shapes.
func scaffoldMutation(p annotations.Placement, s, line string) (string, string, map[string]string) {
	upd := func(doc, name string, args [][2]string, setLines string, extra ...[2]string) (string, string, map[string]string) {
		all := append([][2]string{{"ticketId", "string!"}}, args...)
		return scaffoldTicketWith(extra...), scaffoldDoc(p, doc) + line + "mutate ticket " + name + s + " {\n" + scaffoldArgs(all...) +
			"  update {\n    id: args.ticketId\n" + setLines + "  }\n}\n", nil
	}
	switch p.Name {
	case "actor":
		return upd("", "takeTicket", nil, "    ownerUserId: actor.userId\n")
	case "addToSet":
		return upd("", "labelTicket", [][2]string{{"labels", "[]string!"}}, "    labels: args.labels\n", [2]string{"labels", "[]string"})
	case "removeFromSet":
		return upd("", "unlabelTicket", [][2]string{{"labels", "[]string!"}}, "    labels: args.labels\n", [2]string{"labels", "[]string"})
	case "appendFields":
		return upd("", "attachToTicket", [][2]string{{"attachmentIds", "[]string!"}}, "    attachmentIds: args.attachmentIds\n", [2]string{"attachmentIds", "[]string"})
	case "mergeFields":
		return upd("", "setTicketPreferences", [][2]string{{"preferences", "object!"}}, "    preferences: args.preferences\n", [2]string{"preferences", "object"})
	case "noUnset":
		return upd("", "closeTicket", nil, "    status: \"closed\"\n    closedAt: now\n", [2]string{"closedAt", "datetime"})
	case "scrubPii":
		return upd("", "forgetTicketReporter", nil, "    status: \"redacted\"\n", [2]string{"reporterEmail", "string  @pii"})
	case "createOnly":
		return scaffoldTicketWith([2]string{"attempts", "int"}), scaffoldDoc(p, "") + line +
			"mutate ticket openTicket" + s + " {\n" + scaffoldArgs([2]string{"ticketId", "string!"}, [2]string{"title", "string!"}) +
			"  insert {\n    id: args.ticketId\n    title: args.title\n    status: \"open\"\n    attempts: 0\n  }\n}\n", nil
	}
	return upd("Retitle one ticket.", "retitleTicket", [][2]string{{"title", "string!"}}, "    title: args.title\n")
}

// scaffoldAutomation is the automation skeleton: when a node joins the
// cluster, raise a ticket through the fixture's mutation.
func scaffoldAutomation(p annotations.Placement, s, ann string) (string, string, map[string]string) {
	fixture := scaffoldTicket + "\n/// Raise a ticket with a title.\nmutate ticket raiseTicket" + s + " {\n" + scaffoldArgs([2]string{"title", "string!"}) +
		"  insert {\n    title: args.title\n    status: \"open\"\n  }\n}\n"
	trigger := `@trigger(event="node.created", concept="v1:cluster:node", partition="*")` + "\n"
	name := "welcomeNode" + s
	step := "  step raise {\n    mutation raiseTicket" + s + " (title: \"A node joined the cluster\")\n  }\n"
	args := ""
	switch p.Name {
	case "trigger":
		trigger = ""
	case "schedule":
		trigger, name = "", "hourlyReview"+s
		step = "  step raise {\n    mutation raiseTicket" + s + " (title: \"Review the queue\")\n  }\n"
	case "template":
		trigger, name = "", "raiseRequestedTicket"+s
		args = "  args {\n    /// The ticket's title.\n    title  string!\n  }\n\n"
		step = "  step raise {\n    mutation raiseTicket" + s + " (title: args.title)\n  }\n"
	case "actor":
		fixture = scaffoldTicket + "\n/// Raise a ticket with a title and an owner.\nmutate ticket raiseTicket" + s + " {\n" +
			scaffoldArgs([2]string{"title", "string!"}, [2]string{"ownerUserId", "string!"}) +
			"  insert {\n    title: args.title\n    status: \"open\"\n    ownerUserId: args.ownerUserId\n  }\n}\n"
		step = "  step raise {\n    mutation raiseTicket" + s + " (title: \"A node joined the cluster\", ownerUserId: actor.userId)\n  }\n"
	}
	return fixture, scaffoldDoc(p, "When a node joins the cluster, open a ticket to welcome it.") + trigger + ann + "\nautomation " + name + " {\n" + args + step + "}\n", nil
}

// scaffoldSeed is the seed skeleton. A seed materializes through its
// concept's create<Concept> mutation, found by bare name, so each seed cell
// declares a concept of its own and that mutation beside it: a cell an author
// copies seeds a row, not just a registry entry.
func scaffoldSeed(p annotations.Placement, line string) (string, string, map[string]string) {
	c := scaffoldSeedCells[p.Name]
	idArg := c.concept + "Id"
	conceptFields := [][2]string{}
	for _, f := range c.fields {
		conceptFields = append(conceptFields, [2]string{f[0], strings.TrimSuffix(f[1], "!")})
	}
	createArgs := append([][2]string{{idArg, "string!"}}, c.fields...)
	var sets strings.Builder
	sets.WriteString("    id: args." + idArg + "\n")
	for _, f := range c.fields {
		sets.WriteString(fmt.Sprintf("    %s: args.%s\n", f[0], f[0]))
	}
	fixture := "/// The row this cell's seed writes.\nconcept " + c.concept + " {\n" + scaffoldColumns("  ", conceptFields) + "}\n\n" +
		"/// The mutation the seed materializer writes this concept's rows through: create<Concept>.\n" +
		"mutate " + c.concept + " create" + upperFirst(c.concept) + " {\n" + scaffoldArgs(createArgs...) + "  insert {\n" + sets.String() + "  }\n}\n"
	var body strings.Builder
	for _, b := range c.body {
		body.WriteString("  " + b[0] + ": " + b[1] + "\n")
	}
	var side map[string]string
	if p.Name == "templateFile" {
		side = map[string]string{"triageAssistant.tmpl": "You triage support tickets: read each one, name its category, and say how urgent it is.\n"}
	}
	return fixture, scaffoldDoc(p, "") + line + "seed " + c.concept + " " + c.seed + " {\n" + body.String() + "}\n", side
}

// ---- the per-name tables -----------------------------------------------------
//
// Three receivers cannot be written from a generic skeleton, because each cell
// needs something no other cell may share: a capability verb the embedded
// catalog does not declare yet (the catalog refuses a verb declared twice), a
// rule precedence no other unlocked rule holds (a tie is a load error), a seed
// concept of its own (a seed writes through its concept's create<Concept>,
// found by bare name). A placement these tables do not name is an error in
// scaffoldCell, naming the table to extend, rather than a skeleton with an
// empty verb, precedence or concept in it.

// scaffoldCapabilityVerbs is the capability verb, class and argument each
// capability cell declares.
var scaffoldCapabilityVerbs = map[string][3]string{
	"description": {"fs.list", "read", "path"},
	"enabled":     {"fs.exists", "read", "path"},
	"disabled":    {"fs.glob", "read", "pattern"},
	"sideEffect":  {"fs.mkdir", "write", "path"},
}

// scaffoldRulePrecedence is the precedence each rule cell's rule holds.
// @precedence's own cell writes the registry's example (60) instead.
var scaffoldRulePrecedence = map[string]string{
	"description": "11", "enabled": "12", "disabled": "13", "exclude": "14", "level": "15",
	"locked": "16", "onUnavailable": "17", "policy": "18", "precedence": "60", "when": "20",
}

// scaffoldSeedCell is one seed cell's concept and seed.
type scaffoldSeedCell struct {
	concept string
	fields  [][2]string // concept fields beyond the id
	seed    string
	body    [][2]string // seed body, field -> literal
}

// scaffoldSeedCells is the concept and seed each seed cell declares.
var scaffoldSeedCells = map[string]scaffoldSeedCell{
	"description":  {"ticketCategory", [][2]string{{"name", "string!"}}, "billingCategory", [][2]string{{"name", `"Billing"`}}},
	"enabled":      {"cannedReply", [][2]string{{"text", "string!"}}, "thanksReply", [][2]string{{"text", `"Thanks for reaching out -- we are on it."`}}},
	"disabled":     {"queueBanner", [][2]string{{"message", "string!"}}, "maintenanceBanner", [][2]string{{"message", `"The queue is paused for maintenance."`}}},
	"namespace":    {"helpArticle", [][2]string{{"title", "string!"}}, "gettingStarted", [][2]string{{"title", `"Getting started with the support queue"`}}},
	"scope":        {"personalQueue", [][2]string{{"ownerUserId", "string!"}, {"name", "string!"}}, "myTickets", [][2]string{{"name", `"My tickets"`}}},
	"templateFile": {"supportAssistant", [][2]string{{"name", "string!"}, {"systemPrompt", "string"}}, "triageAssistant", [][2]string{{"name", `"Triage"`}}},
	"version":      {"escalationRule", [][2]string{{"name", "string!"}, {"afterHours", "int"}}, "urgentEscalation", [][2]string{{"name", `"Urgent"`}, {"afterHours", "4"}}},
}

// scaffoldUnmapped is the error for a placement one of the per-name tables
// must name and does not, "" when the placement needs no table or has an
// entry.
func scaffoldUnmapped(p annotations.Placement) string {
	var table string
	var ok bool
	switch p.Receiver {
	case annotations.Capability:
		_, ok = scaffoldCapabilityVerbs[p.Name]
		table = "scaffoldCapabilityVerbs (a verb the Go vocabulary classifies and the embedded catalog does not declare)"
	case annotations.Rule:
		_, ok = scaffoldRulePrecedence[p.Name]
		table = "scaffoldRulePrecedence (a precedence no other cell's rule holds)"
	case annotations.Seed:
		_, ok = scaffoldSeedCells[p.Name]
		table = "scaffoldSeedCells (a concept of the cell's own, with its create<Concept>)"
	default:
		return ""
	}
	if ok {
		return ""
	}
	return fmt.Sprintf("no skeleton for @%s on %s: add an entry for %q to %s", p.Name, p.Receiver.Phrase(), p.Name, table)
}

// scaffoldConceptField is the field (or fields) the annotation means
// something on, as name/rest-of-line pairs.
func scaffoldConceptField(name, ann string) [][2]string {
	switch name {
	case "default":
		return [][2]string{{"priority", "string  " + ann}}
	case "immutable", "unique":
		return [][2]string{{"ticketNumber", "string  " + ann}}
	case "internal":
		return [][2]string{{"triageScore", "int  " + ann}}
	case "maximum", "minimum":
		return [][2]string{{"progress", "int  " + ann}}
	case "open":
		return [][2]string{{"details", "object  " + ann + " {\n    source  string\n  }"}}
	case "pattern":
		return [][2]string{{"slug", "string  " + ann}}
	case "pii":
		return [][2]string{{"reporterEmail", "string  " + ann}}
	case "secret":
		return [][2]string{{"webhookToken", "string  " + ann}}
	case "serverSet":
		return [][2]string{{"closedAt", "datetime  " + ann}}
	case "variant":
		// The discriminator is a SIBLING of the variant field, on the object
		// that owns it: the engine ties each branch to the discriminator's
		// value with an if/then on the owner (memql#3623), and a discriminator
		// declared inside the branches names no sibling, so those ifs never
		// fire. An enum, required, so a value naming no branch is refused too
		// (dsl/_reference/_concept.memql, section 9).
		return [][2]string{
			{"replyVia", `enum("email", "phone")!`},
			{"replyTo", "object!  " + ann + " {\n    email {\n      address  string!\n    }\n    phone {\n      number  string!\n    }\n  }"},
		}
	}
	return [][2]string{{"summary", "string  " + ann}}
}
