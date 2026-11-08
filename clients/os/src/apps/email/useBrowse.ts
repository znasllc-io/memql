import { getRowByConceptAndId, rowString, sameEntityId, type Row } from "@znasllc-io/memql-sdk-core/client";
import { useLiveCollection, usePagedCollection } from "../../live/useLiveCollection";
import { flatten } from "../../kit/rows";
import { useDebouncedValue } from "../../logs/useDebouncedValue";
import { CAMPAIGN_CONCEPT, AUDIENCE_CONCEPT, TEMPLATE_CONCEPT, EMAIL_RULE_CONCEPT } from "./rows";

export type BrowseResource = "campaigns" | "audiences" | "templates" | "journeys" | "rules";
const concepts: Record<BrowseResource | "sites", string> = { sites: "v1:platform:site", campaigns: CAMPAIGN_CONCEPT, audiences: AUDIENCE_CONCEPT, templates: TEMPLATE_CONCEPT, journeys: "v1:campaigns:journey", rules: EMAIL_RULE_CONCEPT };
export interface BrowseQuestion { search?: string; status?: string; includeArchived?: boolean; accountId?: string; confirmation?: string; trigger?: string; audienceIds?: readonly string[] }

export function useEmailBrowse(resource: BrowseResource, question: BrowseQuestion, enabled = true) {
  const search = useDebouncedValue(question.search?.trim() ?? "", 250);
  const args = { ...question, ...(resource === "rules" ? { includeArchived: undefined } : {}), search: search || undefined };
  const key = JSON.stringify(args);
  const concept = concepts[resource];
  return usePagedCollection<Row>(enabled ? `email:browse:${resource}:${key}` : null, connection => ({
    concept,
    seed: async (cursor, signal) => {
      const options = { signal, ...(cursor ? { cursor } : {}) };
      const result = resource === "campaigns" ? await connection.query.campaignsBrowse({ ...args, audienceIds: args.audienceIds ? [...args.audienceIds] : undefined }, options)
        : resource === "audiences" ? await connection.query.audiencesBrowse(args, options)
        : resource === "templates" ? await connection.query.templatesBrowse(args, options)
        : resource === "journeys" ? await connection.query.journeysBrowse(args, options)
        : await connection.query.emailRulesBrowse(args, options);
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    reread: async (id, signal) => await getRowByConceptAndId(connection.query, concept, id, { signal }) as Row | null,
    inScope: raw => matchesBrowse(resource, flatten(raw), args),
  }));
}

function matchesBrowse(resource: BrowseResource, row: Row, args: BrowseQuestion): boolean {
  const status = rowString(row, "status");
  if (resource === "campaigns" && rowString(row, "testSourceCampaignId")) return false;
  if (resource !== "campaigns" && !args.includeArchived && status === "archived") return false;
  if (args.accountId && rowString(row, "accountId") !== args.accountId) return false;
  if (args.confirmation && rowString(row, "confirmation") !== args.confirmation) return false;
  if (args.status && (args.status === "progress" ? ["sent", "cancelled", "failed"].includes(status) : status !== args.status)) return false;
  const trigger = resource === "rules" ? "event" : (row.trigger as { kind?: string } | undefined)?.kind;
  if (args.trigger && trigger !== args.trigger) return false;
  const needle = args.search?.toLowerCase();
  if (!needle) return true;
  const fields = resource === "campaigns" ? ["name", "fromName", "replyTo"] : resource === "templates" ? ["name", "subject", "preheader"] : resource === "audiences" ? ["name", "description"] : ["name", "triggerConcept"];
  return fields.some(field => rowString(row, field).toLowerCase().includes(needle)) || (resource === "campaigns" && !!args.audienceIds?.includes(rowString(row, "audienceId")));
}

/** Selected records are resolved independently of list/search membership. */
export function useEmailRecord(resource: BrowseResource, id: string) {
  const concept = concepts[resource];
  return useLiveCollection<Row>(id ? `email:record:${resource}:${id}` : null, connection => ({
    concept,
    seed: async (_cursor, signal) => {
      const row = await getRowByConceptAndId(connection.query, concept, id, { signal });
      return { rows: row ? [row as Row] : [], nextCursor: "" };
    },
    eventInScope: rowId => sameEntityId(rowId, id),
    inScope: row => sameEntityId(rowString(flatten(row), "id"), id),
    reread: async (rowId, signal) => await getRowByConceptAndId(connection.query, concept, rowId, { signal }) as Row | null,
    paged: false,
  }));
}

/** Only referenced identities are hydrated for directory labels. */
export function useEmailReferences(resource: BrowseResource | "sites", ids: readonly string[]) {
  const wanted = [...new Set(ids.filter(Boolean))].sort();
  const concept = concepts[resource];
  return useLiveCollection<Row>(wanted.length ? `email:references:${resource}:${JSON.stringify(wanted)}` : null, connection => ({
    concept,
    seed: async (_cursor, signal) => {
      const rows: Row[] = [];
      for (let i = 0; i < wanted.length; i += 6) {
        const chunk = await Promise.all(wanted.slice(i, i + 6).map(id => getRowByConceptAndId(connection.query, concept, id, { signal })));
        rows.push(...chunk.filter((row): row is Row => row !== null));
      }
      return { rows, nextCursor: "" };
    },
    eventInScope: id => wanted.some(wantedId => sameEntityId(wantedId, id)),
    inScope: row => wanted.some(id => sameEntityId(id, rowString(flatten(row), "id"))),
    reread: async (id, signal) => await getRowByConceptAndId(connection.query, concept, id, { signal }) as Row | null,
    paged: false,
  }));
}

export function useCampaignAudienceSearch(search: string) {
  const needle = useDebouncedValue(search.trim(), 250);
  const feed = useLiveCollection<Row>(needle ? `email:audience-name-search:${needle}` : null, connection => ({
    concept: AUDIENCE_CONCEPT,
    seed: async (cursor, signal) => {
      const result = await connection.query.audienceNamesMatching({ search: needle }, { signal, ...(cursor ? { cursor } : {}) });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    reread: async (id, signal) => await getRowByConceptAndId(connection.query, AUDIENCE_CONCEPT, id, { signal }) as Row | null,
    inScope: row => rowString(flatten(row), "name").toLowerCase().includes(needle.toLowerCase()),
  }));
  return { ...feed, search: needle, ready: !needle || (feed.snapshot.state === "live" && !feed.snapshot.error), ids: feed.snapshot.rows.map(row => rowString(flatten(row), "id")) };
}
