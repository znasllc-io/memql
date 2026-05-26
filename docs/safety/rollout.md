# Safety Classifier — Rollout Runbook

**Status:** v1 substrate shipped (#226 epic complete: #227-#235).
**Default mode:** shadow end-to-end.
**Audience:** operators responsible for flipping `enforce` per-surface.

This is the playbook for moving memQL's safety classifier from
shadow (observe-only) to enforce (live blocking) per surface. The
substrate is built so the rollout is **incremental + reversible**:
one surface at a time, env-only knobs, no code deploy needed for
a rollback.

---

## What's running today (in shadow)

Two classifier substrates run on every node, both in shadow mode by
default:

| Substrate | What it screens | Verdicts | Persistence concept |
|---|---|---|---|
| **Command classifier** (#227-#231) | Actions before dispatch — workbench exec, computer-use, tool invocations | Allow / Ask / Deny | `v1:safety:classification` |
| **Output screener** (#233) | Incoming content before model re-ingestion — http_fetch bodies (more surfaces in follow-ups) | Clean / Suspicious / Blocked | `v1:safety:outputScreening` |

Both write one row per evaluation to their concept (since #234).
Shadow rows are the substrate for the FP/FN measurement that
drives the enforce-flip decision.

---

## Pre-flight checklist (before flipping ANY enforce knob)

1. **Run the red-team corpus tests locally:** `go test ./component/safety/ -run TestCommandCorpus -run TestScreenCorpus`. Both must pass — they're the FP/FN budget gate and ride CI on every commit.
2. **Measure 7-day block-rate per surface from shadow:**
   ```sql
   SELECT payload->>'surface', payload->>'decision', COUNT(*)
   FROM memory_nodes WHERE concept = 'v1:safety:classification'
     AND payload->>'mode' = 'shadow' AND created_at > NOW() - INTERVAL '7 days'
   GROUP BY 1, 2;
   ```
   For each surface, compute `would-block / total`. The acceptable
   threshold is operator judgement -- a workbench surface
   running 0.1% Deny + 2% Ask is a green-light to flip; 30% Deny on
   a previously-functional agent flow needs investigation first.
3. **Sample 20 Deny + 20 Ask rows manually.** Look for false positives -- legitimate work the agent was doing that the classifier
   misread. If FPs > ~5%, tighten the offending rule + add a corpus
   benign entry before flipping enforce. The corpus is the CI gate
   against the same regression in the other direction.
4. **Identify the rollback step BEFORE the flip.** Note which env
   var you're setting so you (or pager-duty) can revert in one
   command.

---

## Env-knob taxonomy

Every enforce knob has the same shape: `MEMQL_SAFETY_<dimension>_MODE` (or `_FAIL_CLOSED` / `_SUSPICIOUS_AS_BLOCKED`). All knobs default to safe (shadow for mode; fail-default-for-surface for fail-closed; opt-in pass-through for suspicious-as-blocked).

### Command classifier mode (per surface)

| Surface | Env var | Default |
|---|---|---|
| Global fallback | `MEMQL_COMMAND_CLASSIFIER_MODE` | `shadow` |
| Workbench (sandboxed) | `MEMQL_SAFETY_WORKBENCH_MODE` | (global) |
| Computer-use headless | `MEMQL_SAFETY_COMPUTER_USE_HEADLESS_MODE` | (global) |
| Computer-use embodied | `MEMQL_SAFETY_COMPUTER_USE_EMBODIED_MODE` | (global) |
| Tool webhook | `MEMQL_SAFETY_TOOL_WEBHOOK_MODE` | (global) |
| Tool integration | `MEMQL_SAFETY_TOOL_INTEGRATION_MODE` | (global) |

Values: `off` / `shadow` / `enforce`.

### Output screener mode (per content type / ingress channel)

| Ingress | Env var | Default |
|---|---|---|
| Global fallback | `MEMQL_SAFETY_OUTPUT_SCREEN_MODE` | `shadow` |
| HTTP fetch (highest risk) | `MEMQL_SAFETY_OUTPUT_HTTP_FETCH_MODE` | (global) |
| Tool output | `MEMQL_SAFETY_OUTPUT_TOOL_OUTPUT_MODE` | (global) |
| File read | `MEMQL_SAFETY_OUTPUT_FILE_READ_MODE` | (global) |
| Knowledge seed | `MEMQL_SAFETY_OUTPUT_KNOWLEDGE_SEED_MODE` | (global) |

Values: same as above.

### Suspicious-as-Blocked (per content type)

Output screener has a Suspicious tier for lower-confidence rules
(jailbreak terms in isolation, role-label injection, etc.). By
default Suspicious passes through; opt-in escalates to Blocked.

| Ingress | Env var | Default |
|---|---|---|
| HTTP fetch | `MEMQL_SAFETY_OUTPUT_HTTP_FETCH_SUSPICIOUS_AS_BLOCKED` | `false` |
| Tool output | `MEMQL_SAFETY_OUTPUT_TOOL_OUTPUT_SUSPICIOUS_AS_BLOCKED` | `false` |
| File read | `MEMQL_SAFETY_OUTPUT_FILE_READ_SUSPICIOUS_AS_BLOCKED` | `false` |
| Knowledge seed | `MEMQL_SAFETY_OUTPUT_KNOWLEDGE_SEED_SUSPICIOUS_AS_BLOCKED` | `false` |

Values: `true` / `1` / `yes` / `on` (any other value = false).

### Fail-closed posture (per surface)

Controls whether a classifier ERROR (provider outage, etc.) blocks
the dispatch (fail-closed) or lets it through (fail-open). Each
surface has a baked-in default by blast radius; env overrides.

| Surface | Env var | Default |
|---|---|---|
| Workbench | `MEMQL_SAFETY_WORKBENCH_FAIL_CLOSED` | `false` (open) |
| Computer-use headless | `MEMQL_SAFETY_COMPUTER_USE_HEADLESS_FAIL_CLOSED` | `true` (closed) |
| Computer-use embodied | `MEMQL_SAFETY_COMPUTER_USE_EMBODIED_FAIL_CLOSED` | `true` (closed) |
| Tool webhook | `MEMQL_SAFETY_TOOL_WEBHOOK_FAIL_CLOSED` | `false` (open) |
| Tool integration | `MEMQL_SAFETY_TOOL_INTEGRATION_FAIL_CLOSED` | `false` (open) |

Values: `true` / `1` / `yes` / `on` => fail-closed; `false` / `0` / `no` / `off` => fail-open. Anything else falls through to the default.

### Persistence opt-out (one global knob, both substrates)

| Knob | Env var | Default |
|---|---|---|
| Persist classification + screening rows | `MEMQL_SAFETY_PERSIST_CLASSIFICATIONS` | (on when engine ready) |

Set to `off` / `false` / `0` to disable persistence -- slog still records every verdict, but the DSL rows don't get written. Useful in test / dev when DB is intentionally absent.

### LLM screener opt-in (command classifier; the LLM screener for output is deferred)

| Knob | Env var | Default |
|---|---|---|
| Add LLM layer to command classifier chain | `MEMQL_SAFETY_LLM_PROVIDER` | unset (rules-only) |

Set to the name of a registered structured-chat provider to enable the LLM classifier for the ambiguous middle that rules don't cover.

### Approval substrate (memql#232)

| Knob | Env var | Default |
|---|---|---|
| Persist approval requests | `MEMQL_SAFETY_APPROVAL_SINK` | (on when engine ready) |
| Approval TTL (hours) | `MEMQL_SAFETY_APPROVAL_TTL_HOURS` | `24` |

`MEMQL_SAFETY_APPROVAL_TTL_HOURS=0` is a legitimate posture (no bypass tokens -- every Ask requires fresh approval).

---

## Recommended rollout sequence

Order is by ascending blast-radius. After each step, **measure for 7 days before advancing**. If anything looks off, revert the env var (no deploy needed for the rollback).

1. **Flip output-screener http_fetch to enforce.** Lowest risk because (a) HTTP fetched content is the highest-confidence attack surface and (b) the failure mode is "agent reads sanitised stub" not "agent crashes":
   ```
   MEMQL_SAFETY_OUTPUT_HTTP_FETCH_MODE=enforce
   ```
   Watch: `v1:safety:outputScreening WHERE verdict='blocked' AND mode='enforce'`. Expect a small uptick in `[CONTENT BLOCKED BY OUTPUT SCREENER]` stubs in agent context.

2. **Flip tool-webhook command classifier to enforce.** Tool webhooks have typed signatures + their own validation, so blast radius is bounded:
   ```
   MEMQL_SAFETY_TOOL_WEBHOOK_MODE=enforce
   ```
   Watch: `decision='deny' AND surface='tool_webhook' AND mode='enforce'`. Investigate every Deny row for false-positive risk.

3. **Flip workbench command classifier to enforce.** Per-Plan sandbox, blast radius bounded:
   ```
   MEMQL_SAFETY_WORKBENCH_MODE=enforce
   ```

4. **(Optional) Escalate http_fetch Suspicious to Blocked.** Once http_fetch enforce has been stable for ~2 weeks + the corpus + the Suspicious-tier false-positive rate is acceptable:
   ```
   MEMQL_SAFETY_OUTPUT_HTTP_FETCH_SUSPICIOUS_AS_BLOCKED=true
   ```

5. **Flip computer-use to enforce.** Highest blast radius (user's real machine) -- enforce ONLY after everything else has been stable for ~30 days + you've manually reviewed the would-be-blocked corpus:
   ```
   MEMQL_SAFETY_COMPUTER_USE_HEADLESS_MODE=enforce
   MEMQL_SAFETY_COMPUTER_USE_EMBODIED_MODE=enforce
   ```
   Computer-use already defaults `fail_closed=true`, so a classifier outage blocks dispatch. If that's not the desired posture (e.g. during incident response), flip:
   ```
   MEMQL_SAFETY_COMPUTER_USE_HEADLESS_FAIL_CLOSED=false
   ```

---

## Rollback procedure

Any flip is reversible by setting the same env var to `shadow` (or `off`) and restarting the node. No code deploy needed.

If a regression makes EVERYTHING block, the nuclear option:
```
MEMQL_COMMAND_CLASSIFIER_MODE=off
MEMQL_SAFETY_OUTPUT_SCREEN_MODE=off
```
Both substrates skip evaluation entirely -- zero-cost bypass. Persistence stops; slog still narrates "classifier off." Use this only as a last resort -- it disables the protection on every surface at once.

---

## Adding new rules

The red-team corpus is the spec. Every new rule lands with at least one corpus entry (malicious case it catches) + ideally one benign entry (lookalike that must NOT trip). The CI test (`corpus_command_test.go` / `corpus_screen_test.go`) enforces the FP/FN budget at PR time.

Adding a rule:
1. Author the rule in `component/safety/rules_shell*.go` (command) or `component/safety/screen_rules.go` (output).
2. Register in `DefaultRules()` (command) or `DefaultScreenRules()` (output).
3. Add at least one corpus entry to the appropriate `_test.go`.
4. Run `go test ./component/safety/ -run Corpus`. Both the new entry and ALL existing entries must pass -- no regression.

Loosening / tightening an existing rule:
1. Update the regex.
2. If a benign case starts failing (FP regression), add the case to the benign corpus + tighten further.
3. If a malicious case starts failing (FN regression), the rule loosen-pass is wrong -- revert + try a different shape.
4. CI catches both directions on every PR.

---

## References

- Epic: #226
- Substrate: #227-#234 (foundation, rules, enforcement wiring, LLM, decision policy, approval, audit, output screening)
- Rollout (this doc): #235
- Concepts: `v1:safety:classification` (#234), `v1:safety:approvalRequest` (#232), `v1:safety:outputScreening` (#233)
- Code: `component/safety/` + `component/safety/recorder/` + `component/safety/approval/`
