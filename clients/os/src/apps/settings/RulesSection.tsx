import { RecordListSkeleton } from "../../kit/RecordListSkeleton";
import { useMemo, useState } from "react";

import { Button, Caption, Chip, RecordList, RecordRow, Head, Notice, Refine, Select } from "../../kit";
import { useSession } from "../../chrome/access";
import { LEVELS } from "./routingFacts";
import { DescribeRulePanel } from "./DescribeRulePanel";
import { RuleFieldsPanel } from "./RuleFieldsPanel";
import {
  FLOOR_RULE_SENTENCE,
  LOCKED_RULE_SENTENCE,
  isFloorRule,
  ruleSentence,
  rulesInOrder,
  useRuleActions,
  useRules,
  type RuleRow,
} from "./rulesFacts";

// Settings -> Rules (epic memql#5153, D1).
//
// ===========================================================================
// PRECEDENCE IS THE SPINE, AND THE NUMBER IS DATA
// ===========================================================================
// This list is the one place in the shell where a number in the left gutter
// is not decoration. Rules are evaluated in an order, first match wins, and
// the number IS the order -- a value the person sets, reads back, and
// collides with. That is the test for whether a sequenced device is earned:
// the content is genuinely a sequence and the marker is genuinely its value.
//
// ===========================================================================
// EACH ROW IS A CLAIM, NOT A ROW OF FIELDS
// ===========================================================================
// Somebody reads this list to decide whether the set does what they meant. A
// row of seven field values makes them assemble the claim themselves, once
// per row. So each row is the rule written as one English sentence -- the
// same sentence the describe panel shows before activating, so there is one
// form to learn and the thing you approved is the thing you see afterwards.
//
// ===========================================================================
// A LOCKED RULE SAYS THE WAY PAST IT
// ===========================================================================
// Shipped rules are re-read from the embedded tree on every boot and the
// retire builtin refuses them by name. A lock glyph says "you cannot edit
// this" and none of the rest, so somebody works out the other two by meeting
// a refusal. The head says it in words instead.
//
// ===========================================================================
// AND THE FLOOR IS THE EXCEPTION THIS LIST MUST NOT DRAW WRONG
// ===========================================================================
// "Locked rules run first" is true of five of the six shipped rules and FALSE
// of the sixth. `default` is locked and states no conditions, so if it ran in
// the locked partition it would match every call before any authored rule was
// consulted -- an absorbing state that makes the whole custom tier dead,
// silently, with every rule still listed and nothing failing. The engine
// exempts it by IDENTITY (memql#5127); `rulesInOrder` recognises it the same
// way and sorts it last.
//
// Drawing it locked-first would have been a picture of the bug rather than of
// the fix, and it would have read as authoritative: somebody would have
// concluded their own rules never run and gone looking for a defect that was
// corrected before they arrived.

/**
 * Owner or developer (D7), as a set, explicitly not admin -- the same shape
 * and argument as Doors. Writing a rule is configuration; an admin's concern
 * is user administration, and the ladder puts admin below developer so a
 * minimum cannot express it.
 */
export const RULES_SECTION_RESOURCE = "app:settings/rules";

type Editing = { kind: "none" } | { kind: "describe" } | { kind: "fields"; seed: RuleRow | null };

export function RulesSection() {
  const { access } = useSession();
  const rules = useRules(true);
  const actions = useRuleActions(rules.reload);
  const [editing, setEditing] = useState<Editing>({ kind: "none" });
  const [search, setSearch] = useState("");
  const [level, setLevel] = useState("");
  const [origin, setOrigin] = useState("");
  const [confirming, setConfirming] = useState("");

  const ordered = useMemo(() => rulesInOrder(rules.rules), [rules.rules]);

  const shown = useMemo(
    () =>
      ordered.filter((rule) => {
        if (origin === "shipped" && !rule.locked) return false;
        if (origin === "mine" && rule.locked) return false;
        if (level !== "" && rule.level !== level && rule.when["level"] !== level) return false;
        const q = search.trim().toLowerCase();
        if (q === "") return true;
        return (
          rule.name.toLowerCase().includes(q) ||
          rule.policy.toLowerCase().includes(q) ||
          ruleSentence(rule).toLowerCase().includes(q)
        );
      }),
    [ordered, origin, level, search],
  );

  const chips = [
    ...(level === "" ? [] : [{ id: "level", label: `level ${level}`, onRemove: () => setLevel("") }]),
    ...(origin === ""
      ? []
      : [
          {
            id: "origin",
            label: origin === "shipped" ? "shipped" : "mine",
            onRemove: () => setOrigin(""),
          },
        ]),
  ];

  return (
    <div className="os-settings os-settings-wide">
      <Head title="Rules" meta={rules.read && !rules.loading && !rules.error && rules.supported ? shown.length : undefined}>
        {/* The one primary act. Absent where the cluster cannot take a custom
            rule at all -- offering it would produce a refusal at the last
            step of a flow somebody has already invested in. */}
        {actions.supported && editing.kind === "none" ? (
          <Button tone="primary" onClick={() => setEditing({ kind: "describe" })}>
            Describe a rule
          </Button>
        ) : null}
      </Head>

      <p className="os-caption">
        A rule looks at what a call is -- its level, its prompt, the role acting
        -- and picks where it goes. They are tried in the order below and the
        first match wins. {LOCKED_RULE_SENTENCE}
      </p>

      {rules.error ? (
        <Notice
          tone="warn"
          sentence={`The cluster declined this read for ${access?.role || "your role"}.`}
          detail={rules.error}
        />
      ) : null}

      {!rules.supported ? (
        <Caption>
          This cluster does not route by rules yet. When it does, the rules it
          ships with appear here and you can add your own above them.
        </Caption>
      ) : (
        <>
          {ordered.length === 0 ? null : (
            <div className="os-rules-scope">
              <Refine
                label="Refine rules"
                search={search}
                onSearch={setSearch}
                placeholder="Search"
                chips={chips}
              >
                <Select id="rules-facet-level" label="Level" value={level} onChange={setLevel}>
                  <option value="">Any level</option>
                  {LEVELS.map((one) => (
                    <option key={one} value={one}>
                      {one}
                    </option>
                  ))}
                </Select>
                <Select id="rules-facet-origin" label="Origin" value={origin} onChange={setOrigin}>
                  <option value="">Shipped and mine</option>
                  <option value="shipped">Shipped only</option>
                  <option value="mine">Mine only</option>
                </Select>
              </Refine>
            </div>
          )}

          {rules.loading && ordered.length === 0 ? (
            <RecordListSkeleton label="Loading the rules" />
          ) : ordered.length === 0 ? (
            <Caption>
              No rules yet. Describe one and every call that matches it goes
              where you said.
            </Caption>
          ) : shown.length === 0 ? (
            <Caption>No rule matches that.</Caption>
          ) : (
            <RecordList as="ol" label="Rules, in the order they are tried">
              {shown.map((rule, i) => (
                <RuleLine
                  key={rule.name}
                  rule={rule}
                  // THE PRECEDENCE COLUMN IS NOT MONOTONIC, AND IT IS RIGHT.
                  // Shipped rules run before custom ones whatever their
                  // number, so the true order reads 60, 40, 70, 0 -- which
                  // looks like a broken sort to anybody scanning the gutter.
                  // A hairline at each partition boundary is what makes the
                  // number legible as data rather than as a mistake.
                  firstCustom={!rule.locked && (i === 0 || shown[i - 1]!.locked)}
                  confirming={confirming === rule.name}
                  busy={actions.state.busy}
                  onEdit={() => setEditing({ kind: "fields", seed: rule })}
                  onAskRemove={() => setConfirming(rule.name)}
                  onKeep={() => setConfirming("")}
                  onRemove={() => {
                    setConfirming("");
                    void actions.retire(rule.name, rule.revision);
                  }}
                />
              ))}
            </RecordList>
          )}
        </>
      )}

      {editing.kind === "describe" ? (
        <DescribeRulePanel
          actions={actions}
          onActivated={() => {
            setEditing({ kind: "none" });
            actions.clear();
          }}
          onEditAsFields={(seed) => setEditing({ kind: "fields", seed })}
          onCancel={() => {
            setEditing({ kind: "none" });
            actions.clear();
          }}
        />
      ) : null}

      {editing.kind === "fields" ? (
        <RuleFieldsPanel
          actions={actions}
          seed={editing.seed}
          existing={ordered}
          onActivated={() => {
            setEditing({ kind: "none" });
            actions.clear();
          }}
          onCancel={() => {
            setEditing({ kind: "none" });
            actions.clear();
          }}
        />
      ) : null}

      {editing.kind === "none" && actions.state.message !== "" ? (
        <Notice
          tone={actions.state.failed ? "error" : "info"}
          sentence={actions.state.failed ? "That did not go through." : "Done."}
          detail={actions.state.message}
        />
      ) : null}
    </div>
  );
}

/**
 * One rule: its precedence, its sentence, and the acts that are legal on it.
 *
 * A SHIPPED RULE CARRIES NO ACTS AT ALL -- not disabled ones. The engine
 * refuses to retire it by name, so an Edit or a Remove here could only ever
 * produce a refusal, and rule 12 says an act that is not legal is absent.
 * What it carries instead is the word "shipped", which is the fact.
 */
function RuleLine({
  rule,
  firstCustom,
  confirming,
  busy,
  onEdit,
  onAskRemove,
  onKeep,
  onRemove,
}: {
  rule: RuleRow;
  /** First rule of the custom block -- draws the partition hairline. */
  firstCustom: boolean;
  confirming: boolean;
  busy: boolean;
  onEdit: () => void;
  onAskRemove: () => void;
  onKeep: () => void;
  onRemove: () => void;
}) {
  return (
    <div data-os-locked={rule.locked || undefined} data-os-floor={isFloorRule(rule) || undefined} data-os-first-custom={firstCustom || undefined}>
      <RecordRow name={rule.name} secondary={ruleSentence(rule)} icon={<span className="os-mono">{rule.precedence}</span>}
        stateExtra={<>{rule.locked ? <Chip tone="muted">shipped</Chip> : null}{isFloorRule(rule) ? <Chip tone="muted">the floor</Chip> : null}</>}
        actions={<>        {rule.locked ? null : confirming ? (
          <span className="os-rule-confirm" role="group" aria-label={`Remove ${rule.name}`}>
            <Button onClick={onKeep}>Keep it</Button>
            <Button tone="danger" busy={busy} busyLabel="Removing" onClick={onRemove}>
              Remove {rule.name}
            </Button>
          </span>
        ) : (
          <>
            <Button onClick={onEdit} ariaLabel={`Edit ${rule.name} as fields`}>
              Edit as fields
            </Button>
            <Button tone="danger" onClick={onAskRemove} ariaLabel={`Remove ${rule.name}`}>
              Remove
            </Button>
          </>
        )}</>}>
        {isFloorRule(rule) ? <span>{FLOOR_RULE_SENTENCE}</span> : null}
        {rule.described ? <span>You wrote: {rule.described}</span> : null}
      </RecordRow>
    </div>
  );
}
