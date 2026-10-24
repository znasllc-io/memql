# MemQL Cockpit

Cockpit is the machine-side worker and the `memql` operator command. It is a
separate Go module in the MemQL monorepo. Read the repository's AGENTS.md and
the MemQL UI design skill before changing human-facing output or native UI.

- Build from the repository root with `make cockpit`, or `make -C cockpit
  cockpit-computeruse` for the native computer-use variant.
- Run `go test ./cockpit/...` from the root. `make test` includes the module.
- Engine and Cockpit source always come from the same checkout. There is no
  sibling checkout or engine pin. `GOWORK=off` remains the module boundary check.
- Keep `cockpit/VERSION`, the worker's fallback version and the command's
  fallback version in agreement. Cockpit releases use `cockpit/vX.Y.Z` tags;
  engine releases use `vX.Y.Z`. A tag does not select an installed release.
- Preserve the worker protocol compatibility checks, consent, OS permission
  checks, credential isolation, signing identity and installer checksums.
- The command is still `memql`. The engine command runs inside its containers;
  do not rename either command or add a Cockpit engine-node build tag.

Source imported from `znasllc-io/memql-cockpit` at
`669820bd06f8f008982f174af0e975f6aef479b4`, including the streaming and retained
evidence companion changes. Earlier history remains in that repository.
