import { useMemo, useState, type ReactNode } from "react";
import { Link, useNavigate } from "react-router-dom";

import { useCluster } from "../cluster/ClusterProvider";
import { useConcepts } from "../cluster/useConcepts";
import { Empty } from "../components/StatusMessage";
import { Checkbox, ErrorNotice, Skeleton, TextInput } from "../ui";
import { filterConcepts } from "../concepts/registry";
import { VIEWS } from "../views/registry";
import { ComposeButton } from "./ComposeLayout";
import { useDescribeView } from "./describe";
import { composeNewPath, composedViewPath } from "./urls";
import { useSavedViews } from "./useSavedViews";

// The composer's front door: what you have already composed, and what you
// could compose next.
//
// THE PICKER LISTS EVERY CONCEPT THE CLUSTER PUBLISHES, and it does not filter
// out the five that have a designed view -- it MARKS them. "No predefined
// view" is the case the issue is about, but it is not a prohibition: somebody
// who wants their own arrangement of the user list should have it, and a
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
  const [description, setDescription] = useState("");
  const { clients } = useCluster();
  const describe = useDescribeView(concepts, clients?.suggest);

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

      {/* DESCRIBE IT (epic memql#4661, task memql#4670). The other entry to a
          view: a sentence instead of a concept picker. It is FIRST because it
          is the one somebody who does not know what a concept is can use, and
          because the picker below is still there when it does not work. */}
      <section className="min-w-0">
        <h2 className="mb-3 text-xs font-semibold tracking-wide text-muted uppercase">
          Describe it
        </h2>
        <form
          className="flex flex-wrap items-center gap-2"
          onSubmit={(event) => {
            event.preventDefault();
            describe.ask(description);
          }}
        >
          <TextInput
            value={description}
            onChange={setDescription}
            placeholder="the agents that failed something this week"
            ariaLabel="Describe the view you want"
          />
          <ComposeButton
            tone="accent"
            onClick={() => describe.ask(description)}
            disabled={describe.status === "asking" || description.trim() === ""}
          >
            {describe.status === "asking" ? "Thinking…" : "Draft it"}
          </ComposeButton>
        </form>
        {describe.status === "unavailable" ? (
          <p className="mt-2 rounded border border-line bg-raised px-3 py-2 text-sm text-muted">
            {describe.unavailable} Pick the concepts yourself below and the composer will
            match elements to them.
          </p>
        ) : null}
        {describe.status === "ready" && describe.draft !== undefined ? (
          <div className="mt-2 rounded border border-accent bg-surface px-3 py-2">
            <p className="text-sm text-fg">
              {describe.draft.reasoning === ""
                ? `A draft over ${describe.draft.conceptIds.join(", ")}.`
                : describe.draft.reasoning}
            </p>
            <div className="mt-2 flex items-center gap-2">
              <ComposeButton
                tone="accent"
                onClick={() =>
                  navigate(composeNewPath(describe.draft!.conceptIds), {
                    // The draft rides HISTORY STATE rather than the URL: an
                    // arrangement is a structure, and a URL carrying one is a
                    // URL nobody can read or share. The concept ids stay in
                    // the query string, so a reload without the state opens a
                    // working composer over the same concepts.
                    state: { describedDraft: describe.draft },
                  })
                }
              >
                Open it
              </ComposeButton>
              <ComposeButton onClick={describe.dismiss}>Not that</ComposeButton>
            </div>
          </div>
        ) : null}
      </section>

      <section className="min-w-0">
        <h2 className="mb-3 text-xs font-semibold tracking-wide text-muted uppercase">
          Your views
        </h2>
        {saved.error ? (
          <ErrorNotice sentence="Could not read your saved views." next="Reload the page to read them again." detail={saved.error} />
        ) : saved.loading ? (
          <Skeleton variant="rows" rows={4} />
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
            <label className="block">
              <span className="sr-only">Search concepts</span>
              <TextInput type="search" value={query} onChange={setQuery} placeholder="Search concepts" />
            </label>
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
          <ErrorNotice sentence="Could not read the concept registry, so there is nothing to compose over yet." detail={error} />
        ) : loading ? (
          <Skeleton variant="rows" rows={4} />
        ) : matches.length === 0 ? (
          <Empty>No concept matches that search.</Empty>
        ) : (
          <ul className="flex flex-col divide-y divide-line rounded border border-line bg-surface">
            {matches.map((concept) => (
              <li key={concept.id} className="px-3 py-2">
                <Checkbox
                  checked={selected.includes(concept.id)}
                  onChange={() => toggle(concept.id)}
                  label={
                    <>
                      <span className="font-mono text-xs break-all text-subtle">
                        {concept.id}
                      </span>
                      {predefined.has(concept.id) ? (
                        <span className="ml-2 text-xs text-muted">
                          (has a designed view)
                        </span>
                      ) : null}
                      {/* Block spans, not <p>: a <label> takes PHRASING
                          content, and Checkbox WRAPS its label rather than
                          pointing at it with htmlFor -- which is what makes
                          the whole row clickable. */}
                      <span className="block text-sm text-fg">{concept.entity}</span>
                      {concept.description === "" ? null : (
                        <span className="block text-xs text-muted">{concept.description}</span>
                      )}
                    </>
                  }
                />
              </li>
            ))}
          </ul>
        )}
      </section>
    </section>
  );
}
