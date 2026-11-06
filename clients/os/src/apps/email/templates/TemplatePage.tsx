import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { refusalSentence } from "@znasllc-io/memql-sdk-core/client";

import { useSession } from "../../../chrome/access";
import { Button, Fact, Facts, Head, Notice, Panel } from "../../../kit";
import { ActionBar, type ActionBarTone } from "../../../kit/ActionBar";
import { InlineSkeleton } from "../../../kit/ContentSkeleton";
import { openHandoff, VSCODE_NO_ANSWER_MESSAGE } from "../../../items/vscode";
import { useAccountOptions } from "../../accounts/tie";
import { useCancelComposition, useRetryComposition, useTemplateLifecycle } from "../actions";
import { toKitActs, type ActSpec } from "../acts";
import { useLanding } from "../campaigns/landing";
import { organizationName } from "../organizations";
import { compositionFailed, isPublished, isWriting, layoutName, templateIsArchived, templateName, type TemplateRow } from "../rows";
import { useEmailRender, useTemplateRecord, type EmailLayouts } from "../useEmail";
import { browserZone, elapsedWords, momentWords } from "../when";
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

type Lifecycle = "archive" | "restore" | "cancel" | "retry";

const REFUSED: Record<Lifecycle, string> = {
  archive: "The template wasn't archived.",
  restore: "The template wasn't restored.",
  cancel: "The writing wasn't stopped.",
  retry: "The email wasn't started again.",
};

const LOST: Record<Lifecycle, string> = {
  archive: "Couldn't confirm that it was archived.",
  restore: "Couldn't confirm that it was restored.",
  cancel: "Couldn't confirm that it was stopped.",
  retry: "Couldn't confirm that it was started again.",
};

/** The active automations that send a template: series hold their emails while it is a draft; event automations send none. */
export interface SendingAutomations {
  /** Series (journeys): a step waits until the template is published. */
  held: readonly string[];
  /** Event automations (rules): an event while it is a draft sends nothing, and is not sent later. */
  unsent: readonly string[];
}

const NO_AUTOMATIONS: SendingAutomations = { held: [], unsent: [] };

/** “A”, “A” and “B”, “A”, “B” and “C”: automations by name, with the noun they take. */
function automationsNamed(names: readonly string[]): string {
  const quoted = names.map((name) => `“${name}”`);
  const list = quoted.length < 3 ? quoted.join(" and ") : `${quoted.slice(0, -1).join(", ")} and ${quoted[quoted.length - 1]}`;
  return `${quoted.length === 1 ? "the automation" : "the automations"} ${list}`;
}

/** The warning for editing a template that active automations send: saving makes it a draft, and what each kind does then. */
export function holdWarning({ held, unsent }: SendingAutomations): string {
  const first = held.length === 0 ? "Saving changes makes this template a draft again." : `Saving changes makes this template a draft again, and ${automationsNamed(held)} ${held.length === 1 ? "holds its emails" : "hold their emails"} until the template is published.`;
  if (unsent.length === 0) return first;
  return `${first} While it is a draft, ${automationsNamed(unsent)} ${unsent.length === 1 ? "sends none of its emails" : "send none of their emails"}, and they are not sent later.`;
}

export function TemplatePage({ template, layouts, automations = NO_AUTOMATIONS, onBack, onReread }: {
  template: TemplateRow;
  /** The active automations that send this template, by kind. */
  automations?: SendingAutomations;
  /** The organizations' layout readings, which name the layout it was made from. */
  layouts: EmailLayouts;
  /** Back to the templates. */
  onBack: () => void;
  /** Read the templates again: a change that was taken but has not arrived. */
  onReread: () => void;
}) {
  const { config, access } = useSession();
  const viewer = access?.userId ?? "";
  const accounts = useAccountOptions();
  const zone = browserZone();

  // ---- the email, as it is sent -------------------------------------------
  const record = useTemplateRecord(template.id, template.createdAt);
  const content = useMemo(() => (record.value === null ? null : lifecycleContent(record.value)), [record.value]);
  const render = useEmailRender(content, template.accountId);

  // ---- the bar -------------------------------------------------------------
  const lifecycle = useTemplateLifecycle();
  const cancelling = useCancelComposition();
  const retrying = useRetryComposition();
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
  // (An email being written moves its composition, not its status.)
  const marker = `${template.status}|${template.composition?.state ?? ""}|${template.createdAt}`;
  const flight = useLanding<Lifecycle>({ marker: () => marker, reread: onReread });
  const landing = flight.landing;

  /** The last change that did not go as asked, while the state it was pressed in still stands. */
  const [outcome, setOutcome] = useState<{ act: Lifecycle; lost: boolean; from: string } | null>(null);
  const missed = outcome !== null && outcome.from === marker ? outcome : null;

  // The editor's handoff, and whether it answered.
  const [editorSilent, setEditorSilent] = useState(false);
  const stopListening = useRef<() => void>(() => {});
  useEffect(() => () => stopListening.current(), []);

  const specs = landing !== null ? [landing.spec] : templateActs(template, askingNow, viewer);

  async function change(spec: ActSpec, act: Lifecycle) {
    // One change at a time: a second press while the first is on its way is the same press.
    if (working.current) return;
    working.current = true;
    setBusyId(spec.id);
    setAsking("");
    setOutcome(null);
    const from = marker;
    flight.begin(act, spec);
    try {
      const done =
        act === "archive" ? await lifecycle.archive(template)
        : act === "restore" ? await lifecycle.restore(template)
        : act === "cancel" ? await cancelling.run(template.id)
        : await retrying.run(template.id);
      if (done) {
        flight.taken();
        return;
      }
      flight.drop();
      const doing = act === "cancel" ? cancelling : act === "retry" ? retrying : lifecycle;
      const lost = doing.lost();
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
      case "cancel-composition":
        void change(spec, "cancel");
        return;
      case "retry-composition":
        void change(spec, "retry");
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
  const writing = isWriting(template);
  const failed = compositionFailed(template);
  const tone: ActionBarTone = landing !== null || writing ? "busy" : failed ? "paused" : isPublished(template) ? "live" : "none";
  const word =
    landing !== null ? WORDS.changing[landing.act]
    : writing ? WORDS.composition.writingBar
    : failed ? WORDS.composition.failed
    : templateState(template.status);
  // What went wrong is the server's own plain sentence, shown as it is.
  const detail = landing === null && failed ? template.composition?.error : undefined;
  // How long MemQL has been writing, in the editor's words ("2 min"), looked
  // at again every few seconds while it does.
  const startedAt = template.composition?.startedAt ?? "";
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!writing) return;
    setNow(Date.now());
    const timer = setInterval(() => setNow(Date.now()), 5000);
    return () => clearInterval(timer);
  }, [writing]);
  const started = Date.parse(startedAt);
  const meta = writing && landing === null && !Number.isNaN(started) ? elapsedWords(now - started) : undefined;
  const notes = template.composition?.notes ?? [];
  const question = askingNow === "archive" && landing === null ? `Archive “${name}”? Campaigns and automations that use it can't send it until it's restored.` : null;

  // ---- what the page says --------------------------------------------------
  const layoutFact: ReactNode =
    template.layoutId === "" ? (
      WORDS.customLayout
    ) : layoutName(layouts.readings, template.layoutId) !== "" ? (
      layoutName(layouts.readings, template.layoutId)
    ) : layouts.settled ? (
      ""
    ) : (
      <InlineSkeleton label="Loading the layout" />
    );

  function outcomeNotice(): ReactNode {
    if (missed === null) return null;
    const said = missed.act === "cancel" ? cancelling.error : missed.act === "retry" ? retrying.error : lifecycle.error;
    if (missed.lost) return <Notice tone="warn" sentence={LOST[missed.act]} next="Its state will update here." detail={said} />;
    return <Notice tone="error" sentence={REFUSED[missed.act]} next="Nothing changed." detail={said} />;
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
    <div className="os-action-pane" data-os-dividers="none" data-os-page-context={JSON.stringify({ page: "Template", name, id: template.id })}>
      <div className="os-action-body os-app-stack">
        <Head navigationKey={`template:${template.id}`} title={name} back={{ label: "Templates", onSelect: onBack }} />
        {automations.held.length + automations.unsent.length > 0 && !archived ? <Notice tone="warn" sentence={holdWarning(automations)} /> : null}
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
        {notes.length === 0 ? null : <Notice tone="info" sentence={notes.join(" ")} />}
        {preview()}
      </div>

      <ActionBar state={word} detail={detail} meta={meta} tone={archived && landing === null ? "none" : tone} live acts={toKitActs(specs, run, landing !== null ? landing.spec.id : busyId)}>
        {question === null ? null : <span className="os-actbar-confirm">{question}</span>}
      </ActionBar>
    </div>
  );
}
