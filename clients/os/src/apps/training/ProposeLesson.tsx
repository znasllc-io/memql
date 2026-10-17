import { ActionBar } from "../../kit/ActionBar";
import { useRef, useState } from "react";
import { Button, Caption, Notice } from "../../kit";
import { useOsConnection } from "../../live/connection";
import { LessonDraftForm } from "./LessonDraftForm";
import { emptyLesson, lesson } from "./lessons";
export function ProposeLesson({
  conversationId,
  turnId,
  text,
}: {
  conversationId: string;
  turnId: string;
  text: string;
}) {
  const connection = useOsConnection();
  const [open, setOpen] = useState(false),
    [draft, setDraft] = useState(() => emptyLesson(text)),
    [busy, setBusy] = useState(false),
    [error, setError] = useState(""),
    [saved, setSaved] = useState(false);
  const request = useRef({ signature: "", id: "" });
  async function save() {
    if (!connection || busy) return;
    const signature = JSON.stringify(draft);
    if (request.current.signature !== signature)
      request.current = { signature, id: crypto.randomUUID() };
    setBusy(true);
    setError("");
    try {
      lesson(
        (
          await connection.query.librarySaveTrainingLesson({
            action: "propose",
            requestId: request.current.id,
            conversationId,
            turnId,
            draft,
          })
        ).single(),
      );
      setSaved(true);
      setOpen(false);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }
  if (saved) return <Caption>Candidate saved. Review it in Training.</Caption>;
  return (
    <div className="os-app-stack">
      {!open ? (
        <Button
          tone="quiet"
          onClick={() => setOpen(true)}
          disabled={!connection}
        >
          Propose lesson
        </Button>
      ) : (
        <>
          <LessonDraftForm value={draft} onChange={setDraft} />
          {error ? (
            <Notice
              tone="error"
              sentence="The candidate could not be saved."
              detail={error}
            />
          ) : null}
          <ActionBar
            state="Candidate"
            detail="Private to you. Saving does not enable future use."
            acts={[
              { label: "Cancel", text: true, onAct: () => setOpen(false) },
              { label: "Save candidate", busy, onAct: () => void save() },
            ]}
          />
        </>
      )}
    </div>
  );
}
