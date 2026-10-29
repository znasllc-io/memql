# Portable release and qualification tools

These are source snapshots, not installed products. No binary, credential store,
worker credential, enrollment configuration, Azure binding, private input path,
or completed Warehouse secret-seeding tool is included. Normal owner login on
the destination machine must use the supported Cockpit flow; never copy another
machine's session or keyring.

## Release helper setup

Use a full canonical `znasllc-io/memql` checkout with the reviewed generated SDK.
The captured helper declares Go 1.26.8 and was built with the explicitly selected
Go 1.27.3 toolchain. The generated workspace does not select that toolchain for
you. Preserve its `.../cockpit/releaseoperator` module path because Go's
internal-import rule is required for normal Cockpit authentication.

Set `MEMQL_SOURCE` to that local checkout and `TOOL_DIR` to this package's
`tools/release-operator` directory. Python 3.9+ and an installed Go toolchain are
required. The setup step reads the checkout's workspace with
`go work edit -json`, then writes only the helper's ignored local `go.work`.
It refuses unknown replacement directives or a different existing binding.
It does not build, download a toolchain, log in, or contact production.

```sh
python3 "$TOOL_DIR/setup_workspace.py" --source-tree="$MEMQL_SOURCE"
python3 "$TOOL_DIR/setup_workspace.py" --source-tree="$MEMQL_SOURCE" --execute
```

After reviewing the source and checkout compatibility, build the helper **in
this directory** so its retained replay guard remains adjacent to the binary:

```sh
cd "$TOOL_DIR"
GOTOOLCHAIN=go1.27.3 GOWORK="$TOOL_DIR/go.work" go test ./...
GOTOOLCHAIN=go1.27.3 GOWORK="$TOOL_DIR/go.work" go build -o "$TOOL_DIR/release-operator" .
```

The existing owner cluster must be named `memql-znas-io`, use
`https://api.memql.znas.io` (optionally explicit port 443), and have issuer
`https://identity.memql.znas.io`. The helper resolves the normal credential
store in-process and accepts only an owner session. Serialize authenticated
helper calls to avoid concurrent session-refresh races. Do not run a server
binary to discover its version; inspect connection metadata instead.

Use exact `run`, `steps`, and `work-run` reads for bounded monitoring. The broad
`runs` list previously exceeded the transport limit. Native writes are plans
until `--execute`; they require fresh reviewed source, current policy, head,
admission, controller, candidate, and destination evidence. A build does not
authorize a mutation. Retained candidate assets can be read through
`candidate-assets` only after the exact native candidate exists.

### Historical retries: never replay

The embedded retry source snapshots describe obsolete original attempts. Their
heads have been superseded. Keep them byte-identical as source evidence; do not
interpret them as present eligibility. The failed-only call for source
`cec99162f28f29f58fb802029ca05864` was already issued once and created native
run `4e67e38e52167d057ff7ba3a99513e4a`. Its exact durable marker is preserved at
`release-operator/rerun-intents/cec99162f28f29f58fb802029ca05864.json`.

Do not delete, relocate, regenerate, or ignore that marker, and do not place
the binary elsewhere. Its `intent-recorded` state intentionally remains after
success; it is a replay refusal marker, not a pending action. It protects only
the adjacent executable directory and cannot replace checking durable native
history across operators. The two whole-rerun source IDs were also superseded;
neither is a next action. All historical retry operations are out of scope for
resumption. A future supported retry requires new independent review.

## Cockpit artifact qualification

`public-release-qualification` contains unchanged Python source and tests.
Requirements are Python 3.9+, Go for static binary metadata, and macOS
`/usr/bin/codesign` for signature checks. On other operating systems, signature
inspection is explicitly unexecuted, not passed. Standalone installation is
restricted to native macOS in a temporary subprocess HOME/BIN_DIR/TMPDIR; no
worker enrollment, service operation, permission enablement, or app launch is
part of these tools. Linux installation needs separate isolation because the
installer can create desktop menu entries.

`verify-retained.py` inspects the candidate-assets output offline before any
publication. `qualify.py` verifies the eventual public release and exact
annotated tag dereference, 41 assets, candidate digests, checksums, SBOMs,
platform/version/source/variant metadata, bundle IDs, and available static
signature evidence. Use the actual next reviewed tag/source/candidate; old
`cockpit/v0.21.0` examples are not valid for the repaired installer. Installation
mode must follow complete provenance review. Cross-platform assets are only
statically inspected; notarization and first-launch behavior remain unproven.

`fleet-fixtures.py` runs six existing Fleet tests using synthetic fixtures and
a temporary subprocess HOME. Prepare dependencies from the checkout's lockfiles
in `sdk/ts` and `clients/os`: run `npm ci --ignore-scripts --no-audit --no-fund`
in each, and `npm run build` in `sdk/ts` before running the Fleet tests. Do not
copy `node_modules`. Historical clean installer head `e0e4f834` passed all 108
tests; rerun against the actual release source when needed. No generated Fleet
command is executed by the fixture runner.

The wrappers default to plan mode. Read their `--help` output and use a fresh
output directory; no release candidate or completed public qualification is
bundled here.

## Release ordering and completed secret setup

Cockpit and engine/docs releases need distinct reviewed commits and component
tags; Cockpit can go first. Use the native `cockpit` profile without engine
producers. Engine/docs publication is still needed before retiring the old
repository. Preserve existing immutable tags, external runs, and operators.
Refresh all version, policy, merge, admission and active-run facts after handoff.
Do not deploy a controller over active runs or enable the owner's disabled Mac
pipeline consent.

Warehouse package credential setup was already completed and verified with a
bounded package read. It must not be seeded again. This package intentionally
contains no seeder, vault identifiers, private input, or seeder intent. A
successful package read does not prove a complete native Warehouse build.
