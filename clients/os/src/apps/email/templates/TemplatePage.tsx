import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { refusalSentence } from "@znasllc-io/memql-sdk-core/client";

import { useSession } from "../../../chrome/access";
import { Button, Fact, Facts, Head, Notice, Panel } from "../../../kit";
import { ActionBar, type ActionBarTone } from "../../../kit/ActionBar";
import { InlineSkeleton } from "../../../kit/ContentSkeleton";
import { openHandoff, VSCODE_NO_ANSWER_MESSAGE } from "../../../items/vscode";
import { useAccountOptions } from "../../accounts/tie";
import { useTemplateLifecycle } from "../actions";
import { toKitActs, type ActSpec } from "../acts";
import { useLanding } from "../campaigns/landing";
import { organizationName } from "../organizations";
import { isPublished, layoutName, templateIsArchived, templateName, type TemplateRow } from "../rows";
import { useEmailRender, useTemplateRecord, type EmailLayouts } from "../useEmail";
import { browserZone, momentWords } from "../when";
import { WORDS, templateState } from "../words";
import { Preview } from "./Preview";
import { lifecycleContent, templateActs, templateEditorURL } from "./templateActs";

// ONE TEMPLATE'S PAGE (memql#6033), which replaces the list it was opened from
// (DESIGN.md rule 11): what the template is, the email as it is sent, and ONE
// bar that carries everything that changes it (rule 12).
//
// THE EMAIL IS THE ENGINE'S RENDER. The list row is a summary, so the page
// reads the template whole by id (`templateById`) and hands its document --
// for a template saved before documents existed, the custom email its bodies
// make -- to `emailRender`, the renderer a send uses: the preview is the email
// that goes out, with the engine's list of what keeps it from going.
//
// WRITING IT IS THE EDITOR'S. Open in editor goes to the Library file the
// template was published from, or the template itself; the page archives and
// restores and nothing else. An archived template stays on its page with
// Restore right there, whatever the list is showing.

type Lifecycle = "archive" | "restore";

const REFUSED: Record<Lifecycle, string> = {
  archive: "The template wasn't archived.",
  restore: "The template wasn't restored.",
};

const LOST: Record<Lifecycle, string> = {
  archive: "Couldn't confirm that it was archived.",
  restore: "Couldn't confirm that it was restored.",
};

export function TemplatePage({ template, layouts, onBack, onOpenLayout, onReread }: {
  template: TemplateRow;
  /** The organizations' layout readings, which name the layout it was made from. */
  layouts: EmailLayouts;
  /** Back to the templates. */
  onBack: () => void;
  /** Open the page of the layout it was made from. */
  onOpenLayout: (layoutId: string) => void;
  /** Read the templates again: a change that was taken but has not arrived. */
  onReread: () => void;
}) {
  const { config } = useSession();
  const accounts = useAccountOptions();
  const zone = browserZone();

  // ---- the email, as it is sent -------------------------------------------
  const record = useTemplateRecord(template.id, template.createdAt);
  const content = useMemo(() => (record.value === null ? null : lifecycleContent(record.value)), [record.value]);
  const render = useEmailRender(content, template.accountId);

  // ---- the bar -------------------------------------------------------------
  const lifecycle = useTemplateLifecycle();
  const [asking, setAsking] = useState<"" | "archive">("");
  const [busyId, setBusyId] = useState("");
  const working = useRef(false);
  // A question about an act that stopped being legal (it was archived somewhere
  // else, or went back to draft) is dropped, not left to come back.
  const askingNow = asking === "archive" && isPublished(template) ? "archive" : "";
  useEffect(() => {
    if (asking !== "" && askingNow === "") setAsking("");
  }, [asking, askingNow]);

  // A CHANGE ON ITS WAY HOLDS THE BAR until the template's row says it happened.
  const flight = useLanding<Lifecycle>({ marker: () => template.status, reread: onReread });
  const landing = flight.landing;

  /** The last change that did not go as asked, while the state it was pressed in still stands. */
  const [outcome, setOutcome] = useState<{ act: Lifecycle; lost: boolean; from: string } | null>(null);
  const missed = outcome !== null && outcome.from === template.status ? outcome : null;

  // The editor's handoff, and whether it answered.
  const [editorSilent, setEditorSilent] = useState(false);
  const stopListening = useRef<() => void>(() => {});
  useEffect(() => () => stopListening.current(), []);

  const specs = landing !== null ? [landing.spec] : templateActs(template, askingNow);

  async function change(spec: ActSpec, act: Lifecycle) {
    // One change at a time: a second press while the first is on its way is the same press.
    if (working.current) return;
    working.current = true;
    setBusyId(spec.id);
    setAsking("");
    setOutcome(null);
    const from = template.status;
    flight.begin(act, spec);
    try {
      const done = act === "archive" ? await lifecycle.archive(template) : await lifecycle.restore(template);
      if (done) {
        flight.taken();
        return;
      }
      flight.drop();
      const lost = lifecycle.lost();
      setOutcome({ act, lost, from });
      if (lost) onReread();
    } finally {
      working.current = false;
      setBusyId("");
    }
  }

  function run(id: string) {
    const spec = specs.find((candidate) => candidate.id === id) ?? { id, label: id };
    switch (id) {
      case "archive":
        setOutcome(null);
        setAsking("archive");
        return;
      case "keep":
        setAsking("");
        return;
      case "archive-now":
      case "archive-confirmed":
        void change(spec, "archive");
        return;
      case "restore":
        void change(spec, "restore");
        return;
      case "open-editor":
        setEditorSilent(false);
        stopListening.current();
        // Inside the click: a tab opened after anything asynchronous is a blocked pop-up.
        stopListening.current = openHandoff(templateEditorURL(config.domain, template), () => setEditorSilent(true));
        return;
    }
  }

  const name = templateName(template);
  const archived = templateIsArchived(template);
  const tone: ActionBarTone = landing !== null ? "busy" : isPublished(template) ? "live" : "none";
  const word = landing !== null ? WORDS.changing[landing.act] : templateState(template.status);
  const question = askingNow === "archive" && landing === null ? `Archive “${name}”? Campaigns and automations that use it can't send it until it's restored.` : null;

  // ---- what the page says --------------------------------------------------
  const layoutFact: ReactNode =
    template.layoutId === "" ? (
      WORDS.customLayout
    ) : layoutName(layouts.readings, template.layoutId) !== "" ? (
      <button type="button" className="os-link" onClick={() => onOpenLayout(template.layoutId)}>
        {layoutName(layouts.readings, template.layoutId)}
      </button>
    ) : layouts.settled ? (
      ""
    ) : (
      <InlineSkeleton label="Loading the layout" />
    );

  function outcomeNotice(): ReactNode {
    if (missed === null) return null;
    if (missed.lost) return <Notice tone="warn" sentence={LOST[missed.act]} next="Its state will update here." detail={lifecycle.error} />;
    return <Notice tone="error" sentence={REFUSED[missed.act]} next="Nothing changed." detail={lifecycle.error} />;
  }

  /**
   * The email, and what is wrong with it. A read or a render that failed says so
   * in the engine's words: in the frame's place when there is nothing to show,
   * and above the last email shown when there is.
   */
  function preview(): ReactNode {
    const failure =
      record.state === "error" ? (
        <Notice tone="error" sentence="This cluster did not return the template." detail={refusalSentence(record.error, "templateById")}>
          <Button onClick={() => void record.reload()}>Try again</Button>
        </Notice>
      ) : record.state === "ready" && record.value === null ? (
        <Notice tone="error" sentence={WORDS.unavailable.template} />
      ) : render.state === "error" ? (
        <Notice tone="error" sentence="This cluster did not return the preview." detail={refusalSentence(render.error, "emailRender")}>
          <Button onClick={() => void render.reload()}>Try again</Button>
        </Notice>
      ) : null;
    const shown = render.value;
    const issues = shown?.issues ?? [];
    return (
      <>
        {shown === null ? null : failure}
        {issues.length === 0 ? null : (
          <Notice tone="warn" sentence="Fix these before publishing:">
            <ul className="email-issues">
              {issues.map((issue, at) => (
                <li key={`${issue.code}:${issue.block}:${at}`}>{issue.message}</li>
              ))}
            </ul>
          </Notice>
        )}
        <Preview html={shown?.html ?? ""} text={shown?.text ?? ""} loading={shown === null && failure === null} failed={shown === null ? failure : null} />
      </>
    );
  }

  return (
    <div className="os-action-pane" data-os-page-context={JSON.stringify({ page: "Template", name, id: template.id })}>
      <div className="os-action-body os-app-stack">
        <Head navigationKey={`template:${template.id}`} title={name} back={{ label: "Templates", onSelect: onBack }} />
        {editorSilent ? <Notice tone="warn" sentence={VSCODE_NO_ANSWER_MESSAGE} /> : null}
        {outcomeNotice()}
        <Panel label="Template details">
          <Facts>
            <Fact label="Organization" value={organizationName(accounts, template.accountId)} />
            <Fact label="Layout" value={layoutFact} />
            <Fact label="Subject" value={template.subject} />
            {template.preheader === "" ? null : <Fact label="Preheader" value={template.preheader} />}
            <Fact label={WORDS.changed} value={template.createdAt === "" ? "" : momentWords(template.createdAt, zone)} />
          </Facts>
        </Panel>
        {preview()}
      </div>

      <ActionBar state={word} tone={archived && landing === null ? "none" : tone} live acts={toKitActs(specs, run, landing !== null ? landing.spec.id : busyId)}>
        {question === null ? null : <span className="os-actbar-confirm">{question}</span>}
      </ActionBar>
    </div>
  );
}
