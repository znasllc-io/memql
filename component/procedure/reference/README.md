# Reference implementations -- the research sidecar of design D6

These are **not product**. Design D6 keeps an external mining sidecar "as a
research harness for validating the Go implementation against reference
algorithms, never as product: it would put a second runtime into
product-agnostic engine images and make the learning something other than
rows."

Nothing in `component/procedure` imports this package, and the purity gate
(`purity_test.go`) covers that package rather than this directory.

## Running it

`python3` on `PATH`, no packages:

```bash
go test ./component/procedure/reference/...
```

Without `python3` the harness **skips**, loudly, naming what to install. A
harness that failed when a research sidecar is missing would red the build for
everyone; one that skipped silently is a harness nobody notices has never run.

## What is here

| File | What it is |
|---|---|
| `ref_lcs.py` | Anti-unification distance, from the definition: everything outside a longest common subsequence had to be generalized away |
| `ref_mine.py` | Closed frequent sub-sequences with a gap tolerance, ranked by coverage then cohesion |
| `ref_lcs_wrong.py` | **Deliberately wrong**, committed on purpose |

The references are written from the DEFINITIONS, never transliterated from the
Go. A reference derived from the implementation it checks agrees with it by
construction and proves nothing.

`ref_lcs_wrong.py` is the negative control. It counts positional mismatches
instead of aligning on a common subsequence -- the exact mistake
`symbolize.go`'s comment warns about -- and
`TestParity_TheHarnessFailsWhenAReferenceDisagrees` fails if the Go agrees with
it. Without that test a green parity run would only mean the harness ran.

## Fixtures are shared

Both references read the same `../testdata/*.json` the golden tests read. A
harness with its own fixtures proves the references agree with themselves.
