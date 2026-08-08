import { useMemo, type ReactNode } from "react";
import { Link, useSearchParams } from "react-router-dom";

import { useCluster } from "../cluster/ClusterProvider";
import { useConcepts } from "../cluster/useConcepts";
import { Empty, ErrorMessage, Loading } from "../components/StatusMessage";
import {
  domainSummaries,
  filterConcepts,
  groupByDomain,
  type RegistryFilter,
} from "../concepts/registry";
import { DOMAIN_PARAM, SEARCH_PARAM, conceptPath } from "../concepts/urls";

// The concept registry: every concept this cluster declares, searchable and
// filterable by domain.
//
// Concept-agnostic by construction -- nothing here knows the name of any
// concept. Whatever the DSL declares (core domains, or a product bundle
// mounted at MEMQL_DSL_PATH) appears the moment it is declared, which is the
// same property view-kit enforces one level down for rows.
//
// THE FILTER LIVES IN THE URL, not in component state. A filtered registry is
// then itself an address someone can paste ("here is the identity area,
// narrowed to sessions"), it survives a refresh, and the back button undoes a
// search the way an operator expects. State would have to be lifted,
// serialized and parsed later to get any of that.

export function ConceptsPage(): ReactNode {
  const { status } = useCluster();
  const { concepts, loading, error } = useConcepts();
  const [params, setParams] = useSearchParams();

  const filter: RegistryFilter = {
    query: params.get(SEARCH_PARAM) ?? "",
    domain: params.get(DOMAIN_PARAM) ?? "",
  };

  // Domain counts come from the UNFILTERED registry: a filter that empties a
  // domain must not make that domain vanish from the control you would use to
  // get back out of it.
  const domains = useMemo(() => domainSummaries(concepts), [concepts]);
  // Keyed on the filter's two PRIMITIVE fields rather than the object: the
  // object is rebuilt every render from the search params, so depending on it
  // would recompute on every keystroke elsewhere on the page.
  const matches = useMemo(
    () => filterConcepts(concepts, { query: filter.query, domain: filter.domain }),
    [concepts, filter.query, filter.domain],
  );
  const groups = useMemo(() => groupByDomain(matches), [matches]);

  function setParam(name: string, value: string): void {
    const next = new URLSearchParams(params);
    if (value === "") next.delete(name);
    else next.set(name, value);
    // replace: typing in a search box must not push a history entry per
    // keystroke, or the back button becomes a character-by-character undo.
    setParams(next, { replace: true });
  }

  if (status !== "connected" && concepts.length === 0) {
    return (
      <Empty>Not connected to a cluster. See the connection state in the header.</Empty>
    );
  }

  return (
    <section className="mx-auto flex min-h-full max-w-5xl flex-col gap-5">
      <header>
        <h1 className="text-xl font-semibold tracking-tight">Concept registry</h1>
        <p className="mt-1 text-sm text-muted">
          {concepts.length === 0
            ? "Reading the registry from the cluster."
            : `${concepts.length} concepts declared across ${domains.length} domains. ` +
              "Pick one to inspect its schema and browse its rows."}
        </p>
      </header>

      <div className="flex flex-col gap-3">
        <label className="relative block">
          <span className="sr-only">Search concepts</span>
          <input
            type="search"
            value={filter.query}
            onChange={(event) => setParam(SEARCH_PARAM, event.target.value)}
            placeholder="Search ids, entities and descriptions…"
            className="w-full rounded-lg border border-line bg-surface px-3 py-2 text-sm text-fg placeholder:text-subtle"
          />
        </label>

        {/* Domain chips rather than a <select>: an operator scanning a
            registry wants to SEE how the cluster is carved up, and the counts
            are half the information. The row scrolls on its own axis so a
            cluster with thirty domains does not widen the page. */}
        <div className="-mx-1 flex gap-1 overflow-x-auto px-1 pb-1">
          <DomainChip
            label="All"
            count={concepts.length}
            active={filter.domain === ""}
            onSelect={() => setParam(DOMAIN_PARAM, "")}
          />
          {domains.map((summary) => (
            <DomainChip
              key={summary.domain}
              label={summary.domain}
              count={summary.count}
              active={filter.domain === summary.domain}
              onSelect={() =>
                setParam(
                  DOMAIN_PARAM,
                  filter.domain === summary.domain ? "" : summary.domain,
                )
              }
            />
          ))}
        </div>
      </div>

      {error ? (
        <ErrorMessage>Failed to list concepts: {error}</ErrorMessage>
      ) : loading && concepts.length === 0 ? (
        <Loading what="the concept registry" />
      ) : concepts.length === 0 ? (
        <Empty>This cluster declares no concepts.</Empty>
      ) : matches.length === 0 ? (
        <Empty>
          No concept matches that search.{" "}
          <button
            type="button"
            className="text-accent underline"
            onClick={() => setParams(new URLSearchParams(), { replace: true })}
          >
            Clear the filters
          </button>
        </Empty>
      ) : (
        <div className="flex flex-col gap-6 pb-6">
          {groups.map((group) => (
            <div key={group.domain}>
              <h2 className="mb-2 flex items-baseline gap-2 text-xs font-semibold tracking-wide text-muted uppercase">
                {group.domain}
                <span className="font-normal normal-case">{group.concepts.length}</span>
              </h2>
              <ul className="divide-y divide-line overflow-hidden rounded-lg border border-line bg-surface">
                {group.concepts.map((concept) => (
                  <li key={concept.id}>
                    <Link
                      to={conceptPath(concept.id, params.toString())}
                      className="flex flex-col gap-0.5 px-3 py-2.5 hover:bg-raised"
                    >
                      <span className="flex items-baseline gap-2">
                        <span className="font-medium text-fg">{concept.entity}</span>
                        <span className="truncate font-mono text-xs text-subtle">
                          {concept.id}
                        </span>
                      </span>
                      {concept.description ? (
                        <span className="line-clamp-2 text-xs text-muted">
                          {concept.description}
                        </span>
                      ) : null}
                    </Link>
                  </li>
                ))}
              </ul>
            </div>
          ))}
        </div>
      )}
    </section>
  );
}

function DomainChip({
  label,
  count,
  active,
  onSelect,
}: {
  label: string;
  count: number;
  active: boolean;
  onSelect: () => void;
}): ReactNode {
  return (
    <button
      type="button"
      onClick={onSelect}
      aria-pressed={active}
      className={
        "shrink-0 rounded-full border px-2.5 py-1 text-xs whitespace-nowrap " +
        (active
          ? "border-accent bg-accent-subtle font-medium text-fg"
          : "border-line bg-surface text-muted hover:bg-raised hover:text-fg")
      }
    >
      {label} <span className="text-subtle">{count}</span>
    </button>
  );
}
