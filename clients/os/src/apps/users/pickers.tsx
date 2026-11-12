import { useMemo, useState } from "react";

import { Chip, Input, RankMark } from "../../kit";
import { rungRefusal, rungSentence, type AssignContext } from "./assign";
import { personName, type GroupRow, type PersonRow, type RoleRow } from "./rows";

// The three pickers this app builds and exports (design record, D10).
//
// They live HERE, in the app that owns the concepts, on the `AccountPicker`
// precedent: the kit is the shell's shared vocabulary -- rows, chips, notices,
// the live list -- and a people picker is not vocabulary, it is one domain's
// surface. The kit gains nothing domain-shaped.
//
// All three are PRESENTATION over rows the caller already holds. None of them
// reads, and none of them decides what anybody may do: the engine refuses the
// write it would refuse whatever these render. What they decide is what is
// OFFERED, which is a courtesy to the person reading and never the boundary.

/**
 * Somebody to add: a search over the roster this window already has, minus a
 * given set.
 *
 * A SEARCH RATHER THAN A SELECT, because the roster is the one list in this
 * app with no upper bound -- a cluster with two hundred people would put two
 * hundred options in a dropdown, and the person adding somebody knows the
 * name they are looking for.
 *
 * The exclusion is passed in rather than computed here: the group page
 * excludes its own members, the invite rail excludes nobody, and a picker that
 * decided for itself would need to know which surface it was on.
 */
export function PeoplePicker({
  people,
  exclude = [],
  onPick,
  label,
  placeholder = "Search",
  busyId = "",
}: {
  people: readonly PersonRow[];
  /** User ids not to offer -- the members a group already has. */
  exclude?: readonly string[];
  onPick: (userId: string) => void;
  label: string;
  placeholder?: string;
  /** The id currently being written, so its row can say so. */
  busyId?: string;
}) {
  const [search, setSearch] = useState("");
  const excluded = useMemo(() => new Set(exclude), [exclude]);

  const matches = useMemo(() => {
    const needle = search.trim().toLowerCase();
    const offered = people.filter((p) => !excluded.has(p.id) && p.active);
    if (needle === "") return offered.slice(0, 8);
    return offered
      .filter(
        (p) =>
          personName(p).toLowerCase().includes(needle) ||
          p.primaryEmail.toLowerCase().includes(needle),
      )
      .slice(0, 8);
  }, [people, excluded, search]);

  return (
    <div className="os-people-picker">
      <Input
        id={`people-picker-${label.replace(/\W+/g, "-").toLowerCase()}`}
        label={label}
        value={search}
        onChange={setSearch}
        placeholder={placeholder}
      />
      {matches.length === 0 ? (
        <p className="os-caption">
          {people.length === 0
            ? "Nobody to add yet."
            : "Nobody here by that name. Everybody else is already a member."}
        </p>
      ) : (
        <ul className="os-people-picker-list" aria-label={label}>
          {matches.map((person) => (
            <li key={person.id}>
              <button
                type="button"
                className="os-people-picker-row"
                onClick={() => onPick(person.id)}
                disabled={busyId === person.id}
              >
                <span className="os-people-picker-name">{personName(person)}</span>
                <span className="os-caption os-mono">{person.primaryEmail}</span>
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/**
 * One or several groups, as toggles.
 *
 * TOGGLES RATHER THAN A `<select multiple>`: a native multi-select drops every selection the moment somebody
 * clicks without a modifier, on a control whose job is "one or two groups".
 *
 * Archived groups are offered only when already chosen -- a person being
 * invited should not be offered a group that grants nothing, and a selection
 * already made must stay removable.
 */
export function GroupPicker({
  groups,
  selected,
  onChange,
  label,
  accountNameOf,
  single = false,
}: {
  groups: readonly GroupRow[];
  selected: readonly string[];
  onChange: (next: string[]) => void;
  label: string;
  /** How to name the client a group grants. "" renders no chip. */
  accountNameOf?: (accountId: string) => string;
  /** One at a time: picking replaces rather than adds. */
  single?: boolean;
}) {
  const offered = useMemo(() => {
    const held = new Set(selected);
    return groups.filter((g) => g.status === "active" || held.has(g.id));
  }, [groups, selected]);

  function toggle(groupId: string) {
    if (single) {
      onChange(selected.includes(groupId) ? [] : [groupId]);
      return;
    }
    onChange(
      selected.includes(groupId)
        ? selected.filter((id) => id !== groupId)
        : [...selected, groupId],
    );
  }

  if (offered.length === 0) {
    return <p className="os-caption">No groups yet. The Groups section is where they are made.</p>;
  }

  return (
    <div className="os-group-picker" role="group" aria-label={label}>
      {offered.map((group) => {
        const on = selected.includes(group.id);
        // The client's name only when it SAYS something the group's name does
        // not: an account-kind group is named after its client, so rendering
        // both reads "Acme Acme".
        const named = accountNameOf?.(group.accountId) ?? "";
        const account = named === group.name ? "" : named;
        return (
          <button
            key={group.id}
            type="button"
            className="os-account-label"
            data-on={on || undefined}
            aria-pressed={on}
            onClick={() => toggle(group.id)}
          >
            {group.name}
            {account === "" ? null : <span className="os-caption"> {account}</span>}
            {group.status === "archived" ? <span className="os-caption"> archived</span> : null}
          </button>
        );
      })}
    </div>
  );
}

/**
 * The cluster's ladder, with the offered rule applied.
 *
 * EVERY RUNG IS DRAWN. An unoffered one is dashed and carries its own reason as
 * a title, rather than being dropped: a ladder that showed a different number
 * of rungs on two people's pages would leave the reader with nothing to explain
 * the difference. `assign.ts` states the rule and mirrors
 * `auth.MayAssignRole`; the server is the authority either way.
 */
export function RoleLadderPicker({
  rungs,
  value,
  onChange,
  context,
  actorRole,
  label,
  busy = false,
}: {
  /** The ladder, strongest first. */
  rungs: readonly RoleRow[];
  /** The rung held today, marked. */
  value: string;
  onChange: (slug: string) => void;
  context: AssignContext;
  actorRole: string;
  label: string;
  busy?: boolean;
}) {
  return (
    // `data-picker` is what turns an unoffered rung dashed: the same markup
    // draws a RECORD on the role page, where nothing is being offered and
    // every rung would otherwise read as refused.
    <ul className="os-role-ladder" data-picker="" aria-label={label}>
      {rungs.map((rung) => {
        const refusal = rungRefusal(rung, context);
        const offered = refusal === "";
        const current = rung.slug === value || rung.aliases.includes(value);
        return (
          <li key={rung.slug} className="os-role-rung" data-offered={offered ? "" : undefined} data-current={current ? "" : undefined}>
            <button
              type="button"
              className="os-role-rung-line"
              aria-pressed={current}
              disabled={!offered || busy || current}
              title={offered ? undefined : rungSentence(refusal)}
              onClick={() => onChange(rung.slug)}
            >
              <RankMark actorRole={actorRole} ownerRole={rung.slug} />
              <span className="os-role-rung-name">{rung.name}</span>
              <span className="os-role-slug">{rung.slug}</span>
              {rung.accountId === "" ? null : <Chip title="Scoped to one client">scoped</Chip>}
              {current ? <span className="os-role-rung-held">held</span> : null}
            </button>
          </li>
        );
      })}
    </ul>
  );
}
