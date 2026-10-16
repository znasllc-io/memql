import { useId } from "react";
import { Field, Input, Panel, Select, Subhead, TextArea } from "../../kit";
import { catalog } from "./evaluations";
import type { LessonDraft } from "./lessons";
export function LessonDraftForm({
  value,
  onChange,
}: {
  value: LessonDraft;
  onChange: (v: LessonDraft) => void;
}) {
  const id = useId();
  const field = (key: keyof LessonDraft, label: string, multiline = true) => (
    <Field key={key} label={label}>
      {multiline ? (
        <TextArea
          id={`${id}-${key}`}
          label={label}
          value={value[key]}
          onChange={(v) => onChange({ ...value, [key]: v })}
        />
      ) : (
        <Input
          id={`${id}-${key}`}
          label={label}
          value={value[key]}
          onChange={(v) => onChange({ ...value, [key]: v })}
        />
      )}
    </Field>
  );
  const scenario = (key: "scenario" | "counterexample", label: string) => (
    <Field label={label}>
      <Select
        id={`${id}-${key}`}
        label={label}
        value={value[key]}
        onChange={(v) => onChange({ ...value, [key]: v })}
      >
        <option value="">Choose a scenario</option>
        {catalog.scenarios.map((s) => (
          <option key={s.id} value={s.id}>
            {s.title}
          </option>
        ))}
      </Select>
    </Field>
  );
  return (
    <>
      <Panel>
        <Subhead>Lesson for your work</Subhead>
        {field("title", "Title", false)}
        {field("text", "Proposed guidance")}
        {field("applies", "When it applies")}
        {field("exceptions", "Exceptions")}
      </Panel>
      <Panel>
        <Subhead>Evidence</Subhead>
        {field("observed", "Observed action and outcome")}
        {field(
          "alternative",
          "Effective alternative, or why the action was reasonable",
        )}
        {field("principle", "Principle")}
        {field("factConfidence", "Confidence in the observation", false)}
        {field("lessonConfidence", "Confidence in the lesson", false)}
      </Panel>
      <Panel>
        <Subhead>Validation</Subhead>
        {scenario("scenario", "Application scenario")}
        {scenario("counterexample", "Counterexample scenario")}
      </Panel>
    </>
  );
}
