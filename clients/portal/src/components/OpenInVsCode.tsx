import { useState, type ReactNode } from "react";

import {
  EXTENSION_INSTALL_URL,
  editorOpenUri,
  readStoredEditorScheme,
  storeEditorScheme,
  type EditorScheme,
} from "../cluster/editorLink";
import { ButtonLink } from "../ui";
import { ExternalLink } from "../ui/icons";

// The handoff control (design section 4.2). The portal renders no source --
// this is the one door to it, and it opens in the editor. Nothing here can
// tell whether the extension is installed, so the install pointer is always
// beside the link and the copy says what the link needs.
//
// The id is referenced only through the `name` prop: this directory is on
// the concept-agnostic render path and may not name a concept.
export function OpenInVsCode({ domain, kind, name }: { domain: string; kind: string; name: string }): ReactNode {
  const [scheme, setScheme] = useState<EditorScheme>(() => readStoredEditorScheme());
  if (domain === "") return null;

  const insiders = scheme === "vscode-insiders";
  const editorName = insiders ? "VS Code Insiders" : "VS Code";
  const other: EditorScheme = insiders ? "vscode" : "vscode-insiders";
  const otherName = insiders ? "VS Code" : "VS Code Insiders";

  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
      <ButtonLink href={editorOpenUri({ scheme, domain, kind, name })} title={`Opens the definition in ${editorName}`}>
        <ExternalLink size={13} aria-hidden="true" />
        <span>Open definition in {editorName}</span>
      </ButtonLink>
      <span className="text-xs text-muted">
        Needs the MemQL extension for VS Code.{" "}
        <a href={EXTENSION_INSTALL_URL} target="_blank" rel="noopener noreferrer" className="text-accent hover:underline">
          how to install
        </a>
        {" - "}
        <button
          type="button"
          className="text-accent hover:underline"
          onClick={() => {
            storeEditorScheme(other);
            setScheme(other);
          }}
        >
          Use {otherName}
        </button>
      </span>
    </div>
  );
}
