// Package compose is the Go half of the Materializer (design record
// docs/superpowers/specs/2026-09-05-compose-materializer-design.md, epic
// memql#4977). It backs the five builtins declared in
// dsl/compose/builtins.memql:
//
//	integration.compose.materialize         -- compose, render, stamp, file
//	integration.compose.runRecipe           -- resolve a recipe's selectors and materialize
//	integration.compose.cancel              -- ask a composition to stop
//	integration.compose.composableConcepts  -- what is worth composing from
//	integration.compose.resolveSources      -- what a source list finds, without composing
//
// THE DIVISION OF LABOUR IS THE DESIGN, and it is the work spine's.
// Every decision about BYTES is a pure function in component/compose --
// which format carries provenance, what a docx package holds, whether a
// package source is byte-identical across runs -- so the epic's headline
// claims are properties of values, provable with no engine, no provider
// and no blob storage. This package is responsible for obeying those
// decisions and for the three things a pure package cannot do: reaching
// the engine, getting the actor right (see store.go's header, which is
// the file to read before changing anything here), and putting bytes in
// the Library.
package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	pure "github.com/znasllc-io/memql/component/compose"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// integrationName is the plug-in name and the middle segment of every
// capability FQN. Spelled as a STRING LITERAL in RegisterPlugin below as
// well, because the module-taxonomy gate finds registrations by scanning
// source for the literal.
const integrationName = "compose"

// resultConcept is the synthetic concept a capability reply rides on. It
// is never persisted -- the reply is a value the caller reads, the same
// shape every other integration answers with.
const resultConcept = "v1:compose:result"

// Uploader is object storage. *azureblob.AzureBlobUploader implements it.
//
// NIL IS AN ANSWER RATHER THAN A CRASH, and it is the same answer the
// Library's own upload route gives: with no storage configured the
// composition still gets a row, and the row is marked `failed` with a
// reason naming the two env vars. A person who materialized something
// has to be able to SEE that the bytes did not land -- a silent success
// with a blobUrl nothing answers is the failure this avoids.
type Uploader interface {
	Upload(ctx context.Context, container, objectName string, data []byte, contentType string) (string, error)
}

// ConceptSource is where the composable marks come from. The engine's
// concept registry satisfies it; a test supplies a fixture.
type ConceptSource interface {
	ConceptDefinitions() []*memorynodes.Concept
}

// THERE IS NO GoalOpener SEAM, and its absence is a decision. A
// materialization IS a goal (design D6), and the obvious shape is a Go
// interface onto integrations/work -- but `createGoal` exists there as a
// capability HANDLER rather than as an exported method, so taking that
// shape would mean adding one, coupling two integrations in Go for
// something the DSL already exposes, and giving `requestedVia` a second
// spelling.
//
// So `openGoal` in materialize.go calls the `createGoal` BUILTIN over
// this package's own engine handle, unstamped, under the caller's actor
// -- the same path a DSL author would take. A failure there is logged
// and never fatal: the file is the deliverable and the tracking is
// around it, so the composition carries an empty goalId, which the app
// renders as "not tracked" rather than as a broken link.

// Composer produces the draft. It is the ONE step that reaches a model,
// and the only one.
//
// NIL IS AN ANSWER HERE TOO, and a useful one: with no composer wired, a
// materialization whose caller supplied a draft still works end to end,
// which is what lets the whole render/stamp/file tail be exercised on a
// node with no provider configured. A materialization with NEITHER a
// composer nor a draft is refused, naming which.
type Composer interface {
	Compose(ctx context.Context, req ComposeRequest) (ComposeReply, error)
}

// ComposeRequest is what the reasoning step is given.
type ComposeRequest struct {
	Statement string
	Format    pure.Format
	// Sources are the resolved rows, already narrowed to each concept's
	// @composable(fields=...) projection where one was declared.
	Sources []Resolved
	// TemplateName and TemplateBody are the template's own content,
	// when one was chosen.
	TemplateName string
	TemplateBody string
	// Draft is what the person had already written, when they had.
	Draft string
}

// ComposeReply is what the reasoning step produced.
type ComposeReply struct {
	Draft pure.Draft
	// Models is every model that contributed to THIS call. A list for
	// the reason the concept field is one: a composer that made two
	// calls to two providers reports two entries.
	Models []pure.ModelContribution
}

// Integration exposes the compose capabilities.
type Integration struct {
	engine   Engine
	logger   *slog.Logger
	uploader Uploader
	bucket   string
	instance string

	concepts ConceptSource
	composer Composer

	now func() time.Time
	mu  sync.RWMutex
}

func New(engine Engine, logger *slog.Logger) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	return &Integration{engine: engine, logger: logger, now: time.Now}
}

func init() {
	memql.RegisterPlugin("compose", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return New(pctx.Engine, pctx.Logger), nil
	})
}

// SetUploader wires object storage and its container.
func (i *Integration) SetUploader(u Uploader, bucket string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.uploader, i.bucket = u, strings.TrimSpace(bucket)
}

// SetInstance records the cluster's domain, which is the "which MemQL
// made this" fact a provenance record carries and the question somebody
// holding the file six months later has.
func (i *Integration) SetInstance(domain string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.instance = strings.TrimSpace(domain)
}

// SetConceptSource wires the concept registry the composable marks are
// read from.
func (i *Integration) SetConceptSource(c ConceptSource) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.concepts = c
}

// SetComposer wires the reasoning step.
func (i *Integration) SetComposer(c Composer) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.composer = c
}

// SetNow injects a clock. Tests only.
func (i *Integration) SetNow(f func() time.Time) {
	if f != nil {
		i.now = f
	}
}

func (i *Integration) clock() time.Time {
	if i == nil || i.now == nil {
		return time.Now()
	}
	return i.now()
}

func (i *Integration) log() *slog.Logger {
	if i == nil || i.logger == nil {
		return slog.Default()
	}
	return i.logger
}

func (i *Integration) store() *store { return &store{engine: i.engine} }

func (i *Integration) uploaderRef() (Uploader, string) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.uploader, i.bucket
}

func (i *Integration) instanceRef() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.instance
}

func (i *Integration) conceptsRef() ConceptSource {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.concepts
}


func (i *Integration) composerRef() Composer {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.composer
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return integrationName }

// Capabilities implements memql.IntegrationProvider. Each Name is the
// last segment of the @executor FQN its builtin declares; the registry
// namespaces them as integration.compose.<name>.
// TestCapabilityNamesMatchTheDSL asserts the set against
// dsl/compose/builtins.memql, because a capability the DSL names and the
// registry lacks is a BOOT failure on every node type.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "materialize",
			Description: "Compose the named sources into a file of the chosen format and file it in the Library. Opens a v1:compose:composition, a v1:work:goal with requestedVia=\"materializer\" and that goal's first run, then runs gather / compose / render / stamp / file. Returns {compositionId, goalId, runId, outputFileId, format, provenanceEmbedded}.",
			Handler:     i.handleMaterialize,
			ArgsSchema: map[string]string{
				"name":           "string (required) -- what to call it, and the filename stem",
				"statement":      "string -- what you asked for, in your own words",
				"format":         "string (required) -- markdown | html | txt | csv | json | docx | pdf",
				"sources":        "[]object -- {kind, ref, label}; kind is concept_row | library_file | query",
				"draft":          "string -- a draft to start from; empty means the compose step writes it",
				"templateId":     "string -- v1:compose:template to render through",
				"folderId":       "string -- v1:library:folder to file the output into",
				"accountIds":     "[]string -- account tags; a record, never a visibility scope",
				"deployableKind": "string -- spa | static | shopify_storefront to produce a package source zip instead of a document",
				"recipeId":       "string -- the recipe this run came from, when it came from one",
				"ceilings":       "object -- the goal's ceilings; a zero is 'unset', never 'nothing allowed'",
			},
		},
		{
			Name:        "runRecipe",
			Description: "Run one of your recipes again: resolve its selectors NOW under your own actor, compose, render and file. Returns the same shape materialize does.",
			Handler:     i.handleRunRecipe,
			ArgsSchema: map[string]string{
				"recipeId": "string (required) -- the v1:compose:recipe to run",
				"name":     "string -- what to call this run's output; empty takes the recipe's name plus the date",
			},
		},
		{
			Name:        "cancel",
			Description: "Ask one of your compositions to stop. Cancellation is REQUESTED rather than done: the run notices at its next step boundary. Returns {compositionId, status}.",
			Handler:     i.handleCancel,
			ArgsSchema: map[string]string{
				"compositionId": "string (required) -- the v1:compose:composition to stop",
				"reason":        "string -- why, in words a person can read",
			},
		},
		{
			Name:        "composableConcepts",
			Description: "The concepts worth composing from in this cluster: everything declaring @composable, with the label, the field projection and the list query each declared. Returns {concepts: [{id, as, fields, list, description, marked}]}.",
			Handler:     i.handleComposableConcepts,
			ArgsSchema: map[string]string{
				"includeUnmarked": "boolean -- also return the concepts declaring no @composable, after the marked ones and flagged as unmarked",
			},
		},
		{
			Name:        "resolveSources",
			Description: "Resolve a source list without composing anything: how many rows each entry finds and what stopped it when it found none. Every read runs under your own actor. Returns {sources: [{kind, ref, label, count, problem}], total}.",
			Handler:     i.handleResolveSources,
			ArgsSchema: map[string]string{
				"sources": "[]object (required) -- the same {kind, ref, label} list materialize takes",
			},
		},
	}
}

// ---------------------------------------------------------------------------
// The composable marks
// ---------------------------------------------------------------------------

// composableConcept is one entry of the answer both consumers read --
// the Sources column and the compose prompt. ONE derivation, so the
// person and the model cannot be looking at different sets.
type composableConcept struct {
	Id          string   `json:"id"`
	As          string   `json:"as"`
	Fields      []string `json:"fields,omitempty"`
	List        string   `json:"list,omitempty"`
	Description string   `json:"description,omitempty"`
	// Marked is false for a concept returned only because the caller
	// asked for the unmarked ones too. It is a FIELD rather than two
	// lists so a client rendering them together keeps the distinction.
	Marked bool `json:"marked"`
}

// composables reads the marks out of the concept registry.
func (i *Integration) composables(includeUnmarked bool) []composableConcept {
	src := i.conceptsRef()
	if src == nil {
		return nil
	}
	var marked, unmarked []composableConcept
	for _, c := range src.ConceptDefinitions() {
		if c == nil || strings.TrimSpace(c.Name) == "" {
			continue
		}
		if c.Composable == nil {
			if includeUnmarked {
				unmarked = append(unmarked, composableConcept{
					Id: c.Name, As: entityOf(c.Name), Description: c.Description, Marked: false,
				})
			}
			continue
		}
		as := strings.TrimSpace(c.Composable.As)
		if as == "" {
			as = entityOf(c.Name)
		}
		marked = append(marked, composableConcept{
			Id:          c.Name,
			As:          as,
			Fields:      append([]string(nil), c.Composable.Fields...),
			List:        c.Composable.List,
			Description: c.Description,
			Marked:      true,
		})
	}
	sortByAs(marked)
	sortByAs(unmarked)
	return append(marked, unmarked...)
}

// listQueryFor answers the `list` query a concept declared, or "".
func (i *Integration) listQueryFor(conceptId string) string {
	src := i.conceptsRef()
	if src == nil {
		return ""
	}
	want := strings.TrimSpace(conceptId)
	for _, c := range src.ConceptDefinitions() {
		if c == nil || c.Name != want || c.Composable == nil {
			continue
		}
		return strings.TrimSpace(c.Composable.List)
	}
	return ""
}

// fieldsFor answers the projection a concept declared, or nil.
func (i *Integration) fieldsFor(conceptId string) []string {
	src := i.conceptsRef()
	if src == nil {
		return nil
	}
	want := strings.TrimSpace(conceptId)
	for _, c := range src.ConceptDefinitions() {
		if c == nil || c.Name != want || c.Composable == nil {
			continue
		}
		return c.Composable.Fields
	}
	return nil
}

func entityOf(conceptId string) string {
	if i := strings.LastIndex(conceptId, ":"); i >= 0 {
		return conceptId[i+1:]
	}
	return conceptId
}

func sortByAs(cs []composableConcept) {
	for i := 1; i < len(cs); i++ {
		for j := i; j > 0 && cs[j].As < cs[j-1].As; j-- {
			cs[j], cs[j-1] = cs[j-1], cs[j]
		}
	}
}

// ---------------------------------------------------------------------------
// Replies
// ---------------------------------------------------------------------------

// resultNode wraps a capability's answer as the single node the engine
// hands back to the caller.
func (i *Integration) resultNode(payload map[string]any) []memorynodes.MemoryNode {
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte("{}")
	}
	at := i.clock().UTC()
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("compose:%d", at.UnixNano()),
		Concept:   resultConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: at,
		Payload:   raw,
	}}
}
