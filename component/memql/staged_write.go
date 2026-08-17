package memql

// staged_write.go -- the WRITE chokepoint's half of the STAGED DATA tier
// (epic memql#3974, task memql#3985).
//
// # The title says "stamp and gate". It is neither, and the difference is the task
//
// THERE IS NOTHING TO STAMP. memql#3977 ruled the marking model CONCEPT-GRAIN:
// no marker on the written row at all. That is a measured ruling, not a
// simplification -- `compress_segmentby='concept'` means any per-row marker
// requires chunk decompression, which hard-errors outright under
// `timescaledb.enable_dml_decompression=false`, while the concept-grain flag is
// one row write at 11ms. So a write to a staged concept produces exactly the
// same row it would have produced otherwise, byte for byte, and the visibility
// answer is a pure function of the concept (authoring_concept_staged.go).
//
// AND THE WRITE IS NOT REFUSED. This sits directly beside the memql#3756
// retirement refusal in executeWrite, one branch apart, and it is the opposite
// verdict: rows arriving BEFORE the concept is trained is the entire feature.
// A retired concept is closed to writes because its schema is going away; a
// staged concept is open to writes because staging is what a concept does while
// its data accumulates. The two are siblings in PLACEMENT -- both are properties
// of the concept rather than of anything the caller sent, so both belong at the
// one write chokepoint -- and in nothing else. Do not "align" them.
//
// # What the write path actually owes the tier: SILENCE
//
// One thing, and it is forced rather than chosen. A write to a staged concept
// must publish NO graph.node.created / graph.node.updated event, because those
// events ARE the row.
//
// executeWrite builds its event payload by flattening the entire stored payload
// into the envelope's top level AND retaining it nested underneath:
//
//	maps.Copy(eventPayload, payloadMap)   // every field, flattened
//	eventPayload["payload"] = payloadMap  // and the whole object again
//
// executeUpdate's graph.node.updated build is the same shape, deliberately
// ("mirrors the executeWrite publish shape so subscribers can use the same
// pattern across .created and .updated"), and additionally carries the PRIOR
// row's status as `oldStatus`.
//
// Nothing downstream narrows that. events.Bus.Publish matches on topic pattern
// and fans a CLONE out to every matching subscriber; there is no AccessContext,
// no actor, and no authorization hook anywhere in component/events. The only
// thing that ever edits a graph event payload is BareifyEventPayload on the gRPC
// wire, and that is the bare-id client contract -- an id-shape rewrite -- not a
// filter and not authorization.
//
// So suppressing the READ while emitting the event would not hide a staged row.
// It would hand the complete row -- every field, twice, plus the actor -- to
// every in-process subscriber and to every automation whose trigger matches the
// concept, and then call the row hidden. The read filter (memql#3983) and this
// suppression are not two independent features; either one alone is a
// visibility rule with a hole in it exactly the width of the event bus.
//
// # NO BACKLOG REPLAY when the concept is later trained
//
// Ruled on memql#3979, and it is the question this file is most likely to be
// "fixed" over later, so: the withheld events are GONE, not deferred. Replaying
// them at training time would fire every subscribed automation once per staged
// row -- ten thousand in a burst for rows that arrived over an arbitrary period,
// each carrying a `createdAt` scattered across the past while arriving now. An
// automation cannot tell that apart from ten thousand things happening at once,
// which is the one thing it is built to react to. Training emits ONE
// concept-grain event (memql#3986); that is the whole notification.
//
// # What this file deliberately does NOT do
//
// It does not touch a single read. Two reads on the write path must stay
// ungated, and both are commented at their definitions in executor_mutation.go
// because that is where someone will be standing when they consider gating them:
//
//   - loadPriorPayload -- the read-merge. Gating it makes a hidden prior row
//     report exists=false, which degrades an UPDATE into a CREATE and silently
//     drops every field the caller did not restate. Data loss, on a write the
//     user believes is a partial update. TestStagedWrite_PartialUpdate...
//     PreservesOmittedFields (staged_write_test.go) pins it.
//   - checkNodeExists -- previewInsert's content-addressed id collision probe.
//     Gating it makes it answer "free" about an id a staged row already occupies,
//     so the caller inserts and appends a version onto the staged row.
//
// The rule underneath both: this tier withholds rows from CALLERS asking what
// exists. It must never withhold a row from the ENGINE deciding what to write,
// because the engine is not the audience -- it is the mechanism the audience's
// data survives in.

// withholdGraphWriteEvent reports whether a successful write to conceptName must
// publish NO graph.node.* event.
//
// Named for the ACTION rather than for the state (`conceptDataIsStaged` is the
// state, and it is what this consults) so the two call sites read as the
// decision they are making. It is one sync.Map load behind a string
// concatenation, on the hot path of every write in the installation; keeping it
// that cheap is why the tier stores its marker where it does.
func (e *MemQLEngine) withholdGraphWriteEvent(conceptName string) bool {
	return e.conceptDataIsStaged(conceptName)
}
