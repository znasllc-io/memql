# Nexus and Materializer execution

The reported Nexus goal reached a live BFF at 2026-09-09T14:54:07Z. Its local
work integration had no compiler, so nothing claimed the compiling run. The
abandonment sweep subsequently described a node loss. Materializer independently
had no composer wired and opened an unrelated compiling goal while executing its
file pipeline outside that goal's journal.

## Execution ownership

Persisted compiling runs are work requests for the planner replicas. The graph
event signals availability; a shared, fail-closed claim arbitrates ownership.
The winner reads the current run and goal under the recorded owner, stamps the
run and budget context, and maintains its heartbeat while compiling. The sweep
recovers missed delivery for unclaimed runs. An unclaimed request must not be
described as a lost executing node. Compiled runs continue through the existing
agent dispatcher.

Generated plans are validated, persisted as private authoring bundles and bound
to their originating run. The executing agent reconstructs the complete source
closure under the owner, verifies its version and resolves queries, logic and
shapes through a run-scoped registry. Nothing is globally activated by compile.
Trivial content goals execute one synchronous agent turn; sectionable goals
return their actual parallel results for assembly. The seeded planner agent is
available on an engine-only installation. Work turns use an engine-shipped
prompt and a synchronous composeFile tool, and cannot redelegate the same work
through produceArtifact.
The existing complexity triage explicitly identifies goals requiring a saved
file and supplies its name and supported format. Deterministic file plans call
Materializer directly with the goal and runtime inputs; sectionable file plans
select their completed final prose with `sectionKeys`. The section planner
explicitly marks final content (`deliver: true`) and evidence-only notes
(`deliver: false`); the DSL selects and orders those keys. The integration
reads owner-scoped completed text receipts from that run, snapshots their
exact text and recorded model/source provenance, and renders without another
whole-document model call. Retries on another replica reuse the immutable
snapshot. Missing, unfinished, non-text, duplicate or foreign sections fail
before effects. CSV/JSON and generation from a new request retain composition.
Malformed Materializer model output is a plan failure, not a transport EOF;
recovery can replace just assembly while preserving completed chapters.
Replanning projects receipts into a DSL-budgeted evidence view: small semantic
values are retained, large values become explicitly marked head/tail previews
with character counts and hashes. Duplicate result envelopes and fan-in bodies
cannot consume the whole planning context. Every completed key/call and the
exact sealed source remain available; execution and installation still use the
full journal. Preview omission is never interpreted as missing work or permission
to invent content. Creating a known file
does not need an agent turn to rediscover the file capability through tool
calling. A successful composition requires its Library file to be marked ready.
Known agent-turn file templates verify an owned, ready Library file produced
by that run and step. Journal receipts persist the actual returned value,
including flat builtin output, so replay and assembly cannot lose it behind an
executor wrapper.
Executor argument concatenation decodes string literals through the DSL lexer,
so quotes, line breaks and backslashes in the goal survive the persisted plan.

Materializer knows its automation before it starts. A registered materializeFile
template executes the existing file pipeline in the work journal. Acceptance
returns the composition and work identifiers promptly; the live composition
records completion or failure. A Materializer capability invoked from a Nexus
run uses that run rather than opening a competing planning goal. Completed file
effects must not be duplicated when the journal serves a replay.

## Inference

The composer makes one structured call through the engine's model seam and the
existing fleet-first router, with no concrete provider or model pinned. Actual
provider, model and usage must reach provenance, including fleet usage and
journal-served results. Tabular output supplies cells for CSV and JSON; document
output supplies a title and body. Every executing node gets the same required
composer and object-storage dependencies.
HTML files render Markdown and composed HTML fragments as document structure.
The rendered body passes through an explicit element allowlist that removes
active content, attributes and external resources; title and provenance remain
escaped by the page template.

Planner inference forwards to the agent replica holding the worker stream. The
owner and shared fleet status queries bypass result caching: a cached heartbeat
must not make a connected machine appear offline. Private composition inputs
remain owner-only even when composition metadata is shared through an account.
Agent streaming, background and escalation selection carries the restored
owner, run and deadline into the router so an executing replica can discover
the owner's private fleet before dispatch.
The streaming agent consumes content and assembled tool calls on a terminal
provider chunk before ending that turn; fleet providers deliver tool calls on
that closing chunk.
Interrupted streams return their upstream error with any partial answer, so
a truncated response cannot become a successful Nexus result.

The Materializer accepts source selections, a content request or a starting
draft. Its file-opening intent carries a file ID; Files resolves that against
the retained artifact feed, including an artifact arriving after the intent.
Downloads await activation of the narrowly scoped download worker itself;
the OS page is outside that worker's scope and cannot await page control.
The worker retains a completed download until navigation claims its stream,
including when a small file finishes transferring before the iframe loads.
The composition feed includes the full provenance and source projection so a
reopened composition shows the model and inputs recorded by execution.
For an unfinished composition, Materializer retains its exact owner-scoped work
run and shows failure or cancellation when that run stops. The join verifies the
owner, goal and run; it never changes a ready file or writes a competing terminal
state. Resuming the run reveals the composition's progress again.
HTTP credential reads respect the identity service's relative token lifetime
and share one pending refresh with SDK rotation. A failed rotation during a
rollout cannot leave subsequent downloads using an expired cached credential.
All OS refresh-cookie requests, including the session probe, also share an
origin-wide Web Lock until their response headers have applied the new cookie.
This prevents separate OS tabs from rotating the same predecessor concurrently
without sharing credentials through storage or broadcast messages. Sign-in
requires that browser capability; the lock and fetch share a bounded timeout.
An agent's parent connector creates a fresh transport after peer removal and
detaches the previous BFF identity when the service reconnects to a different
replica. A stale peer's removal cannot close its replacement stream. This keeps
composition updates reaching the browser after a BFF rollout.

## Acceptance

Regression tests exercise intake without a local compiler, competing planner
replicas, duplicate and stale events, missed delivery and long compilation.
Materializer tests exercise one tracked run, model attribution, terminal errors
and repeated execution without duplicate output. Database tests use an isolated
PostgreSQL/TimescaleDB instance. Live acceptance uses the local k3d installation
with two replicas: submit Markdown, HTML and text goals through Nexus;
materialize CSV, JSON, PDF and DOCX through Materializer; verify file contents and
downloadability, local-model routing, and accurate terminal state in the OS.
