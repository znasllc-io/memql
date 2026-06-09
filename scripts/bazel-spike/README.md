# Bazel build-graph spike (CI Tier 4 / #859)

EXPERIMENTAL scaffold for the CI-acceleration north star. This is NOT part
of the build: nothing in `.github/workflows/**` or the `Makefile`
references it, and no root-level Bazel files are committed to `main`.

- Plan + decision record: [`docs/internal/ops/tier4-build-graph.md`](../../docs/internal/ops/tier4-build-graph.md)
- `bootstrap.sh` — run on a scratch branch to generate the root Bazel
  files, run Gazelle, and attempt a first `bazel build`. It refuses to run
  on `main`.

```bash
git switch -c spike/bazel
bash scripts/bazel-spike/bootstrap.sh
# iterate per the "memQL-specific hard parts" in the plan doc
```

Goal of the spike: a green `bazel test //...` plus a demonstrated
cross-machine remote-cache hit, which then justifies adding a non-required
Bazel CI lane (Tier 4 step 2). Until then, Tiers 0–3 (#855–#858) carry the
practical CI speedup.
