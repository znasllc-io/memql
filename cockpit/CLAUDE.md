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

The imported source is followed by the reviewed input guard and retained-output
repairs from `memql-cockpit` through `7a0eee681c6a`. Preserve them when updating
the worker: input is bound to a fresh screenshot receipt and retained window;
interruption latches, with no automatic reacquisition. See
[computer use](docs/computer-use.md) for sampled target checks and platform
limitations. App conversations isolate provider state and serialise attachment
across processes; browser runtime profiles stay out of output archives and
inline retained content. See [local Apps](docs/local-apps.md) for the recording,
recovery and exclusion rules. These source changes do not extend local consent
or authorize input in an active task.
