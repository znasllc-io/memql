import { act, render } from "@testing-library/react";

import { OsProvider } from "../../src/chrome/state";
import { MachinesProvider } from "../../src/live/machines";
import { OS_REGISTRY } from "../../src/apps/registry";
import { BinApp } from "../../src/apps/bin/BinApp";
import type { FilesSettings } from "../../src/apps/files/settings";
import { memSettingsStore, withSession } from "../files/harness";

// The Bin's harness. It reuses the Files one wholesale -- the same
// `executeNamed` funnel, the same fixtures, the same providers -- because the
// two apps read the same concepts and a second fake would be free to disagree
// with the first about what a Library row looks like.

export async function renderBin(
  opts: { section?: string; settings?: Partial<FilesSettings> } = {},
) {
  const view = render(
    withSession(
      <OsProvider registry={OS_REGISTRY} actorRole="owner" grid={{ cols: 8, rows: 5 }}>
        <MachinesProvider>
          <BinApp
            sectionId={opts.section ?? "items"}
            navigate={() => {}}
            askContext={() => {}}
            store={memSettingsStore(opts.settings ?? {})}
          />
        </MachinesProvider>
      </OsProvider>,
    ),
  );
  await act(async () => {});
  return view;
}
