// The "Before it runs" checklist on the install / repair collect screen
// (memql#4195).
//
// The wizard already KNEW these three facts before starting a run -- the graph
// document either loads or the run refuses at its first step, the graph either
// carries elevated steps or not, and the receipt either records a usable
// provider-key path or the form will ask for one. It just told nobody until
// the moment each fact bit. A guided flow states them up front, so the
// operator knows what the run will need before clicking it.
//
// PURE PROJECTION: the panel gathers the inputs (graph load, `sudo -n` probe,
// receipt read) and this module only words them, so the wording is testable
// under bare `node --test` (cmd/memql-lsp/vscodeimportrule_test.go).

export interface PreflightItem {
  label: string;
  /** "ok" renders quiet; "attention" renders emphasised. Never a blocker -- the run itself enforces. */
  state: "ok" | "attention";
  detail: string;
}

export interface PreflightInputs {
  action: "install" | "installGuided" | "repair";
  /** The graph document's fate: step count, or why it could not be read. */
  graph: { ok: true; steps: number; needsElevation: boolean } | { ok: false; error: string };
  /** Whether sudo would run without asking (or the process is root). */
  sudoFree: boolean;
  /** The receipt's usable provider-key path, "" when none is recorded. */
  recordedKeyPath: string;
}

export function preflightItems(inputs: PreflightInputs): PreflightItem[] {
  const items: PreflightItem[] = [];

  if (inputs.graph.ok) {
    items.push({
      label: "Install graph",
      state: "ok",
      detail: `${inputs.graph.steps} steps, loaded. Every step verifies first and skips when already satisfied.`,
    });
  } else {
    items.push({
      label: "Install graph",
      state: "attention",
      detail: `could not be read (${inputs.graph.error}). The run will refuse before its first step.`,
    });
  }

  if (!inputs.graph.ok || !inputs.graph.needsElevation) {
    items.push({
      label: "Privileges",
      state: "ok",
      detail: "No step needs elevation on this run.",
    });
  } else if (inputs.sudoFree) {
    items.push({
      label: "Privileges",
      state: "ok",
      detail: "Some steps edit system files; sudo on this machine runs without asking.",
    });
  } else {
    items.push({
      label: "Privileges",
      state: "attention",
      detail:
        "Some steps edit system files (the hosts file, the certificate store). " +
        "Your password will be asked once, by the editor's own prompt, and held in memory for this run only.",
    });
  }

  if (inputs.action === "repair") {
    items.push(
      inputs.recordedKeyPath === ""
        ? {
            label: "Provider key file",
            state: "attention",
            detail: "No usable path is recorded from the last install; the form asks for one.",
          }
        : {
            label: "Provider key file",
            state: "ok",
            detail: `Recorded from the last install: ${inputs.recordedKeyPath}`,
          },
    );
  } else {
    items.push({
      label: "Provider key file",
      state: "ok",
      detail: "You name a PATH to a file holding the key, below. The key itself never leaves that file.",
    });
  }

  return items;
}
