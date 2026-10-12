import { createContext, useCallback, useContext, useLayoutEffect, useMemo, useRef, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import type { OsAppProps } from "../../system/registry";

// OPENING A RECORD IN THE EMAIL APP FROM SOMEWHERE ELSE (memql#6033).
//
// Three things ask a tab to show a particular page, and they all ask the same
// way: search and Ask (an intent naming a campaign, an audience or a template),
// a page that links to another ("this audience", "this template"), and a flow
// that has to leave for a tab to finish ("Add an audience", "Set up sending").
// Each is "go to this tab and open THIS", so each is one REQUEST held at the
// app root and handed to the section it names.
//
// A REQUEST IS CONSUMED ONCE PER REVISION. The root keeps the last request for
// each section, because the section may not be mounted yet and one that is
// mounted will not mount again; the section acts on a revision it has not yet
// seen. Asking for the same record again is a new revision, so it opens the
// page again after the person has gone back to the list -- and a render that
// carries the same request does not open it a second time.
//
// This lives beside the sections rather than in `EmailApp.tsx` because every
// section imports it and `EmailApp.tsx` imports every section. `EmailApp.tsx`
// re-exports the names a section outside this directory would reach for.

/** The tabs whose records can be opened from elsewhere. */
export type OpenSection = "campaigns" | "automations" | "audiences" | "templates";

/** What to open: a record, or the section's Add wizard. */
export type OpenTarget = { id: string } | { add: true };

/** A record to open, or the section's Add wizard to start; consumed once per revision. */
export type OpenRequest = { revision: number } & OpenTarget;

/** A request to open Settings on one organization. */
export type SettingsRequest = { accountId: string; revision: number };

/**
 * The last request made of each tab. The Settings request is read by the
 * Organization field that scopes the whole Settings page.
 */
export type Requests = Partial<Record<OpenSection, OpenRequest>> & { settings?: SettingsRequest };

export interface EmailNavigation {
  /** Sets the request and calls `navigate(section, { fromContent: true })`. */
  openRecord: (section: OpenSection, target: OpenTarget) => void;
  /** Opens Settings scoped to one organization. */
  openSettings: (accountId: string) => void;
}

export const EmailNavigationContext = createContext<EmailNavigation | null>(null);

/**
 * The app root's side: the requests, and the openers that make them.
 *
 * The openers are the same functions for the life of the window. They read the
 * latest `navigate` through a ref, because the shell hands the app a new one on
 * every render and an opener that changed with it would restart every effect
 * that names it.
 */
export function useEmailRequests(navigate: OsAppProps["navigate"]): { requests: Requests; navigation: EmailNavigation } {
  const [requests, setRequests] = useState<Requests>({});
  const goto = useRef(navigate);
  goto.current = navigate;

  const openRecord = useCallback((section: OpenSection, target: OpenTarget) => {
    setRequests((held) => {
      const request: OpenRequest = { ...target, revision: (held[section]?.revision ?? 0) + 1 };
      return { ...held, [section]: request };
    });
    goto.current(section, { fromContent: true });
  }, []);

  const openSettings = useCallback((accountId: string) => {
    setRequests((held) => ({ ...held, settings: { accountId, revision: (held.settings?.revision ?? 0) + 1 } }));
    goto.current("settings", { fromContent: true });
  }, []);

  const navigation = useMemo(() => ({ openRecord, openSettings }), [openRecord, openSettings]);
  return { requests, navigation };
}

function useNavigation(hook: string): EmailNavigation {
  const navigation = useContext(EmailNavigationContext);
  if (navigation === null) throw new Error(`${hook} outside the Email app`);
  return navigation;
}

/** Open a record in one of the app's tabs, or start the tab's Add wizard. */
export function useOpenRecord(): EmailNavigation["openRecord"] {
  return useNavigation("useOpenRecord").openRecord;
}

/** Open Settings on one organization. */
export function useOpenSettings(): EmailNavigation["openSettings"] {
  return useNavigation("useOpenSettings").openSettings;
}

/**
 * A section's side: act on a request the first time its revision is seen.
 *
 * A LAYOUT EFFECT, so a page asked for before the section was even on screen is
 * the page the person first sees rather than the list for one frame. `apply`
 * is read fresh from a ref, so it may close over the section's state without
 * the request being re-applied each time that state changes.
 */
export function useOpenRequest(request: OpenRequest | undefined, apply: (target: OpenTarget) => void): void {
  const handled = useRef<number | undefined>(undefined);
  const latest = useRef(apply);
  latest.current = apply;
  useLayoutEffect(() => {
    if (request === undefined || request.revision === handled.current) return;
    handled.current = request.revision;
    latest.current("add" in request ? { add: true } : { id: request.id });
  }, [request]);
}

/**
 * Settings' side of `useOpenSettings`: act on a request the first time its
 * revision is seen. Asking again for the same organization is a new revision,
 * so it brings the person back to it after they have chosen another.
 *
 * `apply` is told the organization asked for and nothing about whether it can
 * be chosen: the organizations may not have arrived yet, and Settings checks
 * the choice against them when it draws, so a request made early still lands.
 */
export function useOpenSettingsRequest(request: SettingsRequest | undefined, apply: (accountId: string) => void): void {
  const handled = useRef<number | undefined>(undefined);
  const latest = useRef(apply);
  latest.current = apply;
  useLayoutEffect(() => {
    if (request === undefined || request.revision === handled.current) return;
    handled.current = request.revision;
    latest.current(request.accountId);
  }, [request]);
}

/**
 * The record a page shows: whichever one the id names in the section's WHOLE
 * feed, however its list is filtered. The lists leave finished campaigns and
 * archived audiences and templates out unless Settings says otherwise, and
 * search and Ask still reach them -- a page found only in the filtered list
 * would answer a question about last year's send with this year's. `null` while
 * the feed has not delivered the record (the list stays on screen and the page
 * opens when it does) and for an id that is not there.
 *
 * Projects only as far as the record, and not at all with no page open: the
 * feeds tick several times a second during a send.
 */
export function recordNamed<T extends { id: string }>(rows: readonly Row[], project: (row: Row) => T, id: string): T | null {
  if (id === "") return null;
  for (const row of rows) {
    const record = project(row);
    if (record.id === id) return record;
  }
  return null;
}
