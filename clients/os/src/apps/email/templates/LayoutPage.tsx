import { useRef, useState, type ReactNode } from "react";
import { refusalSentence } from "@znasllc-io/memql-sdk-core/client";

import { useSession } from "../../../chrome/access";
import { Button, Fact, Facts, Head, Notice, Panel } from "../../../kit";
import { ActionBar } from "../../../kit/ActionBar";
import { deployedByLabel, usePeopleNames } from "../../deployables/people";
import { useAccountOptions } from "../../accounts/tie";
import { useLayoutLifecycle } from "../actions";
import { toKitActs, type ActSpec } from "../acts";
import { useLanding } from "../campaigns/landing";
import { organizationName } from "../organizations";
import { layoutIsArchived, layoutTitle, templateName, type LayoutRow, type TemplateRow } from "../rows";
import { useArtifactTitle, useEmailRender, useLayoutRecord, type EmailLayouts } from "../useEmail";
import { browserZone, momentWords } from "../when";
import { WORDS } from "../words";
import { layoutActs } from "./layoutActs";
import { Preview } from "./Preview";

// ONE LAYOUT'S PAGE (Ruling 24): what it is, where it came from, and the email
// it makes, rendered by the engine's one renderer -- the same `emailRender` a
// template's page uses, so a layout previews as an email made from it would.
//
// A BUILT-IN HAS NO LIFECYCLE: it ships with the renderer, so its page has no
// bar at all. An organization's layout has one, with Archive or Restore, held
// to the exact revision `emailLayoutById` answers. Nothing here saves a layout:
// that is the editor's "Save as layout".
//
// WHO AND WHERE FROM, NEVER AN ID. "Saved from" is the template published from
// the same Library file, else the file's own title, else nothing; "Saved by" is
// a person the roster names, "You" for the person looking, else nothing.

type Lifecycle = "archive" | "restore";

const REFUSED: Record<Lifecycle, string> = {
  archive: "The layout wasn't archived.",
  restore: "The layout wasn't restored.",
};

const LOST: Record<Lifecycle, string> = {
  archive: "Couldn't confirm that it was archived.",
  restore: "Couldn't confirm that it was restored.",
};

export function LayoutPage({ layout, layouts, templates, onBack }: {
  layout: LayoutRow;
  layouts: EmailLayouts;
  /** The templates in view: a layout saved from a file a template was published from is named by that template. */
  templates: readonly TemplateRow[];
  /** Back to the layouts. */
  onBack: () => void;
}) {
  const { access } = useSession();
  const accounts = useAccountOptions();
  const zone = browserZone();
  const builtIn = layout.kind === "builtin";
  const name = layoutTitle(layout);

  // ---- what it is and where it came from -----------------------------------
  // An organization's layout is read whole: who saved it, and its document when
  // the listing had to leave it out. A built-in is not a row, and has neither.
  const record = useLayoutRecord(builtIn ? "" : (layout.layoutId ?? ""), layout.revision ?? "");
  const source = layout.sourceArtifactId ?? "";
  const fromTemplate = source === "" ? undefined : templates.find((t) => t.accountId === layout.accountId && t.sourceArtifactId === source);
  const fileTitle = useArtifactTitle(builtIn || source === "" || fromTemplate !== undefined ? "" : source);
  const savedFrom = fromTemplate !== undefined ? templateName(fromTemplate) : fileTitle.value;
  const nameOf = usePeopleNames();
  // "you" in a sentence; a fact starts with it. A person's own name is as they spell it.
  const savedByWho = deployedByLabel(record.value?.ownerUserId ?? "", access?.userId ?? "", nameOf);
  const savedBy = savedByWho === "you" ? "You" : savedByWho;

  // ---- the email it makes ---------------------------------------------------
  // A built-in is previewed for the first organization in view; its document is
  // the same for all of them.
  const document = layout.documentOmitted ? (record.value?.document ?? null) : layout.document;
  const render = useEmailRender(document, builtIn ? (layouts.accountIds[0] ?? "") : layout.accountId);

  // ---- the bar ---------------------------------------------------------------
  const lifecycle = useLayoutLifecycle();
  const [busyId, setBusyId] = useState("");
  const working = useRef(false);
  // A change on its way holds the bar until the next reading says it happened:
  // the readings are not live, so the page reads them again as soon as it is taken.
  const flight = useLanding<Lifecycle>({ marker: () => layout.status, reread: () => void layouts.reload() });
  const landing = flight.landing;
  const [outcome, setOutcome] = useState<{ act: Lifecycle; lost: boolean; from: string } | null>(null);
  const missed = outcome !== null && outcome.from === layout.status ? outcome : null;
  const specs = landing !== null ? [landing.spec] : layoutActs(layout);

  async function change(spec: ActSpec, act: Lifecycle) {
    if (working.current) return;
    working.current = true;
    setBusyId(spec.id);
    setOutcome(null);
    const from = layout.status;
    flight.begin(act, spec);
    try {
      const done = act === "archive" ? await lifecycle.archive(layout) : await lifecycle.restore(layout);
      if (done) {
        flight.taken();
        void layouts.reload();
        return;
      }
      flight.drop();
      const lost = lifecycle.lost();
      setOutcome({ act, lost, from });
      if (lost) void layouts.reload();
    } finally {
      working.current = false;
      setBusyId("");
    }
  }

  function run(id: string) {
    const spec = specs.find((candidate) => candidate.id === id) ?? { id, label: id };
    if (id === "archive") void change(spec, "archive");
    else if (id === "restore") void change(spec, "restore");
  }

  function outcomeNotice(): ReactNode {
    if (missed === null) return null;
    if (missed.lost) return <Notice tone="warn" sentence={LOST[missed.act]} next="Its state will update here." detail={lifecycle.error} />;
    return <Notice tone="error" sentence={REFUSED[missed.act]} next="Nothing changed." detail={lifecycle.error} />;
  }

  /**
   * The email it makes. A read or a render that failed says so in the engine's
   * words: in the frame's place when there is nothing to show, and above the
   * last email shown when there is.
   */
  function preview(): ReactNode {
    // A built-in no reading carried the document of: none can be read by id.
    if (layout.documentOmitted && builtIn) return <Notice tone="info" sentence="This layout can't be previewed right now." />;
    const failure =
      layout.documentOmitted && record.state === "error" ? (
        <Notice tone="error" sentence="This cluster did not return the layout." detail={refusalSentence(record.error, "emailLayoutById")}>
          <Button onClick={() => void record.reload()}>Try again</Button>
        </Notice>
      ) : render.state === "error" ? (
        <Notice tone="error" sentence="This cluster did not return the preview." detail={refusalSentence(render.error, "emailRender")}>
          <Button onClick={() => void render.reload()}>Try again</Button>
        </Notice>
      ) : null;
    const shown = render.value;
    return (
      <>
        {shown === null ? null : failure}
        <Preview html={shown?.html ?? ""} text={shown?.text ?? ""} loading={shown === null && failure === null} failed={shown === null ? failure : null} />
      </>
    );
  }

  const savedAt = layout.revision ?? "";
  return (
    <div className="os-action-pane" data-os-page-context={JSON.stringify({ page: "Layout", name })}>
      <div className="os-action-body os-app-stack">
        <Head title={name} back={{ label: "Templates", onSelect: onBack }} />
        {outcomeNotice()}
        <Panel label="Layout details">
          <Facts>
            <Fact label="Organization" value={builtIn ? WORDS.builtIn : organizationName(accounts, layout.accountId)} />
            <Fact label="Description" value={layout.description} />
            {builtIn || savedFrom === "" ? null : <Fact label="Saved from" value={savedFrom} />}
            {builtIn || savedBy === "" ? null : <Fact label="Saved by" value={savedBy} />}
            {builtIn || savedAt === "" ? null : <Fact label="Saved" value={momentWords(savedAt, zone)} />}
          </Facts>
        </Panel>
        {preview()}
      </div>

      {builtIn ? null : (
        <ActionBar
          state={landing !== null ? WORDS.changing[landing.act] : layoutIsArchived(layout) ? WORDS.archived : WORDS.active}
          tone={landing !== null ? "busy" : layoutIsArchived(layout) ? "none" : "live"}
          live
          acts={toKitActs(specs, run, landing !== null ? landing.spec.id : busyId)}
        />
      )}
    </div>
  );
}
