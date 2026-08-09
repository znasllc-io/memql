import { useMemo, useState, type ReactNode } from "react";
import { Link, useNavigate } from "react-router-dom";

import { useConcepts } from "../cluster/useConcepts";
import { Empty, ErrorMessage, Loading } from "../components/StatusMessage";
import { filterConcepts } from "../concepts/registry";
import { VIEWS } from "../views/registry";
import { ComposeButton } from "./ComposeLayout";
import { composeNewPath, composedViewPath } from "./urls";
import { useSavedViews } from "./useSavedViews";

// The composer's front door: what you have already composed, and what you
// could compose next.
//
// THE PICKER LISTS EVERY CONCEPT THE CLUSTER PUBLISHES, and it does not filter
// out the five that have a designed view -- it MARKS them. "No predefined
// view" is the case the issue is about, but it is not a prohibition: somebody
// who wants their own arrangement of the people list should have it, and a
// picker that silently omitted five concepts would be lying about what the
// cluster carries. The predefined set is read from the view registry rather
// than restated here, so the mark cannot go stale.
//
// No concept id appears in this file. The list comes from the registry the
// browser already reads (ConceptsListMsg), which is the property that makes a
// concept declared tomorrow composable tomorrow with no edit here.

export function ComposeHomePage(): ReactNode {
  const navigate = useNavigate();
  const { concepts, loading, error } = useConcepts();
  const saved = useSavedViews();
  const [query, setQuery] = useState("");
  const [selected, setSelected] = useState<readonly string[]>([]);

  const predefined = useMemo(() => new Set(VIEWS.map((view) => view.conceptId)), []);
  const matches = useMemo(
    () => filterConcepts(concepts, { query, domain: "" }),
    [concepts, query],
  );

  const toggle = (conceptId: string): void => {
    setSelected((current) =>
      current.includes(conceptId)
        ? current.filter((id) => id !== conceptId)
        : [...current, conceptId],
    );
  };

  return (
    <section className="flex min-h-full flex-col gap-8 pb-8">
      <header className="border-b border-line pb-4">
        <h1 className="text-xl font-semibold tracking-tight">Compose a view</h1>
        <p className="mt-1 max-w-2xl text-sm text-muted">
          Pick a concept and get a working view of it without writing any code. Elements
          are matched against the rows&rsquo; actual shape, so the arrangement is there
          before you touch anything; a model can propose a different one, and you can
          overrule either.
        </p>
      </header>

      <section className="min-w-0">
        <h2 className="mb-3 text-xs font-semibold tracking-wide text-muted uppercase">
          Your views
        </h2>
        {saved.error ? (
          <ErrorMessage>Failed to read your views: {saved.error}</ErrorMessage>
        ) : saved.loading ? (
          <Loading what="your saved views" />
        ) : saved.views.length === 0 ? (
          <Empty>You have not composed a view yet.</Empty>
        ) : (
          <ul className="flex flex-col gap-2">
            {saved.views.map((view) => (
              <li
                key={view.id}
                className="rounded border border-line bg-surface px-3 py-2"
              >
                <Link
                  to={composedViewPath(view.id)}
                  className="text-sm font-medium text-fg hover:underline"
                >
                  {view.name}
                </Link>
                <p className="text-xs text-subtle">
                  {view.description === "" ? "No description" : view.description}
                  {" · "}
                  {view.conceptIds.length === 1
                    ? view.conceptIds[0]
                    : `${view.conceptIds.length} concepts`}
                </p>
              </li>
            ))}
          </ul>
        )}
      </section>

      <section className="min-w-0">
        <div className="mb-3 flex flex-wrap items-center justify-between gap-3">
          <h2 className="text-xs font-semibold tracking-wide text-muted uppercase">
            Start from a concept
          </h2>
          <div className="flex items-center gap-2">
            <input
              type="search"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="Search concepts"
              aria-label="Search concepts"
              className="rounded border border-line bg-surface px-2 py-1 text-sm text-fg"
            />
            <ComposeButton
              tone="accent"
              disabled={selected.length === 0}
              onClick={() => navigate(composeNewPath(selected))}
              title={
                selected.length === 0
                  ? "Select at least one concept"
                  : "Open the composer over the selected concepts"
              }
            >
              Compose{selected.length > 1 ? ` ${selected.length} concepts` : ""}
            </ComposeButton>
          </div>
        </div>

        {error ? (
          <ErrorMessage>Failed to list concepts: {error}</ErrorMessage>
        ) : loading ? (
          <Loading what="the concept registry" />
        ) : matches.length === 0 ? (
          <Empty>No concept matches that search.</Empty>
        ) : (
          <ul className="flex flex-col divide-y divide-line rounded border border-line bg-surface">
            {matches.map((concept) => (
              <li key={concept.id} className="flex items-start gap-3 px-3 py-2">
                <input
                  type="checkbox"
                  id={`compose-${concept.id}`}
                  checked={selected.includes(concept.id)}
                  onChange={() => toggle(concept.id)}
                  className="mt-1"
                />
                <label htmlFor={`compose-${concept.id}`} className="min-w-0 cursor-pointer">
                  <span className="font-mono text-xs break-all text-subtle">
                    {concept.id}
                  </span>
                  {predefined.has(concept.id) ? (
                    <span className="ml-2 text-xs text-muted">
                      (has a designed view)
                    </span>
                  ) : null}
                  <p className="text-sm text-fg">{concept.entity}</p>
                  {concept.description === "" ? null : (
                    <p className="text-xs text-muted">{concept.description}</p>
                  )}
                </label>
              </li>
            ))}
          </ul>
        )}
      </section>
    </section>
  );
}
