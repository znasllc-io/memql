# Capability-script contract

**Status:** accepted · **Epic:** [#2212](https://github.com/znasllc-io/memql/issues/2212) · **Issue:** [#2221](https://github.com/znasllc-io/memql/issues/2221) (I14)
**Companion:** [`dsl-behavioral-constructs-adr.md`](./dsl-behavioral-constructs-adr.md) · the [Execution model](../../../DEVOPS_DSL_BUNDLE_HANDOFF.md)

A **capability script** is the deterministic shell backend behind a DSL
`action`. The same script must run **identically** when invoked by the action
executor and when invoked by a human at a terminal. This document is the
contract that makes that true. It is the hardened successor to the
function-based-shell convention in `CLAUDE.md` ("Makefile + shell-script
convention") — that convention still holds (function-based, `set -euo
pipefail`, `main()` last); this adds the I/O and determinism rules a script
needs to be machine-driven.

## Why

The deployment bundle (Phase 3) expresses ops as `action` + `logic` +
`automation`. Per the ADR call-graph: **logic decides, automations
orchestrate, actions touch the world.** A capability script *is the world-touch*
— it must therefore carry **no decisions** of its own (those belong in `logic`),
must be **replayable** (idempotent), and must speak a **structured protocol** so
the executor can drive it and record its result without scraping human prose.

## The seven rules

1. **Single responsibility.** One script = one capability (`k3d.up`,
   `overlay.pinDigests`, `deploy.gate`). If it does two separable things, it is
   two capabilities. No "do everything" entrypoints.

2. **Idempotent.** Running it twice on the same input converges to the same
   state and the second run is a safe no-op. Report whether anything actually
   changed via `changed` (see the result schema) — never *assume* a fresh
   state.

3. **Non-interactive.** It MUST NOT block on a terminal: no `read -p`, no
   interactive confirmation prompts, no `select`, no "press any key". A run with
   stdin closed (`</dev/null`) must complete or fail cleanly. Confirmation for
   destructive operations is an **explicit parameter** (`--confirm=<phrase>`),
   not a prompt — see `cap_confirm_or_die`.

4. **Structured params in.** Inputs arrive, in precedence order:
   `--name=value` flags  >  a JSON object on **stdin** (opt-in via
   `--params-stdin`)  >  environment variables  >  documented defaults.
   The executor prefers stdin JSON; humans prefer flags/env. There are no
   positional arguments.

5. **Structured result out.** On **stdout**, the script emits **exactly one**
   JSON envelope (its result) and **nothing else**. All human-readable logging
   goes to **stderr**. This is the rule that lets one stream feed a machine and
   the other a human at the same time.

6. **Honest exit codes.** `0` = success. Non-zero = failure, and the code is
   meaningful and stable (see the standard codes). The exit code and the
   envelope's `ok`/`error` always agree.

7. **No decisions inside.** The script does not branch on *which environment*,
   *whether the gate is green*, *what the next version is*, or *who is allowed*.
   It takes already-decided inputs and executes. Environment/policy/version/role
   decisions live in DSL `logic`; the script is told what to do. (A script may
   still branch on *mechanics* — "does this resource already exist? then skip" —
   that is idempotency, rule 2, not a decision.)

## Result envelope

A single line of JSON on stdout:

```json
{
  "ok": true,
  "capability": "k3d.down",
  "changed": true,
  "result": { "cluster": "memql", "deleted": true },
  "error": null
}
```

On failure:

```json
{
  "ok": false,
  "capability": "k3d.down",
  "changed": false,
  "result": {},
  "error": { "code": 2, "message": "missing required parameter: cluster" }
}
```

| field        | type            | meaning                                                        |
|--------------|-----------------|----------------------------------------------------------------|
| `ok`         | bool            | success; always equals `exit == 0`                             |
| `capability` | string          | the capability id (matches `cap_init`)                         |
| `changed`    | bool            | did this run mutate state (idempotency signal)                 |
| `result`     | object          | capability-specific fields (digests, paths, counts, …)         |
| `error`      | null \| object  | `{ "code": <exit-code>, "message": <string> }` on failure      |

The action executor records `result` into the deployment record; the **gate**
verifies the outcome (ADR: "pin the procedure, capture the variance"). Outputs
that legitimately vary (image `@sha256`, live metrics) belong in `result`, not
in the exit code.

## Standard exit codes

| code | meaning                                                        |
|------|----------------------------------------------------------------|
| 0    | success                                                        |
| 1    | generic / unexpected failure (the `set -e` abort default)      |
| 2    | bad invocation: missing/invalid/unknown parameter              |
| 3    | refused: required confirmation not provided                    |
| 4    | precondition failed (a required tool/resource is absent)       |
| 5    | operation failed (the underlying command/effect failed)        |

Codes ≥ 2 are reserved meanings; capabilities may use higher codes for
domain-specific failures, but must keep them stable and documented in the
script header.

## The shared runtime: `scripts/lib/capability.sh`

Every capability script sources `scripts/lib/capability.sh`, which implements
the protocol so individual scripts stay focused:

```bash
#!/usr/bin/env bash
set -euo pipefail
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib/capability.sh"

cap_init "k3d.down" "Tear down the local k3d cluster."
cap_spec_param "cluster" "k3d cluster name" "MEMQL_K3D_CLUSTER"
cap_spec_param "purge"   "also purge the kubeconfig context (flag)" ""

function main() {
  cap_handle_meta "$@"          # --help / --print-spec short-circuit
  cap_parse_flags "$@"          # --name=value / --name into CAP_ARG_*
  local cluster purge
  cluster="$(cap_param cluster "${MEMQL_K3D_CLUSTER:-memql}")"
  purge="$(cap_flag purge)"

  if cluster_exists "$cluster"; then
    delete_cluster "$cluster"   # logs go to stderr via cap_info
    cap_changed
    cap_result_set_raw deleted true
  else
    cap_info "cluster '$cluster' absent — nothing to delete"
    cap_result_set_raw deleted false
  fi
  cap_result_set cluster "$cluster"
  cap_ok
}
main "$@"
```

Key helpers (full reference in the file header):

| helper                          | purpose                                                   |
|---------------------------------|-----------------------------------------------------------|
| `cap_init <id> <summary>`       | declare the capability + install the result-guarantee trap |
| `cap_spec_param <n> <d> [env]`  | document a param for `--print-spec` / `--help`            |
| `cap_handle_meta "$@"`          | handle `--help` / `--print-spec` and exit                 |
| `cap_parse_flags "$@"`          | parse `--k=v` / `--k` into `CAP_ARG_*`; reject unknowns    |
| `cap_param <name> [default]`    | resolve a param (flag > stdin JSON > default)             |
| `cap_flag <name>`               | the parsed `--name` flag value                            |
| `cap_require <name> <value>`    | fail (exit 2) when a required param is empty              |
| `cap_confirm_or_die <got> <exp>`| non-interactive replacement for a `read -p` confirmation  |
| `cap_info/cap_warn/cap_error`   | human logging — **to stderr**                            |
| `cap_result_set <k> <strval>`   | add a string field to `result`                            |
| `cap_result_set_raw <k> <json>` | add a number/bool/object field to `result`                |
| `cap_changed`                   | mark that this run mutated state                          |
| `cap_ok [rawjson]`              | emit the success envelope + exit 0                        |
| `cap_fail <code> <message>`     | emit the failure envelope + exit `<code>`                 |

The library installs an `EXIT` trap so that **even an uncaught `set -e` abort
emits a failure envelope** — there is no silent death. Success paths must call
`cap_ok` explicitly; the conformance test enforces this.

## Capability descriptor (`--print-spec`)

Every script answers `--print-spec` with a machine-readable descriptor — the
surface registry (I13) and the action executor use it to discover params:

```json
{ "capability": "k3d.down", "summary": "Tear down the local k3d cluster.",
  "params": [ {"name":"cluster","description":"k3d cluster name","env":"MEMQL_K3D_CLUSTER"},
              {"name":"purge","description":"also purge the kubeconfig context (flag)","env":""} ] }
```

## How the action executor drives a capability

The `cockpit/runner` surface (I13) resolves a DSL `action` to its capability
script and runs it; `component/deploycontrol`'s `Executor` is the in-process
seam for the Go side. The drive sequence:

1. Render the action's `argTemplate` / params into a JSON object.
2. Invoke `script --params-stdin` with that JSON on stdin (or expand to flags).
3. Capture **stdout** → parse the single JSON envelope; **stderr** → logs.
4. Trust the **exit code**: non-zero ⇒ failure, surface `error.message`.
5. Record `result` into the deployment record (mutation); the gate verifies.

Because logs are on stderr and the result is the only thing on stdout, the
executor never has to scrape prose, and a human running the same command sees
the logs inline and can `| jq` the result.

## Conformance

`scripts/lib/capability_contract_test.go` (`go test ./scripts/lib/`) statically
and dynamically enforces the contract on every capability script (those that
source `capability.sh`):

- sources `scripts/lib/capability.sh`;
- calls `cap_init`;
- contains **no interactive prompt** (`read -p` / `read -rp` / `select`);
- answers `--print-spec` with a valid descriptor whose `capability` matches
  `cap_init`;
- runs cleanly with stdin closed (no blocking).

New capability scripts are picked up automatically. A script that is *not* a
capability backend (a pure status reporter, a dev convenience) need not adopt
the contract, but anything an `action` resolves to MUST.

## Migrating an existing script

1. `source scripts/lib/capability.sh`; add `cap_init` + `cap_spec_param`s.
2. Route every human `echo`/`info` to **stderr** (use `cap_info`/`cap_warn`/`cap_error`).
3. Replace the final human "done" output with `cap_result_set*` + `cap_ok`.
4. Replace any `read -p` confirmation with a `--confirm=<phrase>` param + `cap_confirm_or_die`.
5. Remove environment **decisions** (e.g. `case $ENV in staging|prod)` that
   *chooses behavior*) — accept the already-decided inputs as params. Mechanical
   idempotency branches stay.
6. Add the `--help` / `--print-spec` handling via `cap_handle_meta`.

## Scope note (engine cluster)

The north-star bundle drives the **engine mesh only** (identity, cognition,
voice, agent, planner, workbench, mcp, voice-agent — no `bff`, no `copresent`).
The local engine path (`scripts/k3d/*`) is the reference implementation of this
contract. Staging/prod Azure/ArgoCD scripts (`scripts/deploy/*`) converge here
via the Makefile/ArgoCD refactor track; this contract governs them as they are
adopted as capability backends, and their interactive confirmations are
removed now (rule 3) regardless.
