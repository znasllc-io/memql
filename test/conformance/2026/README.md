# The MemQL conformance corpus, edition 2026

Every rule of the language, as a file the engine is held to. Each case is a
small `.memql` file and a verdict; the runner (`test/conformance/corpus_test.go`)
loads it through the same code a node boots with and fails when the engine
does not reach the verdict the case names.

The design is D23 of
[the DSL version-one record](../../../docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
The corpus is also the source of the documentation's examples and of the
examples a model is shown, so a case that loads is a form an author may copy.

## Layout

| Directory | Holds |
|---|---|
| `manifest.json` | The edition and language line every case here is written in, and the corpus status (`draft` until the freeze). |
| `cells/<receiver>/<annotation>/` | One directory per place an annotation can be written, from the annotation registry. Each holds at least one case that loads and one that is refused. |
| `expr/<position>/` | One directory per expression position, from `component/language/tiers`. Each holds at least one case that loads and one that is refused. |
| `negative/<construct>/` | One fault per file, the file named after the fault. |
| `scenarios/<domain>/` | Whole automations as the product ships them, loaded together. |
| `fuzz/` | Inputs that once broke the parser. |

## A case directory

A directory that holds cases has one `expect.json`, the case files it names,
and optionally a `fixture.memql` holding what the cases lean on (a concept a
query reads, a spec a filter names). The fixture is loaded beside every case in
the directory. Every `.memql` file in the directory is either the fixture or
named by a case; the runner refuses a file nothing names.

```json
{
  "cases": [
    { "file": "cache-seconds.memql", "verdict": "load_ok" },
    {
      "file": "cache-with-a-string.memql",
      "verdict": "refuse_parse",
      "code": "annotation_form",
      "message": "@cache on a query takes"
    }
  ]
}
```

| Field | Meaning |
|---|---|
| `file` | The case file, in this directory. |
| `verdict` | One of the five below. |
| `code` | The stable rule id a refusal carries, when it carries one. A refusal that carries one must have it named here: the id is the part of the contract a reworded message keeps. |
| `message` | Text the refusal must contain. Required for a refusal: the wording is part of the contract. |
| `concept` | For `lower` and `evaluate`: the concept, by bare name, the expression is over. The fixture declares it. |
| `position` | For `lower` and `evaluate`: the expression position. Under `expr/<position>/` it defaults to the directory. |
| `row`, `args`, `actor` | For `lower` and `evaluate`: the values the expression reads. A case binds only the roots its position has: `args` and `actor` are refused where the position has neither (a spec body, a trigger filter), and a prompt input has no `actor`. |
| `calls` | For `evaluate`: the answer to each construct call the expression makes, by `"<kind> <name>"` (`"query openTickets"`). The corpus boots no database, so what a call returns is the case's to state; the construct must be one the fixture declares and the engine registered, and a call is admitted only where the position admits one (a logic body, a step argument). |
| `sql` | For `lower`: text the lowered SQL must contain. |
| `expect` | For `evaluate`: the value the expression must produce. An absent result is `null`. |
| `note` | Why the case exists. Not checked. |

## The verdicts

| Verdict | The engine must |
|---|---|
| `load_ok` | parse the file and load it with no problem, and register every query, mutation, logic, spec, trait, tool and concept it declares. |
| `refuse_parse` | refuse the file when it parses it in the edition's grammar. |
| `refuse_load` | parse the file and refuse it at load. |
| `lower` | lower the expression to SQL containing `sql`. |
| `evaluate` | evaluate the expression against `row` to `expect`. |

The edition's grammar is the expression grammar of edition 2026, which the
loaders read behind `parser.Options.ExpressionsV1` until the embedded tree is
migrated and the option becomes the default. Until then the loaders still read
the four spellings it retires from the predicate positions -- a filter with no
lambda header, a spec or trait `{ return }` body, a raw-text `@filter` -- so
a refusal is judged by the edition's grammar, and a case that loads must parse
in both: the grammar the loaders read it with today, and the one they will
read it with after the flip. A cell written today therefore means the same
thing after the flip, without anyone touching the corpus.

A case file for `lower` or `evaluate` holds the expression alone -- at a
position written as a lambda over the row (a query filter, a spec body, a
trigger filter, a refine clause) the whole lambda, `row => ...` -- and every
other case file is a `.memql` file an author could write. Which engine answers
depends on the tier the position evaluates in (`test/conformance/engine_adapter_test.go`):

- **An in-process position** (tier M) answers through the edition-2026
  evaluator, `memql.EvalExpr`; a condition through `memql.EvalCondition`,
  which refuses a value that is not boolean. It has no SQL, so it takes no
  `lower` case.
- **A pushdown position** (tier P) answers through `memql.Lower` and the
  executor: `lower` is the SQL the query pushes down, and `evaluate` is the
  executor's own in-process post-filter over the row.
- **A literal position** (a sort key, an `@rowAuthz` argument, a tool
  `@default`) is not an expression: an `evaluate` case there holds a literal.

`now` reads `2026-01-02T03:04:05Z` in every case, so a case that reads the
clock has one answer on every run.

## What a load proves

A `load_ok` verdict is only evidence if the engine read the case, so the
runner rules out the two ways a load can raise no problem having read nothing:

- **A construct name two files declare** fails the corpus, naming both. Two
  domains declaring one name make every bare lookup of it ambiguous, so the
  load would say nothing about which declaration a call reaches.
- **A declaration the engine did not register** fails the case: the runner
  boots the cases that loaded a second time and looks each declared query,
  mutation, logic, spec, trait, tool and concept up. A construct form no loader
  recognises raises no problem, because nothing parsed it.

Every load case gets its own copy of the directory's fixture. A fixture two or
more load cases mount therefore declares no query, mutation or logic -- its
bare name would be ambiguous across the copies. A function one load case calls
belongs in that case's file; a fixture holds what every case may share:
concepts, specs and traits, and the functions only expression cases call.

## The completeness gates

Two gates read the corpus rather than a list kept beside it:

- `TestCorpusTierCompleteness` (`test/conformance/corpus_tiers_test.go`)
  parses every positive case at its position and fails naming each
  `(position, node kind)` the tier manifest admits that no positive case uses,
  each position with no refused case, each catalog function no positive case
  calls, and each positive case that uses what its position refuses. It logs
  how many of the manifest's cells it found covered.
- `TestCorpusRefusesEveryRetiredForm` fails naming each spelling edition 2026
  retires (`parser.V1RetiredForms()`, and the names the function catalog
  retires) that no refused case pins by its rule id with a message naming the
  replacement.

## Running it

```bash
go test -count=1 -run 'TestCorpus' ./test/conformance/
```

A failure names the file, the verdict it expected and what the engine did
instead. A bug in the language is not fixed until the case that shows it is
here.
