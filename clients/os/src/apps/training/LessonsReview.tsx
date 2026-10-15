import { LessonUsage } from "./LessonUsage";
import { ActionBar, type Act } from "../../kit/ActionBar";
import { useCallback, useEffect, useRef, useState } from "react";
import {
  AutoRefresh,
  Caption,
  Fact,
  Facts,
  Field,
  Head,
  Input,
  Notice,
  Panel,
  RecordList,
  RecordRow,
  Subhead,
  TextArea,
} from "../../kit";
import { RecordListSkeleton } from "../../kit/RecordListSkeleton";
import { useOsConnection } from "../../live/connection";
import { readHistory } from "./evaluations";
import { LessonDraftForm } from "./LessonDraftForm";
import {
  lesson,
  lessonEvents,
  lessonHeads,
  guidanceReceiptLabel,
  type Lesson,
  type LessonDraft,
} from "./lessons";

export function LessonsReview() {
  const connection = useOsConnection();
  const [events, setEvents] = useState<Lesson[]>([]),
    [selected, select] = useState(""),
    [ready, setReady] = useState(false),
    [error, setError] = useState(""),
    [busy, setBusy] = useState(false),
    [draft, setDraft] = useState<LessonDraft | null>(null),
    [reason, setReason] = useState(""),
    [application, setApplication] = useState(""),
    [counterexample, setCounterexample] = useState("");
  const reading = useRef(false),
    active = useRef(true);
  const [editReceipt, setEditReceipt] = useState("");
  const request = useRef({ signature: "", id: "" });
  const reload = useCallback(async () => {
    if (!connection || reading.current) return;
    reading.current = true;
    try {
      const rows = await readHistory((cursor) =>
        connection.query.workTrainingLessonsForOwner({}, { cursor }),
      );
      if (active.current) {
        setEvents(lessonEvents(rows));
        setReady(true);
        setError("");
      }
    } catch (e) {
      if (active.current) setError(e instanceof Error ? e.message : String(e));
    } finally {
      reading.current = false;
    }
  }, [connection]);
  useEffect(() => {
    active.current = true;
    void reload();
    return () => {
      active.current = false;
    };
  }, [reload]);
  const heads = lessonHeads(events),
    current = heads.find((l) => l.id === selected);
  async function act(
    action: "edit" | "review" | "reject" | "enable" | "retire",
  ) {
    if (!connection || !current || busy) return;
    const expectedReceipt = action === "edit" ? editReceipt : current.receipt;
    setBusy(true);
    setError("");
    try {
      const input = {
        action,
        lessonId: current.id,
        expectedReceipt,
        ...(action === "edit" && draft ? { draft } : {}),
        reason,
        evaluationIds: action === "enable" ? [application, counterexample] : [],
      };
      const signature = JSON.stringify(input);
      if (request.current.signature !== signature)
        request.current = { signature, id: crypto.randomUUID() };
      const next = lesson(
        (
          await connection.query.librarySaveTrainingLesson({
            ...input,
            requestId: request.current.id,
          })
        ).single(),
      );
      setEvents((held) => [
        next,
        ...held.filter((l) => l.receipt !== next.receipt),
      ]);
      setDraft(null);
      setReason("");
      if (action === "enable")
        await connection.query.libraryIndexTrainingLesson({
          lessonId: next.id,
          expectedReceipt: next.receipt,
        });
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }
  async function index() {
    if (!connection || !current || busy) return;
    setBusy(true);
    setError("");
    try {
      await connection.query.libraryIndexTrainingLesson({
        lessonId: current.id,
        expectedReceipt: current.receipt,
      });
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }
  const edit: Act = {
    label: "Edit",
    text: true,
    onAct: () => {
      if (current) {
        setDraft({ ...current.draft });
        setEditReceipt(current.receipt);
      }
    },
  };
  const decision = (
    label: string,
    action: "review" | "reject" | "enable" | "retire",
  ): Act => ({ label, busy, onAct: () => void act(action) });
  const acts: Act[] = !current
    ? []
    : draft
      ? [
          { label: "Cancel", text: true, onAct: () => setDraft(null) },
          { label: "Save revision", busy, onAct: () => void act("edit") },
        ]
      : current.review === "candidate"
        ? [
            edit,
            decision("Reject", "reject"),
            decision("Mark reviewed", "review"),
          ]
        : current.review === "reviewed"
          ? [
              edit,
              decision("Retire", "retire"),
              current.enabled
                ? { label: "Index lesson", busy, onAct: () => void index() }
                : decision("Enable for my work", "enable"),
            ]
          : [edit];
  return (
    <section
      aria-label="Lesson candidates"
      className="os-app-stack training-lesson-review"
    >
      <AutoRefresh
        onRefresh={reload}
        busy={busy}
        enabled={connection !== null && !draft}
      />
      {current ? (
        <Head
          title={current.draft.title}
          back={{
            label: "Lessons",
            onSelect: () => {
              select("");
              setDraft(null);
            },
          }}
        />
      ) : (
        <Subhead>Lessons</Subhead>
      )}
      {error ? (
        <Notice
          tone="error"
          sentence="The lesson operation could not be completed."
          detail={error}
        />
      ) : null}
      {!ready && !error ? (
        <RecordListSkeleton label="Reading lesson candidates" />
      ) : null}
      {ready && !current ? (
        heads.length ? (
          <RecordList as="ul" label="Manual lessons">
            {heads.map((l) => (
              <RecordRow
                key={l.id}
                name={l.draft.title}
                secondary={`Version ${l.version} · Private to you`}
                state={l.enabled ? "Enabled" : l.review}
                onOpen={() => select(l.id)}
              />
            ))}
          </RecordList>
        ) : (
          <Caption>
            No lesson candidates. Propose one from a saved guidance message in
            Ask.
          </Caption>
        )
      ) : null}
      {current ? (
        <>
          {draft ? (
            <LessonDraftForm value={draft} onChange={setDraft} />
          ) : (
            <>
              <Panel>
                <Subhead>Guidance</Subhead>
                <p style={{ whiteSpace: "pre-wrap" }}>{current.draft.text}</p>
                <Facts>
                  <Fact label="Applies when" value={current.draft.applies} />
                  <Fact label="Exceptions" value={current.draft.exceptions} />
                  <Fact label="Scope" value="Your work" />
                  <Fact label="Revision" value={current.digest} />
                </Facts>
              </Panel>
              <Panel>
                <Subhead>Evidence</Subhead>
                <blockquote>{current.source.quote}</blockquote>
                <Facts>
                  <Fact
                    label="Guidance receipt"
                    value={guidanceReceiptLabel(current.source.status)}
                  />
                  <Fact label="Source work" value={current.source.runId} />
                  <Fact
                    label="Observed action and outcome"
                    value={current.draft.observed}
                  />
                  <Fact label="Alternative" value={current.draft.alternative} />
                  <Fact label="Principle" value={current.draft.principle} />
                  <Fact
                    label="Observation confidence"
                    value={current.draft.factConfidence}
                  />
                  <Fact
                    label="Lesson confidence"
                    value={current.draft.lessonConfidence}
                  />
                </Facts>
              </Panel>
              <Panel>
                <Subhead>Validation</Subhead>
                <Facts>
                  <Fact
                    label="Application scenario"
                    value={current.draft.scenario}
                  />
                  <Fact
                    label="Counterexample"
                    value={current.draft.counterexample}
                  />
                  <Fact
                    label="Saved evaluations"
                    value={current.evaluationIds.join(", ") || "Not recorded"}
                  />
                </Facts>
                {current.review === "reviewed" && !current.enabled ? (
                  <>
                    <Field label="Application evaluation receipt">
                      <Input
                        id="lesson-application"
                        label="Application evaluation receipt"
                        value={application}
                        onChange={setApplication}
                      />
                    </Field>
                    <Field label="Counterexample evaluation receipt">
                      <Input
                        id="lesson-counterexample"
                        label="Counterexample evaluation receipt"
                        value={counterexample}
                        onChange={setCounterexample}
                      />
                    </Field>
                    <Caption>
                      Use results from Training Evaluations for this exact
                      revision. Enabling indexes the lesson using the configured
                      embedding route.
                    </Caption>
                  </>
                ) : null}
              </Panel>
              <LessonUsage lessonId={current.id} />
              <Panel>
                <Subhead>History</Subhead>
                <RecordList as="ul" label="Lesson history">
                  {events
                    .filter((l) => l.id === current.id)
                    .sort((a, b) => b.sequence - a.sequence)
                    .map((l) => (
                      <RecordRow
                        key={l.receipt}
                        name={`${l.action} · Version ${l.version}`}
                        secondary={l.reason || l.at}
                        state={l.review}
                      />
                    ))}
                </RecordList>
              </Panel>
              <Field label="Decision reason">
                <TextArea
                  id="lesson-reason"
                  label="Decision reason"
                  value={reason}
                  onChange={setReason}
                />
              </Field>
            </>
          )}
          {draft && editReceipt !== current.receipt ? (
            <Notice
              tone="error"
              sentence="This lesson changed while you were editing. Your draft is preserved; reopen the current revision before saving."
            />
          ) : null}
          <ActionBar
            scope={current.receipt}
            state={current.enabled ? "Enabled" : current.review}
            detail={
              current.enabled
                ? "Eligible for selection after indexing. Selection and outcomes are recorded separately."
                : "Not available to future work."
            }
            acts={acts}
          />
        </>
      ) : null}
    </section>
  );
}
