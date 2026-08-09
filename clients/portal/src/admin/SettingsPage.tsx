import { useState, type FormEvent, type ReactNode } from "react";
import { TABLE_ELEMENT } from "@znasllc-io/memql-view-kit";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import type { ClusterSettingsEdit } from "@znasllc-io/memql-sdk-core/identityadmin";

import { ErrorMessage } from "../components/StatusMessage";
import { Band, MetaButton } from "../views/ViewLayout";
import { ViewElement } from "../views/ViewElement";
import { AdminFrame, Reading, Refused } from "./AdminLayout";
import { brandAssetSummary, settingRows, SETTING_CONCEPT } from "./rows";
import { SubmitButton } from "./PeoplePage";
import { surfaceById } from "./urls";
import { useAdminAccess, useAdminWrites, useClusterSettings, type WriteState } from "./useAdminConsole";
import { WriteOutcome } from "./WriteOutcome";

// Cluster settings: a reading, and the form that changes it.
//
// ===========================================================================
// THE FORM IS NOT THE GATE
// ===========================================================================
// `updateClusterSettings` carries no role predicate of its own -- a memQL
// mutation cannot -- and the coarse write check that applies to a call over
// the query surface admits every role from `writer` up. So the console does
// NOT call the mutation. It calls IdentityAdminClient, which lands on
// component/identity/adminops, where the owner/admin rule the retired templ
// route enforced now lives in one place, with the bounds checks and the
// `cluster_settings_updated` audit event beside it.
//
// The read is a separate story worth stating rather than eliding:
// `clusterSettingsCurrent` is not role-gated either. What it projects is the
// operator-visible configuration -- brand, registration policy, lifetimes --
// and the console shows it to owners and admins only, as a courtesy on top of
// an ungated read.
//
// UNITS. The row stores lifetimes in seconds (days for the invitation one) and
// an operator sets them in minutes and days. That conversion is presentation
// and lives here, in the form, rather than in the protocol -- the wire carries
// the concept's own units, so a second client cannot disagree about what a
// number means. 0 is the concept's sentinel for "use the boot default", and
// the form says so rather than showing a bare zero.
export function SettingsPage(): ReactNode {
  const surface = surfaceById("settings");
  const { role, canAdminister, resolved } = useAdminAccess();
  const settings = useClusterSettings(canAdminister);
  const writes = useAdminWrites();
  const rows = settingRows(settings.data);

  if (surface === undefined) return null;
  if (!canAdminister) {
    return (
      <AdminFrame surface={surface} role={role} resolved={resolved}>
        <Refused role={role} resolved={resolved} />
      </AdminFrame>
    );
  }

  const domain = (settings.data?.["clusterDomain"] as string | undefined) ?? "";
  const mode = (settings.data?.["registrationMode"] as string | undefined) ?? "";

  return (
    <AdminFrame
      surface={surface}
      role={role}
      resolved={resolved}
      actions={<MetaButton onClick={settings.reload}>Refresh</MetaButton>}
    >
      <Band>
        <div className="flex flex-wrap gap-2">
          <Reading
            label="Cluster domain"
            value={settings.loading ? "…" : domain === "" ? "not set" : domain}
            sub="every public service URL derives from it"
          />
          <Reading
            label="Registration"
            value={settings.loading ? "…" : mode === "" ? "unknown" : mode}
            sub="how a new person gets an account"
          />
          <Reading
            label="Brand images"
            value={settings.data === null ? "…" : brandAssetSummary(settings.data)}
            sub="shown on sign-in and in outbound mail"
          />
        </div>
        {settings.error === "" ? null : (
          <div className="mt-3">
            <ErrorMessage>Could not read the settings: {settings.error}</ErrorMessage>
          </div>
        )}
        <WriteOutcome state={writes} />
      </Band>

      <Band title="In force" meta="Anything left unset falls back to the node's environment" panel>
        {rows.length === 0 ? (
          <p className="p-3 text-sm text-subtle">
            {settings.loading
              ? "Reading settings…"
              : "This cluster has no settings row yet. The first-run wizard writes it."}
          </p>
        ) : (
          <ViewElement
            element={TABLE_ELEMENT}
            rows={rows}
            concept={SETTING_CONCEPT}
            options={{ bindings: { column: ["group", "setting", "value"] } }}
          />
        )}
      </Band>

      <Band title="Change a setting" meta="Every save writes one audit event">
        {settings.data === null ? (
          <p className="text-sm text-subtle">
            {settings.loading
              ? "Reading settings…"
              : "There is no settings row to edit yet. The first-run wizard writes it."}
          </p>
        ) : (
          <SettingsForm row={settings.data} writes={writes} onSaved={settings.reload} />
        )}
      </Band>

    </AdminFrame>
  );
}

// The editable slice, as a form.
//
// WHAT IS NOT HERE, and why each absence is deliberate rather than pending:
//
//   clusterDomain       every public service URL derives from it; changing it
//                       from a browser mid-session would break the session
//                       doing the changing. It is a deploy-time decision.
//   bootstrap*          the record of who claimed this cluster and when. An
//                       append-only fact, not a preference.
//   brand data-URIs     a megabyte of base64 is not something a text box
//                       edits; the retired console had a cropper for it, and a
//                       cropper is a feature, not a field.
//
// None of the three is passed by the write path either, so they INHERIT rather
// than being cleared -- the mutation read-merges, so an omitted argument keeps
// the persisted value while a passed empty string would wipe it.
function SettingsForm({
  row,
  writes,
  onSaved,
}: {
  row: Row;
  writes: WriteState;
  onSaved: () => void;
}): ReactNode {
  const [draft, setDraft] = useState(() => draftFrom(row));

  const submit = (event: FormEvent) => {
    event.preventDefault();
    writes.run((client) => client.updateClusterSettings(toEdit(draft)), onSaved);
  };
  const set = (patch: Partial<Draft>) => setDraft({ ...draft, ...patch });

  return (
    <form onSubmit={submit} className="grid gap-4 rounded-lg border border-line bg-surface p-4 lg:grid-cols-2">
      <fieldset className="flex flex-col gap-3">
        <legend className="text-xs font-semibold tracking-wide text-muted uppercase">
          Who gets in
        </legend>
        <label className="flex flex-col gap-1 text-xs text-muted">
          Registration mode
          <select
            aria-label="Registration mode"
            value={draft.registrationMode}
            onChange={(e) => set({ registrationMode: e.target.value })}
            className="rounded border border-line bg-surface px-2 py-1 text-sm text-fg"
          >
            {["open", "domain_restricted", "invite_only", "waitlist"].map((mode) => (
              <option key={mode} value={mode}>
                {mode}
              </option>
            ))}
          </select>
        </label>
        <Text label="Allowed email domains" value={draft.registrationDomains} onChange={(v) => set({ registrationDomains: v })} />
        <Text label="Internal email domains" value={draft.internalDomains} onChange={(v) => set({ internalDomains: v })} />
        <label className="flex flex-col gap-1 text-xs text-muted">
          Role granted to internal users
          <select
            aria-label="Role granted to internal users"
            value={draft.internalDefaultRole}
            onChange={(e) => set({ internalDefaultRole: e.target.value })}
            className="rounded border border-line bg-surface px-2 py-1 text-sm text-fg"
          >
            {["owner", "admin", "developer", "writer", "reader"].map((r) => (
              <option key={r} value={r}>
                {r}
              </option>
            ))}
          </select>
        </label>
        <Text
          label="Waitlist notifications go to"
          value={draft.accessRequestNotifyEmails}
          onChange={(v) => set({ accessRequestNotifyEmails: v })}
        />
      </fieldset>

      <fieldset className="flex flex-col gap-3">
        <legend className="text-xs font-semibold tracking-wide text-muted uppercase">
          How long a credential lives
        </legend>
        <p className="text-xs text-subtle">
          Leave a box empty to inherit the node&apos;s environment default. The
          cluster rejects a value outside its bounds rather than clamping it, so
          an out-of-range save comes back saying so.
        </p>
        <Text label="Access token (minutes)" value={draft.accessTokenMinutes} onChange={(v) => set({ accessTokenMinutes: v })} />
        <Text label="Refresh token (days)" value={draft.refreshTokenDays} onChange={(v) => set({ refreshTokenDays: v })} />
        <Text label="Magic link (minutes)" value={draft.magicLinkMinutes} onChange={(v) => set({ magicLinkMinutes: v })} />
        <Text label="Invitation (days)" value={draft.invitationDays} onChange={(v) => set({ invitationDays: v })} />
        <label className="flex flex-col gap-1 text-xs text-muted">
          Refresh cookie SameSite
          <select
            aria-label="Refresh cookie SameSite"
            value={draft.refreshCookieSameSite}
            onChange={(e) => set({ refreshCookieSameSite: e.target.value })}
            className="rounded border border-line bg-surface px-2 py-1 text-sm text-fg"
          >
            <option value="">the node default</option>
            <option value="lax">lax</option>
            <option value="none">none</option>
          </select>
        </label>
      </fieldset>

      <fieldset className="flex flex-col gap-3 lg:col-span-2">
        <legend className="text-xs font-semibold tracking-wide text-muted uppercase">
          How the cluster presents itself
        </legend>
        <div className="grid gap-3 sm:grid-cols-2">
          <Text label="Brand name" value={draft.brandName} onChange={(v) => set({ brandName: v })} />
          <Text label="Brand colour" value={draft.brandPrimaryColor} onChange={(v) => set({ brandPrimaryColor: v })} />
        </div>
        <Text
          label="Registered OAuth clients (JSON)"
          value={draft.registeredClientsJson}
          onChange={(v) => set({ registeredClientsJson: v })}
        />
      </fieldset>

      <div className="lg:col-span-2">
        <SubmitButton busy={writes.busy}>Save the settings</SubmitButton>
      </div>
    </form>
  );
}

interface Draft {
  registrationMode: string;
  registrationDomains: string;
  internalDomains: string;
  internalDefaultRole: string;
  accessRequestNotifyEmails: string;
  accessTokenMinutes: string;
  refreshTokenDays: string;
  magicLinkMinutes: string;
  invitationDays: string;
  refreshCookieSameSite: string;
  brandName: string;
  brandPrimaryColor: string;
  registeredClientsJson: string;
}

function text(row: Row, key: string): string {
  const v = row[key];
  return typeof v === "string" ? v : "";
}

// Seconds on the row -> the operator's unit in the box, and an unset value
// (0) -> an empty box rather than a "0" that reads as "no time at all".
function inUnits(row: Row, key: string, per: number): string {
  const v = row[key];
  const n = typeof v === "number" ? v : 0;
  return n <= 0 ? "" : String(n / per);
}

function draftFrom(row: Row): Draft {
  return {
    registrationMode: text(row, "registrationMode") || "open",
    registrationDomains: text(row, "registrationDomains"),
    internalDomains: text(row, "internalDomains"),
    internalDefaultRole: text(row, "internalDefaultRole") || "writer",
    accessRequestNotifyEmails: text(row, "accessRequestNotifyEmails"),
    accessTokenMinutes: inUnits(row, "accessTokenTTLSeconds", 60),
    refreshTokenDays: inUnits(row, "refreshTokenTTLSeconds", 86400),
    magicLinkMinutes: inUnits(row, "magicLinkTTLSeconds", 60),
    invitationDays: inUnits(row, "invitationTTLDays", 1),
    refreshCookieSameSite: text(row, "refreshCookieSameSite"),
    brandName: text(row, "brandName"),
    brandPrimaryColor: text(row, "brandPrimaryColor"),
    registeredClientsJson: text(row, "registeredClientsJSON"),
  };
}

// An unparseable or empty box becomes 0, which the concept reads as "use the
// boot default" -- the same thing the box said.
function whole(value: string, per: number): number {
  const n = Number.parseInt(value.trim(), 10);
  return Number.isFinite(n) && n > 0 ? n * per : 0;
}

function toEdit(d: Draft): ClusterSettingsEdit {
  return {
    registrationMode: d.registrationMode,
    registrationDomains: d.registrationDomains,
    internalDomains: d.internalDomains,
    internalDefaultRole: d.internalDefaultRole,
    accessRequestNotifyEmails: d.accessRequestNotifyEmails,
    accessTokenTtlSeconds: whole(d.accessTokenMinutes, 60),
    refreshTokenTtlSeconds: whole(d.refreshTokenDays, 86400),
    magicLinkTtlSeconds: whole(d.magicLinkMinutes, 60),
    invitationTtlDays: whole(d.invitationDays, 1),
    refreshCookieSameSite: d.refreshCookieSameSite,
    brandName: d.brandName,
    brandPrimaryColor: d.brandPrimaryColor,
    registeredClientsJson: d.registeredClientsJson,
  };
}

function Text({
  label,
  value,
  onChange,
}: {
  label: string;
  value: string;
  onChange: (next: string) => void;
}): ReactNode {
  return (
    <label className="flex flex-col gap-1 text-xs text-muted">
      {label}
      <input
        type="text"
        value={value}
        onChange={(event) => onChange(event.target.value)}
        className="rounded border border-line bg-surface px-2 py-1 text-sm text-fg"
      />
    </label>
  );
}
