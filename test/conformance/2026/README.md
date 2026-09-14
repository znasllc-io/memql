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
named by a case; the runner refuses a file nothing names. Any other file in the
directory -- a prompt's or a seed's `@templateFile`, a `namespace.pin` -- is
mounted beside every case in it under the same name, as it would sit beside a
domain's `.memql` files.

Each case loads as its own domain, and the load cases share boots, so the
names a case declares must be unique across the corpus: a bare name two
domains declare is ambiguous to every lookup by bare name (a tool's handler, an
automation's step). A cell's constructs carry the annotation's name for that
reason (`openTicketsCache`, `retitleTicketServerOnly`).

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
| `code` | The stable rule id a refusal carries, when it carries one. |
| `message` | Text the refusal must contain. Required for a refusal: the wording is part of the contract. |
| `concept` | For `lower` and `evaluate`: the concept, by bare name, the expression is over. The fixture declares it. |
| `position` | For `lower` and `evaluate`: the expression position. Under `expr/<position>/` it defaults to the directory. |
| `row`, `args`, `actor` | For `lower` and `evaluate`: the values the expression reads. |
| `sql` | For `lower`: text the lowered SQL must contain. |
| `expect` | For `evaluate`: the value the expression must produce. |
| `note` | Why the case exists. Not checked. |

## The verdicts

| Verdict | The engine must |
|---|---|
| `load_ok` | parse the file and load it with no problem. |
| `refuse_parse` | refuse the file when it parses it. |
| `refuse_load` | parse the file and refuse it at load. |
| `lower` | lower the expression to SQL containing `sql`. |
| `evaluate` | evaluate the expression against `row` to `expect`. |

A case file for `lower` or `evaluate` holds the expression alone; every other
case file is a `.memql` file an author could write.

## Running it

```bash
go test -count=1 -run TestCorpusVerdicts ./test/conformance/
```

A failure names the file, the verdict it expected and what the engine did
instead. A bug in the language is not fixed until the case that shows it is
here.
