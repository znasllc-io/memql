import { useMemo, useState } from "react";

import { Input, MultiPicker, RoleSelect } from "../../kit";
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

/** Search and select groups. Archived groups remain removable when selected. */
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
  const offered = groups.filter(group => group.status === "active" || selected.includes(group.id));
  return <MultiPicker label={label} single={single} selected={selected} onChange={onChange}
    choices={offered.map(group => ({ id: group.id, label: group.name, description: accountNameOf?.(group.accountId) || undefined }))} />;
}

/** Shared role selector with the server assignment policy reflected in its options. */
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
  void actorRole;
  return <RoleSelect label={label} value={value} onChange={onChange} disabled={busy}
    options={rungs.map(rung => ({ value: rung.slug, label: rung.name, disabled: rungRefusal(rung, context) !== "", reason: rungSentence(rungRefusal(rung, context)) }))} />;
}
