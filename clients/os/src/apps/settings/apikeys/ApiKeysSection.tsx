import { useEffect, useMemo, useRef, useState } from "react";
import { KeyRound } from "lucide-react";

import { useSession } from "../../../chrome/access";
import { AutoRefresh, ChoiceRow, EmptyState, Head, HeadNote, InlineSkeleton, Notice, RecordList, RecordListSkeleton, RecordRow, Refine } from "../../../kit";
import { AddButton } from "../../../kit/AddButton";
import { PageHelp } from "../../../kit/pageHelp";
import { holds } from "../../../system/roles";
import { matchesQuery } from "../../fleet/machines/sharing";
import { AddKeyWizard } from "./AddKeyWizard";
import { KeyDetail } from "./KeyDetail";
import { useKeyActivity, useVendorKeys } from "./useVendorKeys";
import { costWords, drawsStates, stateWords, vendorName, type KeyCard } from "./words";

// Settings > API keys (design record 2026-11-21-vendor-accounts-and-api-keys,
// U1 as amended 2026-11-22: the owner moved a person's keys out of Fleet, which
// opens for owners and developers only, to a place everybody can open).
//
// A person's own keys and the keys other people lent them, as one flat list of
// records that open (DESIGN.md, "Record lists"). The Head carries the title and
// the one Add control. A key's page and the add wizard each take the list's
// place, under the trail back to it: a list and its detail never share a
// scroll column (rule 11).
//
// WHO SEES WHAT IS THE CLUSTER'S ANSWER, NOT THIS FILE'S. The list is what
// `vendorKeysForActor` returns for the caller: their own keys, whatever their
// role, and the keys lent to them. What this file decides is only which
// controls to draw: the Add control for somebody who holds the part, and for a
// cluster owner the question "every key on this cluster", asked behind Refine
// and sent only when it is asked.
//
// THE SECTION HAS NO FLOOR OF ITS OWN (apps/registry.tsx). A key's owner must
// always be able to reach it to change or remove it, so the page opens for
// everybody who opens Settings and only ADDING is a part an operator can
// withhold.

/** The part that adds a key. Every predefined role holds it (K4, amended). */
export const API_KEYS_ADD_PART = "app:settings/apikeys";

type View = { kind: "list" } | { kind: "add" } | { kind: "key"; id: string };

export function ApiKeysSection({ active = true }: { active?: boolean }) {
  const keys = useVendorKeys();
  const { access } = useSession();
  // Asking for every key is a cluster owner's question. The cluster is the
  // authority either way; this only keeps the control off pages it would
  // answer nothing on.
  const clusterOwner = access?.role === "owner";
  const mayAdd = holds("execute", API_KEYS_ADD_PART);
  const [view, setView] = useState<View>({ kind: "list" });
  const [search, setSearch] = useState("");
  // Moves each time the section is shown again, so what a row read once is
  // read again for that visit and never on a timer.
  const [visit, setVisit] = useState(0);

  // SHOWN AGAIN, READ AGAIN. The pane stays mounted while another section is
  // open, and a key can change with no act here.
  const shownBefore = useRef(active);
  useEffect(() => {
    if (active && !shownBefore.current) {
      keys.reload();
      setVisit((n) => n + 1);
    }
    shownBefore.current = active;
  }, [active, keys.reload]);

  const toList = () => setView({ kind: "list" });

  // Every key the reader can open, once each: their own, then what is lent to
  // them, then (for a cluster owner who asked) everybody else's.
  //
  // THE FULLER CARD WINS. A key lent to a cluster owner arrives twice once
  // they ask for every key: as a borrower's short card, and as the full card
  // a cluster owner is given of any key. The short one has no Check now and
  // no Remove, and a cluster owner may check and remove every key on their
  // cluster, this one included. It keeps its place in the list.
  const everything = useMemo(() => {
    const at = new Map<string, number>();
    const out: KeyCard[] = [];
    for (const card of [...keys.mine, ...keys.lent, ...keys.all]) {
      const seen = at.get(card.id);
      if (seen === undefined) {
        at.set(card.id, out.length);
        out.push(card);
      } else if (card.full && !out[seen]!.full) {
        out[seen] = card;
      }
    }
    return out;
  }, [keys.mine, keys.lent, keys.all]);

  const opened = view.kind === "key" ? everything.find((card) => card.id === view.id) : undefined;
  // A key that is no longer in the list (removed here or elsewhere) has no
  // page: back to the list rather than a page about nothing.
  useEffect(() => {
    if (view.kind === "key" && keys.state === "ready" && opened === undefined) setView({ kind: "list" });
  }, [view, keys.state, opened]);

  if (view.kind === "add") {
    return <AddKeyWizard keys={keys} taken={keys.mine.map((card) => card.label)} onCancel={toList} onAdded={toList} />;
  }
  if (view.kind === "key" && opened !== undefined) {
    return <KeyDetail key={opened.id} card={opened} keys={keys} siblings={keys.mine} onBack={toList} onRemoved={toList} />;
  }

  const shown = everything.filter((card) => matchesQuery(search, card.label, card.ownerName, vendorName(card.vendor)));
  const states = drawsStates(shown);
  const perVendor = new Map<string, number>();
  for (const card of keys.mine) perVendor.set(card.vendor, (perVendor.get(card.vendor) ?? 0) + 1);
  const narrowed = keys.wantAll || search.trim() !== "";

  return (
    <div className="os-apikeys" data-os-page-context={JSON.stringify({ page: "API keys", search: search || undefined })}>
      <Head
        title="API keys"
        meta={keys.state === "ready" && narrowed ? `${shown.length} shown` : undefined}
        note={keys.behind !== "" ? <HeadNote sentence="API key updates are interrupted." next="Showing the last known state." detail={keys.behind} /> : null}
      >
        {clusterOwner ? (
          <Refine
            iconOnly
            label="Find API keys"
            search={search}
            onSearch={setSearch}
            chips={keys.wantAll ? [{ id: "all", label: "Every key on this cluster", onRemove: () => keys.setWantAll(false) }] : []}
          >
            <ChoiceRow
              label="Whose keys"
              value={keys.wantAll ? "all" : "mine"}
              onChange={(next) => keys.setWantAll(next === "all")}
              options={[
                { value: "mine", label: "Mine and lent to me" },
                { value: "all", label: "Every key on this cluster" },
              ]}
            />
          </Refine>
        ) : null}
        {mayAdd ? <AddButton label="Add an API key" onClick={() => setView({ kind: "add" })} /> : null}
      </Head>
      <PageHelp title="API keys">
        <p>An API key pays for Anthropic or OpenAI models with an account of your own. It is sealed in this cluster when you add it, and nothing shows it again.</p>
        <p>A route you choose in Ask spends your default key at the model's vendor. Auto spends a key only after you turn on "Let Auto use this key" on that key. You can lend a key to named people and groups, who then pay for their calls with it.</p>
        <p>The cluster never spends a key for its own work. The cluster's own vendor account is set up under Vendors.</p>
        <p>Every call a key pays for checks it, and a key nothing used for a day is checked by listing the vendor's models, which sends no prompt and costs nothing.</p>
      </PageHelp>
      <AutoRefresh enabled={active} busy={keys.busy !== ""} onRefresh={keys.reload} />

      {keys.state === "loading" ? (
        <RecordListSkeleton label="Loading API keys" rows={3} />
      ) : keys.state === "failed" ? (
        <Notice tone="error" sentence="Your API keys could not be read." next="Nothing was changed. It is tried again on its own." detail={keys.error} />
      ) : shown.length === 0 && narrowed ? (
        <EmptyState icon={KeyRound} title="No matching keys">Try another name, owner or vendor.</EmptyState>
      ) : shown.length === 0 ? (
        <EmptyState icon={KeyRound} title="No API keys">
          {mayAdd ? "Add a key from Anthropic or OpenAI to pay for their models with your own account." : "Keys that other people lend you appear here."}
        </EmptyState>
      ) : (
        <RecordList as="ul" label="API keys">
          {shown.map((card) => (
            <KeyRow key={card.id} card={card} drawState={states} severalAtVendor={(perVendor.get(card.vendor) ?? 0) > 1} visit={visit} onOpen={() => setView({ kind: "key", id: card.id })} />
          ))}
        </RecordList>
      )}
    </div>
  );
}

// ONE KEY, AS A ROW. The name stands over where it is from (its vendor and the
// last characters that tell two keys apart, or who lent it), the quiet middle
// holds its few facts, and the state is one word with a dot. The row opens the
// key's page; it holds no control of its own.
function KeyRow({ card, drawState, severalAtVendor, visit, onOpen }: { card: KeyCard; drawState: boolean; severalAtVendor: boolean; visit: number; onOpen: () => void }) {
  const vendor = vendorName(card.vendor);
  const words = stateWords(card);
  const mine = card.relation === "mine";
  const secondary = card.full ? [vendor, card.fingerprint].filter(Boolean).join(" · ") : `${vendor} · lent by ${card.ownerName || "its owner"}`;
  return (
    <RecordRow
      icon={<KeyRound size={18} aria-hidden />}
      name={card.label}
      secondary={secondary}
      state={drawState ? words.word : undefined}
      tone={words.tone}
      stateTitle={card.detail || undefined}
      current={card.state === "good" && !card.paused}
      dim={card.paused}
      label={`Open ${card.label}, ${vendor}${drawState ? `, ${words.word.toLowerCase()}` : ""}`}
      onOpen={onOpen}
    >
      {/* Somebody else's key, read by a cluster owner: whose it is. */}
      {!mine && card.full && card.ownerName !== "" ? <span>{card.ownerName}</span> : null}
      {/* The only key at its vendor is the default by construction: not said. */}
      {mine && card.isDefault && severalAtVendor ? <span>Default</span> : null}
      {card.full && card.lentTo > 0 ? <span>Lent to {card.lentTo}</span> : null}
      {mine ? <KeyCost keyId={card.id} visit={visit} /> : null}
    </RecordRow>
  );
}

/** What one of the reader's own keys cost this month, read once per visit. */
function KeyCost({ keyId, visit }: { keyId: string; visit: number }) {
  const reading = useKeyActivity(keyId, true, visit);
  if (reading.state === "loading") return <InlineSkeleton label="Loading this month's cost" />;
  if (reading.state === "failed" || reading.activity === null) return <span title={reading.error || undefined}>Cost not reported</span>;
  return <span>{costWords(reading.activity)}</span>;
}
