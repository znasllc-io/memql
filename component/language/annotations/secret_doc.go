package annotations

// docSecretField is the doc of @secret on a concept field, and the one
// placement doc that runs to paragraphs. It is the scope statement memql#3036
// and #3182-#3184 settled for @secret, and it lives here because the generated
// attribute matrix (docs/public/language/attribute-matrix.md, memql#5360)
// prints it: component/database/memory-nodes' TestSecretEnforcementIsRealAndScoped
// reads that page and fails when a surface goes unnamed, or when a surface
// that now redacts is still described as uncovered. Hover and completion show
// the one-line Docs["secret"] instead.
//
// Extend the covered list by re-running the enumeration it describes, never
// by appending to it, then run `make docs-matrix`.
const docSecretField = "The field holds a secret. It is emitted as `x-secret`, and every validation surface that quotes a rejected value " +
	"replaces the value with `<redacted>` while the argument name and the declared constraint (enum members, bounds, pattern) " +
	"survive, so the diagnostic stays usable.\n" +
	"\n" +
	"The covered surfaces come from an exhaustive enumeration of validators -- every jsonschema `.Validate(` call site and every " +
	"value-quoting rejection message in the engine -- not from adding one entry at a time. Three incremental passes each shipped a " +
	"scope statement that walked past a surface the next pass found, so extend the list by re-running the enumeration, never by " +
	"appending to it.\n" +
	"\n" +
	"- The **function-args validator** (`component/memql/function_validator.go`): enum, minimum, maximum, pattern and date-time " +
	"(memql#3036).\n" +
	"- The **tool-args validator** (`MemQLEngine.validateToolArgs`, `component/memql/tool_execution.go`), compiled from the same " +
	"args schema and running before the function-args validator on the agent path, so it is the surface a rejected secret reaches " +
	"first. Both of its exits redact (memql#3182): the message returned to the model, and the WARN, which redacts per key from the " +
	"args schema instead of serializing the whole args map, and falls back to a values-free `argKeys` list when the tool's " +
	"arguments cannot be classified.\n" +
	"- The **automation args binder** (`component/automations/args_binding.go`), which applies the same rules to event payloads: a " +
	"`graph.node.created` event carries the concept row's fields flattened into its payload. The loader stamps the secret flag " +
	"from the trigger topic's concept, so the enum and pattern refusals, and the WARN log they are written to, all print " +
	"`<redacted>`, closing the one path by which a row value could reach a structured log (memql#3183).\n" +
	"- **Concept payload validation** (`Concept.validate`, so `Create` and `Delete`), where `@minimum`, `@maximum` and `@format` " +
	"declared on the concept are enforced. The six jsonschema keywords that interpolate the instance value (`minimum`, `maximum`, " +
	"`exclusiveMinimum`, `exclusiveMaximum`, `multipleOf`, `format`) all redact, and every other message stays byte-identical to " +
	"an unannotated field's (memql#3184).\n" +
	"- The DSL-callable `validate` and `preflight` builtins (`component/memql/executor_builtin.go`), which compile the same concept " +
	"schema and return every leaf message to the caller in a result payload.\n" +
	"\n" +
	"The last two resolve secrecy through the schema at the failing instance location, so they cover `@secret` at any nesting " +
	"depth and inherit it onto array elements, unlike `Concept.SecretFields()`, which is top-level only and is not the accessor " +
	"those paths use.\n" +
	"\n" +
	"On the two args-validator surfaces, **matching is by argument NAME, not by write target**: an args field is redacted when its " +
	"name appears among the bound concept's `@secret` fields. A mutation writing `apiKey: args.credential` into a `@secret` " +
	"`apiKey` leaves `credential` unredacted, and renaming between argument and field is common, so do not rely on the write " +
	"target. The concept-payload and builtin surfaces resolve through the schema and are not subject to this rule.\n" +
	"\n" +
	"A `@secret` value is **not** redacted from **query results**: any query that projects it returns it in full. That is an " +
	"authorization decision, which needs a definition of \"elevated\" and depends on the per-row authorization model (memql#2803).\n" +
	"\n" +
	"**Length is never redacted anywhere**, deliberately and uniformly: `value too long (N runes, max M)` and the jsonschema " +
	"`minLength` and `maxLength` messages report a rune count for a secret field too. They quote no value, but a length is a " +
	"disclosure; redacting it on one surface would make that surface the only one that differs while withholding nothing the " +
	"others already print.\n" +
	"\n" +
	"Prompt input-schema validation (`PromptTemplate.ValidateData`, `component/memql/ai_prompts.go`) is unclassified: it runs a " +
	"jsonschema over caller data and interpolates the instance the same way, but its schema is built from the prompt's own field " +
	"list rather than from a concept, so there is no `x-secret` in it to read. Nothing is redacted there; treat it as uncovered, " +
	"not as safe.\n" +
	"\n" +
	"So `@secret` stops a credential leaking through a validation diagnostic. It is not a general secrecy guarantee, and not a " +
	"reason to treat a credential in the graph as protected."
