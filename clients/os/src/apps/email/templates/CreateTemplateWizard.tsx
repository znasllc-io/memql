import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { FileText } from "lucide-react";
import { refusalSentence } from "@znasllc-io/memql-sdk-core/client";

import { useSession } from "../../../chrome/access";
import { ChoiceStack, Notice, useNow } from "../../../kit";
import { ContentSkeleton } from "../../../kit/ContentSkeleton";
import type { Stop } from "../../../kit/Rail";
import { Wizard, type WizardStatus } from "../../../kit/Wizard";
import { VSCODE_NO_ANSWER_MESSAGE } from "../../../items/vscode";
import { useDefaultOrganization } from "../../accounts/organization";
import { accountName } from "../../accounts/rows";
import { useAccountOptionsFeed } from "../../accounts/tie";
import { toKitActs, type ActSpec } from "../acts";
import { organizationName, selectableOrganizations } from "../organizations";
import { templateProjection, useProjected } from "../rows";
import { listKnowledge, type EmailFeeds } from "../useEmail";
import { elapsedClock } from "../when";
import { arrivedTemplate, beginTemplateWait, openNewEmail, waitWords, type TemplateWait } from "./newTemplate";

// CREATE NEW TEMPLATE -- on the shell's one wizard (DESIGN.md, "One wizard for
// adding"; Ruling 9).
//
//   Organization (only when there is a choice) -> Write
//
// NOTHING IS CREATED HERE. A template is written in the editor: Open the editor
// opens its New email page for the organization, and the template exists once
// the person publishes the email there -- in another window, written by another
// node. The wizard then waits for it the way the OS learns of every template,
// off the live templates feed (`newTemplate.ts`): the first template in that
// organization that was not in the list when the wait began, in whatever state
// it arrives, opens its page. The editor saves a draft before it publishes, so
// that is usually the draft; its page says so.
//
// A WAIT BEGINS ONLY FROM A LIST THAT HAS FINISHED LOADING: a template that was
// always there and arrives late would look new. Until then the floor offers
// only the way out.

type Step = "organization" | "write";

export function CreateTemplateWizard({ feeds, onBack, onDone }: {
  feeds: EmailFeeds;
  /** Leave: nothing was written, so there is nothing to undo. */
  onBack: () => void;
  /** A template arrived for the organization: open its page. */
  onDone: (templateId: string) => void;
}) {
  const { config } = useSession();

  // ---- whose email ---------------------------------------------------------
  const { accounts, state: accountsState } = useAccountOptionsFeed();
  const choices = useMemo(() => selectableOrganizations(accounts), [accounts]);
  const fallback = useDefaultOrganization(accounts);
  const [picked, setPicked] = useState("");
  // What was chosen, if it can be; else the only one there is; else where this
  // person starts. With several and no start, nothing until they choose.
  const accountId = choices.some((choice) => choice.id === picked)
    ? picked
    : choices.length === 1
      ? choices[0]!.id
      : choices.some((choice) => choice.id === fallback)
        ? fallback
        : "";
  const choosing = choices.length > 1;
  const organization = organizationName(accounts, accountId);
  const [chosenStep, setChosenStep] = useState<Step | null>(null);
  const open: Step = accountId === "" || (chosenStep === "organization" && choosing) ? "organization" : "write";

  // ---- the wait (Ruling 9) -------------------------------------------------
  const templates = useProjected(feeds.templates.snapshot.rows, templateProjection);
  const settled = listKnowledge(feeds.templates) === "known";
  const [wait, setWait] = useState<TemplateWait | null>(null);
  // By the second while a wait is measured.
  const now = useNow(wait !== null ? 1_000 : 60_000);
  const [editorSilent, setEditorSilent] = useState(false);
  const stopListening = useRef<() => void>(() => {});
  useEffect(() => () => stopListening.current(), []);

  const arrival = wait === null ? null : arrivedTemplate(wait, templates);
  const arrivedId = arrival?.template.id ?? "";
  useEffect(() => {
    if (wait === null || arrivedId === "") return;
    stopWaiting();
    onDone(arrivedId);
    // Acts on the arrival only.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [wait, arrivedId]);

  function stopWaiting() {
    stopListening.current();
    stopListening.current = () => {};
    setWait(null);
  }

  function openEditor() {
    if (accountId === "" || !settled) return;
    setEditorSilent(false);
    setWait(beginTemplateWait(templates, accountId, false, Date.now()));
    // INSIDE THE CLICK: a tab opened after anything asynchronous is a blocked pop-up.
    stopListening.current = openNewEmail(config.domain, accountId, () => {
      stopListening.current = () => {};
      setWait(null);
      setEditorSilent(true);
    });
  }

  function chooseOrganization(next: string) {
    // Another organization's templates are not this one's answer.
    if (next !== accountId && wait !== null) stopWaiting();
    setPicked(next);
    setEditorSilent(false);
    setChosenStep("write");
  }

  // ---- the floor -----------------------------------------------------------
  function floor(): { status: WizardStatus; specs: ActSpec[] } {
    // Somebody else's turn: the editor has the email.
    if (wait !== null) {
      const words = waitWords(wait, arrivedTemplate({ ...wait, requirePublished: false }, templates));
      return { status: { ...words, tone: "busy", meta: elapsedClock(now.getTime() - wait.startedAt) }, specs: [{ id: "stop-waiting", label: "Stop waiting", text: true }] };
    }
    const cancel: ActSpec = { id: "cancel", label: "Cancel", text: true };
    if (open === "organization") return { status: { word: "Choose an organization" }, specs: [cancel] };
    if (listKnowledge(feeds.templates) === "refused") {
      return { status: { word: "Ready to write" }, specs: [cancel, { id: "retry", label: "Try again", tone: "primary" }] };
    }
    return { status: { word: "Ready to write" }, specs: settled ? [cancel, { id: "open-editor", label: "Open the editor", tone: "primary" }] : [cancel] };
  }

  function run(id: string) {
    switch (id) {
      case "cancel":
        onBack();
        return;
      case "open-editor":
        openEditor();
        return;
      case "stop-waiting":
        stopWaiting();
        return;
      case "retry":
        feeds.templates.reseed();
        return;
    }
  }

  // ---- the steps -----------------------------------------------------------
  function organizationBody(): ReactNode {
    if (choices.length === 0) {
      return accountsState === "seeding" ? <ContentSkeleton kind="form" label="Loading organizations" /> : <p>No organization is available to you.</p>;
    }
    return (
      <ChoiceStack
        name="email-template-organization"
        label="Organization"
        voice="prose"
        value={accountId}
        options={choices.map((account) => ({ value: account.id, label: accountName(account) }))}
        onChange={chooseOrganization}
      />
    );
  }

  const writeBody = (
    <>
      {editorSilent ? <Notice tone="warn" sentence={VSCODE_NO_ANSWER_MESSAGE} /> : null}
      {listKnowledge(feeds.templates) === "refused" ? (
        <Notice tone="error" sentence="This cluster did not return your templates." detail={refusalSentence(feeds.templates.snapshot.error, "templates")} />
      ) : null}
      <p>{`The editor opens a new email for ${organization}. When you publish it there, it appears here.`}</p>
    </>
  );

  const steps: Stop[] = [];
  if (choosing || accountId === "") {
    steps.push({
      id: "organization",
      name: "Organization",
      state: open === "organization" ? "open" : "done",
      sentence: "Whose email this is.",
      answer: accountId === "" ? undefined : organization,
      body: organizationBody(),
    });
  }
  steps.push({
    id: "write",
    name: "Write",
    state: open === "write" ? "open" : accountId === "" ? "ahead" : "waiting",
    body: accountId === "" ? undefined : writeBody,
  });

  const { status, specs } = floor();
  return (
    <Wizard
      icon={<FileText size={20} aria-hidden />}
      title="Create new template"
      breadcrumbs={[{ label: "Templates", onSelect: onBack }, { label: "Create new template" }]}
      back={{ label: "Templates", onSelect: onBack }}
      label="Creating a template"
      steps={steps}
      open={open}
      onOpen={(next) => setChosenStep(next as Step)}
      status={status}
      acts={toKitActs(specs, run)}
      context={{ page: "Create new template", step: open }}
    />
  );
}
