import { useState } from "react";

import { Button, Caption, Notice, Panel, Subhead } from "../../kit";
import { AccountLabelPicker } from "../accounts/AccountPicker";
import { useAccountOptions } from "../accounts/tie";
import type { CreateGoalState } from "./actions";

// THE ONE INPUT IN THE PRODUCT: a person says what they want done.
//
// ===========================================================================
// A SENTENCE, SO A BOX RATHER THAN A FIELD
// ===========================================================================
// `statement` is "the goal in the person's own words", which is prose and
// routinely two lines. A single-line input would truncate the thing the whole
// system is about into a slot the width of a name field, and somebody would
// write less than they meant to.
//
// `.os-work-statement` is LOCAL rather than promoted to the kit, and the count
// is the reason. There are two multi-line boxes in this shell: the campaigns
// template editor's, which is MONO because it holds markup somebody will paste
// elsewhere, and this one, which is PROSE because it holds a sentence. The
// diagnostics report's is a read-only dump rather than a form field, so it is
// not a third. The kit promotes on a second use of shared BEHAVIOUR -- "the
// shared behaviour is what had to move" -- and there is none here: the border
// and the radius are already tokens, and the only thing a shared component
// would parameterise is the one thing the two boxes disagree about. The day a
// third FIELD wants one, it promotes with a `voice` prop, exactly as
// `ChoiceStack` did.

export function NewGoal({
  create,
  onCreated,
  onCancel,
}: {
  create: CreateGoalState;
  /** Called with the new goal's id, so the list can select what was just made. */
  onCreated: (goalId: string) => void;
  onCancel: () => void;
}) {
  const [statement, setStatement] = useState("");
  const [accountIds, setAccountIds] = useState<string[]>([]);
  const accounts = useAccountOptions();

  async function submit() {
    const id = await create.create({ statement, accountIds });
    if (id === "") return;
    setStatement("");
    setAccountIds([]);
    onCreated(id);
  }

  return (
    <Panel label="A new goal">
      <Subhead>What do you want done?</Subhead>
      <label className="os-sr-only" htmlFor="work-new-goal">
        The goal, in your own words
      </label>
      <textarea
        id="work-new-goal"
        className="os-work-statement"
        rows={3}
        value={statement}
        placeholder="Reconcile last month's ledger against the bank export and tell me what does not match"
        onChange={(e) => setStatement(e.target.value)}
      />

      {/* WHAT HAPPENS NEXT, SAID BEFORE IT HAPPENS. Accepting a goal opens a
          run and starts working it out, which is a thing that can cost money.
          A form whose button said only "Create" would leave somebody to find
          that out from the bill. */}
      {/* THE TAG IS A RECORD, NOT A SCOPE, and the label has to say so. An
          account tag on a goal is who the work is FOR; it narrows no read and
          grants no access, and somebody who read it as permissions would tag
          defensively and wrongly. AccountLabelPicker rather than the single
          picker because `accountIds` is a list on the concept -- the same
          control the Library index uses for the same shape of field. */}
      {accounts.length === 0 ? null : (
        <AccountLabelPicker
          selected={accountIds}
          onChange={setAccountIds}
          accounts={accounts}
          label="Who this work is for (optional)"
        />
      )}

      <Caption>
        It works this out once -- matching what it already knows how to do first, and reaching a
        model only for the parts it has not seen before -- then records the steps so the next run
        of the same thing replays them. It runs under this deployment's default limits; a goal's
        ceilings are set when it is accepted and there is no verb to change them afterwards.
      </Caption>

      {create.error === "" ? null : (
        <Notice
          tone="error"
          sentence="The goal was not accepted."
          next="Nothing was written, so nothing is half-started. You can edit what you wrote and try again."
          detail={create.error}
        />
      )}

      <div className="os-work-form-acts">
        <Button onClick={onCancel}>Cancel</Button>
        <Button tone="primary" busy={create.busy} busyLabel="Starting" onClick={() => void submit()}>
          Start work
        </Button>
      </div>
    </Panel>
  );
}
