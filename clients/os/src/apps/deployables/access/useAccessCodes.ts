import { useCallback, useMemo, useRef, useState } from "react";
import { isTransportInterruption, refusalSentence, rowString, type QueryClient, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useReading, type Reading } from "../../../cluster/reading";
import { useOsConnection } from "../../../live/connection";
import type { SiteRow } from "../rows";

// The reads and the writes behind a deployable's Access codes (design record
// 2026-10-29-deployable-access-codes, section 5).
//
// NOTHING HERE IS A LIVE FEED, for the reason `usePreview` records: neither
// concept publishes graph events (D14), so a collection over one would render a
// list that never moves. So each read happens once and every write reads again.
// Both lists are BUILTINS, because the shared organization gate judges builtins
// and never a query, so a row is the handler's node set flattened by `rows()`, and
// an absent field is absent rather than empty.
//
// THE PLAIN CODE IS IN NO ROW. The reply to a mint or a regenerate is the only
// place it ever exists (D1), so `minted` holds it in React state, nothing writes it
// anywhere else, and it goes when the next write starts. A projection names its
// fields, so a row that somehow carried it would lose it here.

/** One code, as the panel reads it. */
export interface AccessCodeRow {
  id: string;
  label: string;
  gate: string;
  subject: string;
  /** The last 4 symbols, so an operator can tell codes apart. */
  codeHint: string;
  issuedAt: string;
  expiresAt: string;
  /** EMPTY MEANS NO SESSION OF THIS SECRET IS ON RECORD, which `lastOpenedWord` reads. */
  lastOpenedAt: string;
  /** The organization copied from the site at mint (D15). */
  accountId: string;
}

function accessCodeFromRow(row: Row): AccessCodeRow {
  return {
    id: rowString(row, "id"),
    label: rowString(row, "label"),
    gate: rowString(row, "gate"),
    subject: rowString(row, "subject"),
    codeHint: rowString(row, "codeHint"),
    issuedAt: rowString(row, "issuedAt"),
    expiresAt: rowString(row, "expiresAt"),
    lastOpenedAt: rowString(row, "lastOpenedAt"),
    accountId: rowString(row, "accountId"),
  };
}

/**
 * What the list draws: every code still standing, in label order.
 *
 * A REMOVED CODE STAYS AS THE ROW'S RECORD (D9) AND THE OS HIDES IT. LABEL ORDER
 * BECAUSE NOTHING ELSE HOLDS STILL: the engine promises no order, and `createdAt`
 * is the row's last write, so a newest-first list would reshuffle on every touch.
 */
export function accessCodesFromRows(rows: readonly Row[]): AccessCodeRow[] {
  return rows
    .filter((row) => rowString(row, "revokedAt") === "")
    .map(accessCodeFromRow)
    .filter((code) => code.id !== "")
    .sort((a, b) => a.label.localeCompare(b.label) || a.id.localeCompare(b.id));
}

/** One gate the deployable declares. */
export interface AccessGateRow {
  gate: string;
  usesSubject: boolean;
}

function accessGateFromRow(row: Row): AccessGateRow {
  // READ AS TEXT TOO, `packages/calls.ts`'s reading of a flag on a builtin's row,
  // which is how a scalar boolean has reached this client.
  const uses = row["usesSubject"];
  return { gate: rowString(row, "gate"), usesSubject: uses === true || uses === "true" };
}

/** The code as a person receives it, once. Never a row. */
export interface MintedCode {
  code: string;
  label: string;
}

/** What a write said back, and the code it was about: "" for one that was being created. */
export interface AccessRefusal {
  codeId: string;
  sentence: string;
  /** It may have landed all the same, so the panel says so over the list rather than in a draft. */
  unknown: boolean;
}

export type AccessAct = "create" | "regenerate" | "setExpiry" | "remove";

/**
 * How a write ended. REFUSED means the engine answered no, and the draft waits for
 * another try. UNKNOWN means nobody can say from here: the reply never came, or it
 * came without the code. Trying again from a draft could mint a second credential
 * beside one nobody holds, so the list is what gets looked at next.
 */
export type WriteOutcome = "done" | "refused" | "unknown";

/** What to do about a write the connection dropped under, for each act. */
const UNANSWERED: Readonly<Record<AccessAct, string>> = {
  create: "The connection dropped before the cluster answered, so this code may have been saved, and it cannot be shown now. If it is on the list, regenerate it to get one you can copy.",
  regenerate: "The connection dropped before the cluster answered, so a new code may have been saved, and the old one may have stopped working. Regenerate it again to get one you can copy.",
  setExpiry: "The connection dropped before the cluster answered, so this change may have been saved. Check the list before trying again.",
  remove: "The connection dropped before the cluster answered, so this change may have been saved. Check the list before trying again.",
};

/** A write the cluster accepted without the reply it owed: it landed, and nothing shows it. */
class NoCodeInReply extends Error {}

export interface NewAccessCode {
  label: string;
  gate: string;
  subject: string;
  expiresAt: string;
}

export interface AccessCodes {
  /** False before there is a connection to ask through: not a read in progress. */
  connected: boolean;
  codes: Reading<AccessCodeRow[]>;
  gates: Reading<AccessGateRow[]>;
  /** Why each read failed, each sentence once. Two refused reads are usually one refusal. */
  readFailures: string[];
  reread: () => void;
  /** The write in flight. One at a time, because a second mint is a second credential. */
  busy: AccessAct | null;
  refusal: AccessRefusal | null;
  clearRefusal: () => void;
  minted: MintedCode | null;
  dropMinted: () => void;
  create: (code: NewAccessCode) => Promise<WriteOutcome>;
  /** Keeps the expiry unless given one, which a code already past its expiry needs. */
  regenerate: (code: AccessCodeRow, expiresAt?: string) => Promise<WriteOutcome>;
  setExpiry: (code: AccessCodeRow, expiresAt: string) => Promise<WriteOutcome>;
  remove: (code: AccessCodeRow) => Promise<WriteOutcome>;
}

function mintedFrom(result: { rows(): Row[] }, label: string): MintedCode {
  const code = rowString(result.rows()[0] ?? null, "code");
  // Accepted with nothing to show: the secret has moved and nobody holds it, which
  // a form that simply closed would not have said.
  if (code === "") throw new NoCodeInReply("The cluster accepted that but did not return the code. Regenerate it to get one you can copy.");
  return { code, label };
}

/**
 * Runs a read, and puts a dropped socket's failure in words of its own: `stream
 * closed` is the transport talking, and it would reach the panel as the sentence.
 */
async function readOrSayDropped<T>(read: () => Promise<T>): Promise<T> {
  try {
    return await read();
  } catch (err: unknown) {
    if (isTransportInterruption(err)) throw new Error("The connection to the cluster dropped, so the access codes could not be read.");
    throw err;
  }
}

/**
 * A deployable's codes and gates, and the four acts on them.
 *
 * THE RE-READ IS A REVISION, NOT `reread()`. `reread` is ignored while a read is
 * out, which is right for a button and wrong here: a read that started before a
 * write landed answers with the world before it, and would stand. A revision
 * remembers one follow-up read.
 */
export function useAccessCodes(site: SiteRow): AccessCodes {
  const connection = useOsConnection();
  const id = site.id.trim();
  const [written, setWritten] = useState(0);
  const [busy, setBusy] = useState<AccessAct | null>(null);
  const [refusal, setRefusal] = useState<AccessRefusal | null>(null);
  const [minted, setMinted] = useState<MintedCode | null>(null);
  // A ref as well as `busy`: a second press before the page has drawn the first
  // finds the button still enabled, and two mints are two credentials.
  const inFlight = useRef(false);

  const readCodes = useMemo(() => {
    if (connection === null || id === "") return null;
    return async (signal: AbortSignal): Promise<AccessCodeRow[]> => {
      const result = await readOrSayDropped(() => connection.query.siteAccessCodesForSite({ siteId: id }, { signal }));
      return accessCodesFromRows(result.rows());
    };
  }, [connection, id]);
  const codes = useReading<AccessCodeRow[]>(connection === null || id === "" ? "no-site" : `access-codes:${id}`, readCodes, written);

  const readGates = useMemo(() => {
    if (connection === null || id === "") return null;
    return async (signal: AbortSignal): Promise<AccessGateRow[]> => {
      const result = await readOrSayDropped(() => connection.query.siteAccessGates({ siteId: id }, { signal }));
      return result.rows().map(accessGateFromRow).sort((a, b) => a.gate.localeCompare(b.gate));
    };
  }, [connection, id]);
  // Re-read with the codes: a refusal naming the gates means the list on screen is
  // out of date, and that is what would have told the person why.
  const gates = useReading<AccessGateRow[]>(connection === null || id === "" ? "no-site" : `access-gates:${id}`, readGates, written);

  const run = useCallback(
    async (act: AccessAct, construct: string, codeId: string, work: (query: QueryClient) => Promise<void>): Promise<WriteOutcome> => {
      // One is out already: this press did nothing, and what asked stays as it is.
      if (inFlight.current) return "refused";
      // THE HELD CODE GOES FIRST, whatever becomes of this write (`usePreview`'s
      // reasoning): two credentials on one screen is two a person has to tell apart.
      setMinted(null);
      setRefusal(null);
      if (connection === null) {
        setRefusal({ codeId, sentence: "Not connected to the cluster, so nothing was written.", unknown: false });
        return "refused";
      }
      inFlight.current = true;
      setBusy(act);
      try {
        await work(connection.query);
        return "done";
      } catch (err: unknown) {
        // A LOST REPLY IS NOT A REFUSAL. The dispatcher's own account is that a write's
        // outcome must be reconciled before any retry, and the words are ours, because
        // the socket's mean nothing to a person.
        if (err instanceof NoCodeInReply || isTransportInterruption(err)) {
          setRefusal({ codeId, sentence: err instanceof NoCodeInReply ? err.message : UNANSWERED[act], unknown: true });
          return "unknown";
        }
        // An engine frame with nothing in it answers "", and a refusal has to say
        // something: the draft is still on screen, waiting for a reason.
        setRefusal({ codeId, sentence: refusalSentence(err, construct).trim() || "That did not complete. Try again.", unknown: false });
        return "refused";
      } finally {
        inFlight.current = false;
        setBusy(null);
        setWritten((n) => n + 1);
      }
    },
    [connection],
  );

  const create = useCallback(
    (code: NewAccessCode): Promise<WriteOutcome> =>
      run("create", "siteAccessCodeCreate", "", async (query) => {
        const subject = code.subject.trim();
        const result = await query.siteAccessCodeCreate({
          siteId: id,
          label: code.label.trim(),
          gate: code.gate,
          ...(subject === "" ? {} : { subject }),
          expiresAt: code.expiresAt,
        });
        setMinted(mintedFrom(result, code.label.trim()));
      }),
    [run, id],
  );

  const regenerate = useCallback(
    (code: AccessCodeRow, expiresAt?: string): Promise<WriteOutcome> =>
      run("regenerate", "siteAccessCodeRegenerate", code.id, async (query) => {
        const result = await query.siteAccessCodeRegenerate({ codeId: code.id, ...(expiresAt === undefined ? {} : { expiresAt }) });
        setMinted(mintedFrom(result, code.label));
      }),
    [run],
  );

  const setExpiry = useCallback(
    (code: AccessCodeRow, expiresAt: string): Promise<WriteOutcome> =>
      run("setExpiry", "siteAccessCodeSetExpiry", code.id, async (query) => {
        await query.siteAccessCodeSetExpiry({ codeId: code.id, expiresAt });
      }),
    [run],
  );

  const remove = useCallback(
    (code: AccessCodeRow): Promise<WriteOutcome> =>
      run("remove", "siteAccessCodeRemove", code.id, async (query) => {
        await query.siteAccessCodeRemove({ codeId: code.id });
      }),
    [run],
  );

  const { reread: rereadCodes } = codes;
  const { reread: rereadGates } = gates;
  const reread = useCallback(() => {
    rereadCodes();
    rereadGates();
  }, [rereadCodes, rereadGates]);

  // BY STATE, NOT BY `error`: a frame with nothing in it answers "", and a read
  // that failed has to say so rather than draw an empty panel.
  const readFailures = [
    ...new Set([
      ...(codes.state === "failed" ? [refusalSentence(codes.error, "siteAccessCodesForSite").trim() || "The access codes could not be read."] : []),
      ...(gates.state === "failed" ? [refusalSentence(gates.error, "siteAccessGates").trim() || "The gates this app declares could not be read."] : []),
    ]),
  ];

  return {
    connected: connection !== null,
    codes,
    gates,
    readFailures,
    reread,
    busy,
    refusal,
    clearRefusal: useCallback(() => setRefusal(null), []),
    minted,
    dropMinted: useCallback(() => setMinted(null), []),
    create,
    regenerate,
    setExpiry,
    remove,
  };
}
