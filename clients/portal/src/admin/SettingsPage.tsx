import { useState, type FormEvent, type ReactNode } from "react";
import { TABLE_ELEMENT } from "@znasllc-io/memql-view-kit";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import type { ClusterSettingsEdit } from "@znasllc-io/memql-sdk-core/identityadmin";

import { RowDetailDialog } from "../components/RowDetailDialog";
import { rowWithId, useLocalRowId } from "../components/localRow";
import { Band, Button, ErrorNotice, Field, Select, TextInput } from "../ui";
import { ElementView } from "../viewkit/ElementView";
import { AdminFrame, Reading, Refused } from "./AdminLayout";
import { brandAssetSummary, settingRows, SETTING_CONCEPT } from "./rows";

import { surfaceById } from "./urls";
import { useAdminAccess, useAdminWrites, useClusterSettings, type WriteState } from "./useAdminConsole";
import { WriteOutcome } from "./WriteOutcome";

// Cluster settings: a reading, and the form that changes it.
//
// ===========================================================================
// THE FORM IS NOT THE GATE
// ===========================================================================
// `updateClusterSettings` carries no role predicate of its own -- a MemQL
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
  const dialog = useLocalRowId();
  const dialogRow = rowWithId(rows, dialog.rowId);

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
      actions={<Button size="xs" onClick={settings.reload}>Refresh</Button>}
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
            sub="how a new user gets an account"
          />
          <Reading
            label="Brand images"
            value={settings.data === null ? "…" : brandAssetSummary(settings.data)}
            sub="shown on sign-in and in outbound mail"
          />
        </div>
        {settings.error === "" ? null : (
          <div className="mt-3">
            <ErrorNotice sentence="Could not read this cluster's settings." detail={settings.error} />
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
          <ElementView
            element={TABLE_ELEMENT}
            rows={rows}
            concept={SETTING_CONCEPT}
            options={{ bindings: { column: ["group", "setting", "value"] } }}
            onSelect={dialog.onSelect}
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

      <RowDetailDialog
        open={dialog.open}
        onClose={dialog.onClose}
        rowId={dialog.rowId}
        row={dialogRow ?? null}
      />
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
        <Field label="Registration mode">
          <Select ariaLabel="Registration mode" value={draft.registrationMode} onChange={(next) => set({ registrationMode: next })}>
            {["open", "domain_restricted", "invite_only", "waitlist"].map((mode) => (
              <option key={mode} value={mode}>
                {mode}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="Allowed email domains">
          <TextInput value={draft.registrationDomains} onChange={(v) => set({ registrationDomains: v })} />
        </Field>
        <Field label="Internal email domains">
          <TextInput value={draft.internalDomains} onChange={(v) => set({ internalDomains: v })} />
        </Field>
        <Field label="Role granted to internal users">
          <Select ariaLabel="Role granted to internal users" value={draft.internalDefaultRole} onChange={(next) => set({ internalDefaultRole: next })}>
            {["owner", "admin", "developer", "writer", "reader"].map((r) => (
              <option key={r} value={r}>
                {r}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="Waitlist notifications go to">
          <TextInput value={draft.accessRequestNotifyEmails} onChange={(v) => set({ accessRequestNotifyEmails: v })} />
        </Field>
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
        <Field label="Access token (minutes)">
          <TextInput value={draft.accessTokenMinutes} onChange={(v) => set({ accessTokenMinutes: v })} />
        </Field>
        <Field label="Refresh token (days)">
          <TextInput value={draft.refreshTokenDays} onChange={(v) => set({ refreshTokenDays: v })} />
        </Field>
        <Field label="Magic link (minutes)">
          <TextInput value={draft.magicLinkMinutes} onChange={(v) => set({ magicLinkMinutes: v })} />
        </Field>
        <Field label="Invitation (days)">
          <TextInput value={draft.invitationDays} onChange={(v) => set({ invitationDays: v })} />
        </Field>
        <Field label="Refresh cookie SameSite">
          <Select ariaLabel="Refresh cookie SameSite" value={draft.refreshCookieSameSite} onChange={(next) => set({ refreshCookieSameSite: next })}>
            <option value="">the node default</option>
            <option value="lax">lax</option>
            <option value="none">none</option>
          </Select>
        </Field>
      </fieldset>

      <fieldset className="flex flex-col gap-3 lg:col-span-2">
        <legend className="text-xs font-semibold tracking-wide text-muted uppercase">
          How the cluster presents itself
        </legend>
        <div className="grid gap-3 sm:grid-cols-2">
          <Field label="Brand name">
          <TextInput value={draft.brandName} onChange={(v) => set({ brandName: v })} />
        </Field>
          <Field label="Brand colour">
          <TextInput value={draft.brandPrimaryColor} onChange={(v) => set({ brandPrimaryColor: v })} />
        </Field>
        </div>
        <Field label="Registered OAuth clients (JSON)">
          <TextInput value={draft.registeredClientsJson} onChange={(v) => set({ registeredClientsJson: v })} />
        </Field>
      </fieldset>

      <div className="lg:col-span-2">
        <Button type="submit" busyLabel="Working…" busy={writes.busy}>Save the settings</Button>
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

