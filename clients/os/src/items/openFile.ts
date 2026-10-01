import { editorArtifactURL, isZipArtifact, readEditorPreference } from "./editorPreference";
import { openHandoff, type HandoffPorts } from "./vscode";

export interface FileOpenMetadata { name: string; mimeType: string; fileId: string; sizeBytes: number }

/** Reserve a tab during the click, then check current metadata before opening
 * an editor. A renamed ZIP still downloads, and a lookup failure never guesses. */
export async function openCheckedArtifact(input: {
  domain: string; artifactId: string; title: string; ports: HandoffPorts;
  load: () => Promise<FileOpenMetadata>;
  download: (file: FileOpenMetadata) => Promise<void>;
  onNoAnswer: () => void;
}): Promise<() => void> {
  const preference = readEditorPreference();
  const tab = preference === "browser" && !isZipArtifact({ title: input.title }) ? input.ports.reserve?.() : undefined;
  try {
    const file = await input.load();
    if (isZipArtifact(file)) {
      tab?.close();
      await input.download(file);
      return () => {};
    }
    const url = editorArtifactURL(input.domain, input.artifactId, file.name, preference);
    if (tab) { tab.navigate(url); return () => {}; }
    return openHandoff(url, input.onNoAnswer, input.ports);
  } catch (err) { tab?.close(); throw err; }
}
