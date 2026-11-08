import { LessonUsage } from "./LessonUsage";
import { ActionBar, type Act } from "../../kit/ActionBar";
import { useEffect, useRef, useState } from "react";
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
  Refine,
  Subhead,
  TextArea,
} from "../../kit";
import { RecordListSkeleton } from "../../kit/RecordListSkeleton";
import { useOsConnection } from "../../live/connection";
import { usePagedCollection } from "../../live/useLiveCollection";
import { LoadMore } from "../../kit/LoadMore";
import { TEXT_DEBOUNCE_MS, useDebouncedValue } from "../../logs/useDebouncedValue";
import { LessonDraftForm } from "./LessonDraftForm";
import {
  lesson,
  lessonHeads,
  lessonEvents,
  guidanceReceiptLabel,
  type Lesson,
  type LessonDraft,
} from "./lessons";

export function LessonsReview() {
  const connection = useOsConnection();
  const [accepted, setAccepted] = useState({ connection, events: [] as Lesson[] });
  const [selected, select] = useState(""),
    [operationError, setError] = useState(""),
    [busy, setBusy] = useState(false),
    [draft, setDraft] = useState<LessonDraft | null>(null),
    [reason, setReason] = useState(""),
    [application, setApplication] = useState(""),
    [counterexample, setCounterexample] = useState("");
  const [search, setSearch] = useState("");
  const query = useDebouncedValue(search.trim(), TEXT_DEBOUNCE_MS);
  const feed = usePagedCollection<Lesson>(`training:lessons:${query}`, conn => ({
    concept: "v1:work:observation", actions: [],
    comparePageRows: (a, b) => Date.parse(b.at) - Date.parse(a.at) || a.id.localeCompare(b.id),
    seed: async (cursor, signal) => {
      const page = (await conn.query.libraryTrainingLessonsPage({ search: query, cursor }, { signal })).single();
      if (!Array.isArray(page?.lessons)) throw new Error("The lesson page is incomplete.");
      return { rows: page.lessons.map(lesson), nextCursor: String(page.nextCursor ?? "") };
    },
  }));
  const history = usePagedCollection<{ id: string; lesson: Lesson }>(selected ? `training:lesson-history:${selected}` : null, conn => ({
    concept: "v1:work:observation", actions: [],
    comparePageRows: (a, b) => b.lesson.sequence - a.lesson.sequence,
    seed: async (cursor, signal) => {
      const page = await conn.query.workTrainingLessonHistory({ lessonId: selected }, { cursor, signal });
      return { rows: lessonEvents(page.rows()).map(value => ({ id: value.receipt, lesson: value })), nextCursor: page.meta()?.cursor ?? "" };
    },
  }));
  const events = [...new Map([...history.snapshot.rows.map(row => row.lesson), ...(accepted.connection === connection ? accepted.events : [])].map(value => [value.receipt, value])).values()];
  const ready = feed.snapshot.state !== "seeding" && feed.snapshot.state !== "disconnected";
  const error = operationError || feed.snapshot.error;
  const reload = feed.reseed;
  const [editReceipt, setEditReceipt] = useState("");
  const request = useRef({ signature: "", id: "" });
  useEffect(() => { select(""); setDraft(null); setError(""); }, [connection]);
  const heads = lessonHeads([...(accepted.connection === connection ? accepted.events : []), ...feed.snapshot.rows]);
  const current = heads.find(l => l.id === selected);
  const visibleHeads = heads.filter(l => !query || [l.draft.title, l.draft.text, l.draft.applies, l.draft.exceptions].join("\n").toLowerCase().includes(query.toLowerCase()));
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
      setAccepted(held => ({ connection, events: [next, ...(held.connection === connection ? held.events : []).filter(l => l.id !== next.id)] }));
      feed.reseed();
      history.reseed();
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
        resume={false}
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
      {!current ? <Refine label="Search lessons" search={search} onSearch={setSearch} placeholder="Search lessons" /> : null}
      {ready && !current ? (
        visibleHeads.length ? (
          <><RecordList as="ul" label="Manual lessons">
            {visibleHeads.map((l) => (
              <RecordRow
                key={l.id}
                name={l.draft.title}
                secondary={`Version ${l.version} · Private to you`}
                state={l.enabled ? "Enabled" : l.review}
                onOpen={() => select(l.id)}
              />
            ))}
          </RecordList><LoadMore feed={feed} /></>
        ) : (
          <Caption>
            {query ? "No lessons match this search." : "No lesson candidates. Propose one from a saved guidance message in Ask."}
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
                {history.snapshot.error ? <Notice tone="error" sentence="Lesson history could not be read." detail={history.snapshot.error} /> : null}
                {history.snapshot.state === "seeding" ? <RecordListSkeleton label="Reading lesson history" /> : null}
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
                <LoadMore feed={history} />
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
