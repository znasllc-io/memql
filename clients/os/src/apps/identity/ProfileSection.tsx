import { Trash2 } from "lucide-react";
import { useEffect, useId, useMemo, useRef, useState } from "react";

import { useSession } from "../../chrome/access";
import {
  Button,
  Caption,
  ContentSkeleton,
  Details,
  DetailsSection,
  Fact,
  Facts,
  Head,
  Notice,
  Select,
  SettingRow,
  Switch,
  useAppReach,
} from "../../kit";
import { IconButton } from "../../kit/IconButton";
import { InPlaceAsk } from "../../kit/pressHold";
import { useOsConnection } from "../../live/connection";
import { AvatarError, avatarFromFile } from "../../modules/profile/avatarImage";
import { profileInitial, profileName, useMyProfile, type MyProfile } from "../../modules/profile/myProfile";
import { formatStorageBytes } from "../files/backups/transfers";
import {
  composedName,
  profileWrites,
  storageShare,
  useMyBudget,
  useMyPasskeys,
  useMyStorage,
  type ProfileWrites,
} from "./profileData";
import "./profile.css";

// YOUR PROFILE (memql#6220): who you are to this cluster, what you have, and
// the few things about your account that are yours to change.
//
// The layout is Fleet's machine Settings page (DESIGN.md, "Details and
// settings content"): open named sections, one label column, edits in place,
// quiet copying. Everything here acts on the signed-in person's own row --
// the writes are self-only mutations the engine stamps from actor.userId, so
// this page carries no authority of its own.

export function ProfileSection() {
  const { profile, state, error, reread } = useMyProfile();
  const connection = useOsConnection();
  const writes = useMemo(() => profileWrites(connection), [connection]);

  return (
    <div className="os-profile">
      <Head title="Profile" />
      {profile === null ? (
        state === "failed" ? (
          <Notice tone="error" sentence="Your profile could not be read." detail={error}>
            <Button onClick={reread}>Try again</Button>
          </Notice>
        ) : (
          <ContentSkeleton kind="form" label="Reading your profile" />
        )
      ) : (
        <Loaded profile={profile} writes={writes} />
      )}
    </div>
  );
}

function Loaded({ profile, writes }: { profile: MyProfile; writes: ProfileWrites }) {
  const { access } = useSession();
  const name = profileName(profile);
  const role = access?.roleName || profile.role || access?.role || "";
  return (
    <>
      <header className="os-profile-header">
        <ProfilePhoto profile={profile} size="large" />
        <div className="os-profile-identity">
          <p className="os-profile-name">{name}</p>
          <p className="os-profile-meta">{[profile.email, role].filter(Boolean).join(" · ")}</p>
        </div>
      </header>
      <Details>
        <DetailsSection title="General">
          <SettingRow label="Name">
            <NameField profile={profile} rename={writes.rename} />
          </SettingRow>
          <SettingRow label="Photo">
            <PhotoField profile={profile} writes={writes} />
          </SettingRow>
          <Facts>
            <Fact label="Email" value={profile.email} copyValue={profile.email} />
            <Fact label="Role" value={role} />
            {profile.phone ? <Fact label="Phone" value={profile.phone} copyValue={profile.phone} /> : null}
            {profile.title ? <Fact label="Title" value={profile.title} /> : null}
          </Facts>
        </DetailsSection>
        <DetailsSection title="Storage">
          <StorageFacts />
        </DetailsSection>
        <BudgetSection />
        <DetailsSection title="Sign-in and security">
          <SecurityFacts profile={profile} />
        </DetailsSection>
        <DetailsSection title="Preferences">
          <Preferences profile={profile} writes={writes} />
        </DetailsSection>
      </Details>
    </>
  );
}

/** The photo, or the initial on the same circle. Decorative: the name is
 *  always beside it. */
export function ProfilePhoto({ profile, size }: { profile: MyProfile; size: "large" | "small" }) {
  const className = `os-profile-photo os-profile-photo-${size}`;
  if (profile.avatar) return <img className={className} src={profile.avatar} alt="" aria-hidden draggable={false} />;
  return <span className={className} aria-hidden>{profileInitial(profileName(profile))}</span>;
}

// ---------------------------------------------------------------------------
// General
// ---------------------------------------------------------------------------

function NameField({ profile, rename }: { profile: MyProfile; rename: ProfileWrites["rename"] }) {
  const ids = useId();
  // A row written before the name was split has only a display name: it is
  // offered as the first name rather than as an empty field.
  const seedFirst = profile.firstName || (profile.lastName ? "" : profile.displayName);
  const [first, setFirst] = useState(seedFirst);
  const [last, setLast] = useState(profile.lastName);
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState("");

  // THE ROW WINS, the way Fleet's rename does: a name saved in another window
  // has to reach this field. Keyed on the VALUES, so nothing else on the row
  // stamps over a name being typed.
  useEffect(() => {
    setFirst(seedFirst);
    setLast(profile.lastName);
  }, [seedFirst, profile.lastName]);

  const changed = composedName(first, last) !== profileName(profile) || first.trim() !== profile.firstName || last.trim() !== profile.lastName;
  const valid = first.trim() !== "";

  return (
    <form
      className="os-profile-name-form"
      onSubmit={(event) => {
        event.preventDefault();
        if (!changed || !valid || busy) return;
        setBusy(true);
        setRefusal("");
        void rename(first, last).then((why) => {
          setBusy(false);
          setRefusal(why);
        });
      }}
    >
      <label className="os-sr-only" htmlFor={`${ids}-first`}>First name</label>
      <input id={`${ids}-first`} className="os-input" value={first} disabled={busy} placeholder="First name" autoComplete="given-name" onChange={(e) => setFirst(e.target.value)} />
      <label className="os-sr-only" htmlFor={`${ids}-last`}>Last name</label>
      <input id={`${ids}-last`} className="os-input" value={last} disabled={busy} placeholder="Last name" autoComplete="family-name" onChange={(e) => setLast(e.target.value)} />
      {/* ABSENT UNTIL IT MEANS SOMETHING (interface rule 12): a Save over a
          name nobody changed reads as a pending edit that is not there. */}
      {(changed && valid) || busy ? (
        <Button type="submit" tone="primary" ariaLabel="Save name" busy={busy} busyLabel="Saving...">
          Save
        </Button>
      ) : null}
      {refusal ? <p className="os-profile-refusal" role="alert">The name was not changed: {refusal}</p> : null}
    </form>
  );
}

function PhotoField({ profile, writes }: { profile: MyProfile; writes: ProfileWrites }) {
  const file = useRef<HTMLInputElement>(null);
  const [busy, setBusy] = useState(false);
  const [asking, setAsking] = useState(false);
  const [refusal, setRefusal] = useState("");

  async function choose(picked: File | undefined) {
    if (!picked) return;
    setBusy(true);
    setRefusal("");
    try {
      const value = await avatarFromFile(picked);
      setRefusal(await writes.setAvatar(value));
    } catch (err) {
      setRefusal(err instanceof AvatarError ? err.message : `The photo was not changed: ${err instanceof Error ? err.message : String(err)}`);
    } finally {
      setBusy(false);
      if (file.current) file.current.value = "";
    }
  }

  async function remove() {
    setBusy(true);
    setRefusal("");
    const why = await writes.clearAvatar();
    setBusy(false);
    setAsking(false);
    if (why) setRefusal(`The photo was not removed: ${why}`);
  }

  return (
    <div className="os-profile-photo-field">
      <div className="os-profile-photo-row">
        <ProfilePhoto profile={profile} size="small" />
        <Button busy={busy && !asking} busyLabel="Saving..." disabled={busy} onClick={() => file.current?.click()}>
          {profile.avatar ? "Change photo" : "Upload photo"}
        </Button>
        {profile.avatar && !asking ? (
          <IconButton label="Remove photo" className="os-profile-photo-remove" disabled={busy} onClick={() => setAsking(true)}>
            <Trash2 size={17} aria-hidden />
          </IconButton>
        ) : null}
        <input
          ref={file}
          className="os-sr-only"
          type="file"
          accept="image/png,image/jpeg,image/webp"
          tabIndex={-1}
          aria-label="Choose a photo"
          onChange={(event) => void choose(event.target.files?.[0])}
        />
      </div>
      {asking ? (
        <InPlaceAsk className="os-profile-ask">
          <p className="os-profile-ask-line">Remove your photo? Your initial is shown instead.</p>
          <div className="os-panel-actions">
            <Button disabled={busy} onClick={() => setAsking(false)}>Keep it</Button>
            <Button tone="danger" busy={busy} busyLabel="Removing..." onClick={() => void remove()}>Remove photo</Button>
          </div>
        </InPlaceAsk>
      ) : null}
      {refusal ? <p className="os-profile-refusal" role="alert">{refusal}</p> : null}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Storage
// ---------------------------------------------------------------------------

function StorageFacts() {
  const read = useMyStorage();
  if (read.state === "reading") return <Caption>Checking storage…</Caption>;
  if (read.state === "unavailable") return <Caption>Storage is not set up on this cluster.</Caption>;
  if (read.state === "failed") return <Notice tone="error" sentence="Storage could not be checked." detail={read.error} />;
  const { usage } = read;
  const share = storageShare(usage);
  const used = formatStorageBytes(usage.used);
  const reserved = usage.reserved > 0 ? `, ${formatStorageBytes(usage.reserved)} reserved for uploads` : "";
  return (
    <>
      <Facts>
        <Fact label="Allocated" value={usage.exempt ? "No personal limit" : formatStorageBytes(usage.limit)} />
        <Fact label="Used" value={`${used}${reserved}`} />
        <Fact label="Left" value={formatStorageBytes(Math.max(0, usage.available))} />
      </Facts>
      {share !== null ? (
        <div
          className="os-profile-meter"
          role="meter"
          aria-label="Storage used"
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={Math.round(share * 100)}
          data-full={share >= 0.9 || undefined}
        >
          <span style={{ width: `${share * 100}%` }} />
        </div>
      ) : null}
    </>
  );
}

// ---------------------------------------------------------------------------
// AI budget -- present only on a cluster that serves it
// ---------------------------------------------------------------------------

function BudgetSection() {
  const budget = useMyBudget();
  if (budget.state === "absent" || budget.state === "reading") return null;
  return (
    <DetailsSection title="AI budget">
      {budget.state === "failed" ? (
        <Notice tone="error" sentence="Your budget could not be read." detail={budget.error} />
      ) : (
        <BudgetFields limit={budget.budget.monthlyLimitUSD} spent={budget.budget.spentThisMonthUSD} resetsAt={budget.budget.resetsAt} save={budget.save} />
      )}
    </DetailsSection>
  );
}

const USD = new Intl.NumberFormat(undefined, { style: "currency", currency: "USD" });

/** The day the budget turns over. Periods are UTC months, so the date is read
 *  in UTC: in the local zone "Nov 1, 00:00 UTC" is the evening of Oct 31 for
 *  half the world, which is true and misleading. */
export function resetDate(resetsAt: string): string {
  const at = new Date(resetsAt);
  if (resetsAt === "" || Number.isNaN(at.getTime())) return "";
  return at.toLocaleDateString(undefined, { month: "short", day: "numeric", timeZone: "UTC" });
}

/** Dollars typed by a person: digits with at most two decimals, at least 1. */
export function parseBudget(text: string): number | null {
  const trimmed = text.trim().replace(/^\$/, "").replace(/,/g, "");
  if (!/^\d+(\.\d{1,2})?$/.test(trimmed)) return null;
  const value = Number(trimmed);
  return value >= 1 && value <= 1_000_000 ? value : null;
}

function BudgetFields({ limit, spent, resetsAt, save }: { limit: number | null; spent: number; resetsAt: string; save: (limit: number | null) => Promise<string> }) {
  const ids = useId();
  const stored = limit === null ? "" : String(limit);
  const [draft, setDraft] = useState(stored);
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState("");
  useEffect(() => setDraft(stored), [stored]);

  const parsed = parseBudget(draft);
  const cleared = draft.trim() === "";
  const changed = draft.trim() !== stored;
  // ONE FIELD FOR BOTH ACTS: an amount sets the budget, and an emptied field
  // removes it -- "No budget" is what the empty field already says.
  const legal = changed && (cleared || parsed !== null);

  async function commit(next: number | null) {
    setBusy(true);
    setRefusal("");
    const why = await save(next);
    setBusy(false);
    if (why) setRefusal(why);
  }

  return (
    <>
      <SettingRow label="Monthly budget">
        <form
          className="os-profile-budget-form"
          onSubmit={(event) => {
            event.preventDefault();
            if (legal && !busy) void commit(cleared ? null : parsed);
          }}
        >
          <label className="os-sr-only" htmlFor={`${ids}-budget`}>Monthly budget in US dollars</label>
          <span className="os-profile-currency" aria-hidden>$</span>
          <input id={`${ids}-budget`} className="os-input" inputMode="decimal" value={draft} disabled={busy} placeholder="No budget" onChange={(e) => setDraft(e.target.value)} />
          {legal || busy ? <Button type="submit" tone="primary" ariaLabel={cleared ? "Remove budget" : "Save budget"} busy={busy} busyLabel="Saving...">{cleared ? "Remove" : "Save"}</Button> : null}
        </form>
        {changed && !cleared && parsed === null ? <p className="os-profile-refusal">Enter an amount in dollars, at least $1.</p> : null}
        {refusal ? <p className="os-profile-refusal" role="alert">The budget was not changed: {refusal}</p> : null}
        <Caption>Auto uses this to guide paid model spending.</Caption>
      </SettingRow>
      <Facts>
        <Fact label="Spent this month" value={USD.format(spent)} />
        <Fact label="Resets" value={resetDate(resetsAt)} copyValue={resetsAt || undefined} />
      </Facts>
    </>
  );
}

// ---------------------------------------------------------------------------
// Sign-in and security
// ---------------------------------------------------------------------------

function SecurityFacts({ profile }: { profile: MyProfile }) {
  const passkeys = useMyPasskeys();
  const identity = useAppReach("identity");
  const count =
    passkeys.state === "ready" ? (passkeys.active === 0 ? "None" : passkeys.active === 1 ? "1 passkey" : `${passkeys.active} passkeys`)
      : passkeys.state === "reading" ? "Checking…" : "Not reported";
  return (
    <>
      <Facts>
        <Fact label="Passkeys" value={count} />
        <Fact label="Sign-in" value={profile.signInPolicy === "passkey_only" ? "Passkey only" : "Passkey or email link"} />
      </Facts>
      {identity.sections.includes("devices") ? (
        <div className="os-profile-link-row">
          <button type="button" className="os-link" onClick={() => identity.open("devices")}>Manage passkeys and sessions</button>
        </div>
      ) : null}
    </>
  );
}

// ---------------------------------------------------------------------------
// Preferences
// ---------------------------------------------------------------------------

/** Every IANA zone the browser knows, or the person's own and UTC when it
 *  cannot list them. */
export function timezoneChoices(current: string): string[] {
  const intl = Intl as unknown as { supportedValuesOf?: (key: string) => string[] };
  let zones: string[] = [];
  try {
    zones = intl.supportedValuesOf?.("timeZone") ?? [];
  } catch {
    zones = [];
  }
  const local = Intl.DateTimeFormat().resolvedOptions().timeZone;
  const set = new Set(zones);
  for (const zone of [current, local]) if (zone) set.add(zone);
  set.delete("UTC");
  return Array.from(set).sort();
}

function Preferences({ profile, writes }: { profile: MyProfile; writes: ProfileWrites }) {
  const ids = useId();
  const [busy, setBusy] = useState<"" | "computerUse" | "timezone">("");
  const [refusal, setRefusal] = useState<{ row: "computerUse" | "timezone"; why: string } | null>(null);
  const zones = useMemo(() => timezoneChoices(profile.timezone), [profile.timezone]);

  async function run(row: "computerUse" | "timezone", act: () => Promise<string>) {
    setBusy(row);
    setRefusal(null);
    const why = await act();
    setBusy("");
    if (why) setRefusal({ row, why });
  }

  return (
    <>
      <SettingRow label="Computer use">
        <Switch checked={profile.computerUseEnabled} disabled={busy === "computerUse"} onChange={(next) => void run("computerUse", () => writes.setComputerUse(next))}>
          Let tasks use my machines
        </Switch>
        {!profile.computerUseEnabled ? <Caption>Tasks cannot run anything on your machines until this is on.</Caption> : null}
        {refusal?.row === "computerUse" ? <p className="os-profile-refusal" role="alert">Not changed: {refusal.why}</p> : null}
      </SettingRow>
      <SettingRow label="Time zone">
        <Select id={`${ids}-tz`} label="Time zone" value={profile.timezone} onChange={(zone) => void run("timezone", () => writes.setTimezone(zone))}>
          <option value="">UTC</option>
          {zones.map((zone) => (
            <option key={zone} value={zone}>
              {zone.replace(/_/g, " ")}
            </option>
          ))}
        </Select>
        {refusal?.row === "timezone" ? <p className="os-profile-refusal" role="alert">Not changed: {refusal.why}</p> : null}
      </SettingRow>
    </>
  );
}
