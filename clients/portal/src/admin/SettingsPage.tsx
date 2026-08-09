import type { ReactNode } from "react";
import { TABLE_ELEMENT } from "@znasllc-io/memql-view-kit";

import { ErrorMessage } from "../components/StatusMessage";
import { Band, MetaButton } from "../views/ViewLayout";
import { ViewElement } from "../views/ViewElement";
import { AdminFrame, Elsewhere, Reading, Refused } from "./AdminLayout";
import { brandAssetSummary, settingRows, SETTING_CONCEPT } from "./rows";
import { surfaceById } from "./urls";
import { useAdminAccess, useClusterSettings } from "./useAdminConsole";

// Cluster settings, as a reading.
//
// ===========================================================================
// WHY THIS PAGE DOES NOT EDIT
// ===========================================================================
// Not an unfinished form, and not a permissions decision this console made. The
// server-rendered settings page is gated by `requireAdmin` on the route, which
// is the only thing standing between a settings edit and anyone who can write
// to the cluster: `updateClusterSettings` declares no gate of its own, and the
// coarse capability check that would apply to a call over the stream admits
// every role from `writer` up (component/memql/data_verb.go). So wiring a form
// here would not MOVE the owner/admin gate to the browser -- it would delete
// it, and hand a writer the registration mode and every token lifetime.
//
// The read is the same story with a smaller blast radius, and it is worth
// stating rather than eliding: `clusterSettingsCurrent` is not role-gated
// either. What it projects is the operator-visible configuration -- brand,
// registration policy, lifetimes -- and the console shows it to owners and
// admins only, as a courtesy on top of an ungated read.
//
// Both are one `requiresOwnerOrAdmin` conjunct away from being correct, in
// dsl/identity. Until that lands, the honest surface is a reading plus a
// pointer, which is what this is.
export function SettingsPage(): ReactNode {
  const surface = surfaceById("settings");
  const { role, canAdminister, resolved } = useAdminAccess();
  const settings = useClusterSettings(canAdminister);
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

      <Elsewhere what="Changing a setting">
        Edits are not on this console. The mutation behind them carries no role
        check of its own — the owner-and-admin rule is enforced by the identity
        service's route, not by the cluster — so a form here would hand the
        registration mode and every token lifetime to anyone who can write. Edit
        at <code>/admin/settings</code> on the identity service, where that gate
        holds, until the mutation gates itself (memql#3324). Every save there
        writes a <code>cluster_settings_updated</code> audit event.
      </Elsewhere>
    </AdminFrame>
  );
}
