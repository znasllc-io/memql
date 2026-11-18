import { createContext, useContext } from "react";
import { Users } from "lucide-react";

// WHO A FILE IS SHARED WITH (Jesús, Nov 18 2026): "Sharing with: (name)" at the
// foot of the Files rail, and only while the selected file is shared.
//
// THE ENGINE HAS NO SHARE RECORD YET. Sharing a file with a person for a set
// time, with a notification and a download, is the next Files piece (José,
// Nov 17). Until it lands nothing provides this context, so in a real cluster
// the panel never appears; the OS test page provides staged shares so the look
// can be judged. When the engine's share rows exist, their reader becomes the
// provider and nothing below changes.

/** Artifact id -> the names of the people it is shared with. */
export type FileShares = ReadonlyMap<string, readonly string[]>;

export const FileSharesContext = createContext<FileShares>(new Map());

export function useFileShares(): FileShares {
  return useContext(FileSharesContext);
}

export function SharingWith({ names }: { names: readonly string[] }) {
  return (
    <section className="os-files-sharing" aria-label="Sharing with">
      <h3 className="os-files-sharing-head">
        <Users size={14} aria-hidden /> Sharing with
      </h3>
      <ul className="os-files-sharing-list">
        {names.map((name) => (
          <li key={name}>
            <span className="os-files-sharing-ava" aria-hidden>{initials(name)}</span>
            {name}
          </li>
        ))}
      </ul>
    </section>
  );
}

function initials(name: string): string {
  return name.split(/\s+/).filter(Boolean).slice(0, 2).map((part) => part[0]!.toUpperCase()).join("");
}
