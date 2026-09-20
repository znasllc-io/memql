# Fuzz corpus

Inputs that once broke the lexer, the parser or the lowering. `FuzzParse`
(`test/conformance/fuzz_test.go`) feeds the edition's front end and the parser
every case file in this edition's corpus, every `.memql` file here, and
whatever the fuzzer derives from them. The parser may refuse any input; it may
never panic.

**This directory grows one way: from found failures.** When a fuzz run finds an
input that breaks the parser, the input goes here as a `.memql` file named
after what it broke, beside the fix. `go test` then runs it on every build --
because a bug is not fixed until its case is in the corpus (`docs/CLAUDE.md`).

Do not add speculative inputs here. The chosen seed sets live beside their
targets, in Go's own `go test fuzz v1` seed format, and the whole conformance
corpus is already seeded into `FuzzParse` at run time through `f.Add` -- so a
`.memql` file copied here from elsewhere in the corpus is a duplicate the
fuzzer was already starting from.

## Targets and where their seeds live

| Target | Package | Seeds |
|---|---|---|
| `FuzzLexer` | `component/language/parser` | `testdata/fuzz/FuzzLexer/` -- the shapes a lexer gets wrong |
| `FuzzParseV1Expression` | `component/language/parser` | `testdata/fuzz/FuzzParseV1Expression/` -- every retired expression spelling beside the form that replaces it |
| `FuzzLower` | `component/memql` | `testdata/fuzz/FuzzLower/` -- the absence table, as `row => ...` lambdas |
| `FuzzEvalExpr` | `component/memql` | `testdata/fuzz/FuzzEvalExpr/` -- the same absence table as BARE expressions |
| `FuzzParse` | `test/conformance` | every corpus `.memql`, via `f.Add`, plus this directory |

The committed seed sets are written by `scripts/dev/fuzz-seeds.sh`, not by
hand: a malformed seed file is ignored by Go with no warning, so a lost
backslash removes a seed silently. That script only ADDS its own `seed-<slug>`
files; it never cleans a target directory, because those directories also hold
the inputs a fuzz run found, and deleting one throws away the only record that
the bug existed.

**`FuzzLower` and `FuzzEvalExpr` take different input shapes, and the
difference is easy to get wrong.** `FuzzLower` unwraps a one-parameter lambda
and lowers its body, so `row => row.title == nil` is the right seed there.
`FuzzEvalExpr` evaluates whatever it parses, so the same text evaluates to a
lambda VALUE and exercises none of the absence rules; its seeds are bare
expressions over the fixed scope in `expr_eval_fuzz_test.go`.

## Running

```bash
go test -run '^$' -fuzz FuzzParse ./test/conformance/
go test -run '^FuzzLexer$' -fuzz '^FuzzLexer$' -fuzztime=60s ./component/language/parser/
```

CI fuzzes each target for 40 seconds in the `go-checks` job. That is a search,
not a proof; the seeds themselves run as ordinary subtests on every `go test`,
which is the regression guarantee.

**Verify a seed is actually being read by COUNTING SUBTESTS, not by reading
the file.** Go names a seed from `f.Add` `seed#N` and a seed from `testdata/`
after its filename, so `grep -c 'seed#'` does not move when a file is added
and is not the check:

```bash
go test ./component/language/parser/ -run 'FuzzLexer|FuzzParseV1Expression' -v \
  | grep -c '^=== RUN   Fuzz'
```

If that count does not move when a file is added, the file's format is wrong
and Go is skipping it.
