import { useEffect, useRef, type KeyboardEvent } from "react";

import { Field, InlineSkeleton, Input, Select } from "../../../kit";

/**
 * THE BRANCH TO RUN, asked in place under the pipeline's facts while the bar
 * reads "Run a branch". A list when the repository answers with its branches,
 * the way the deploy flow's "Branch or tag" does; a text field when it answers
 * none (a pasted-token source, a probe that failed), so the act is never
 * blocked on a read. The default branch is left out: Run latest is that act,
 * one press away.
 *
 * Focus moves to the control once it is drawn, because the act that opened it
 * has left the bar. Escape takes the question back unless an open list claims
 * it first: the kit's Select closes its list and stops the key there.
 */
export function BranchChoice({
  branches,
  defaultBranch,
  loading,
  value,
  onChange,
  onRun,
  onCancel,
}: {
  branches: readonly string[];
  defaultBranch: string;
  /** The repository's branches are still being read. */
  loading: boolean;
  value: string;
  onChange: (next: string) => void;
  onRun: () => void;
  onCancel: () => void;
}) {
  const box = useRef<HTMLDivElement | null>(null);
  const others = branches.filter((name) => name !== defaultBranch);
  const listed = others.length > 0;

  useEffect(() => {
    if (loading) return;
    box.current?.querySelector<HTMLElement>("#pipeline-branch")?.focus();
  }, [loading, listed]);

  function onKeyDown(event: KeyboardEvent) {
    if (event.key !== "Escape") return;
    event.stopPropagation();
    onCancel();
  }

  return (
    <div ref={box} className="pipeline-branch-choice" onKeyDown={onKeyDown}>
      <Field label="Branch">
        {loading ? (
          <InlineSkeleton label="Reading the repository's branches" />
        ) : listed ? (
          <Select id="pipeline-branch" label="Branch to run" value={value} onChange={onChange}>
            <option value="" disabled>
              Choose a branch
            </option>
            {others.map((name) => (
              <option key={name} value={name}>
                {name}
              </option>
            ))}
          </Select>
        ) : (
          <Input
            id="pipeline-branch"
            label="Branch to run"
            value={value}
            onChange={onChange}
            placeholder="feature/login"
            code
            onEnter={() => {
              if (value.trim() !== "") onRun();
            }}
          />
        )}
      </Field>
    </div>
  );
}
