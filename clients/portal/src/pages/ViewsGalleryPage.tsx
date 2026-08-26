import type { ReactNode } from "react";
import { Link, useNavigate } from "react-router-dom";

import { composePath, composedViewPath } from "../compose/urls";
import { useSavedViews } from "../compose/useSavedViews";
import { Band, Button, Container, EmptyState, ErrorNotice, PageHeader, Skeleton } from "../ui";
import { VIEWS } from "../views/registry";
import { viewPath } from "../views/urls";

// The Views gallery: every screen over this cluster's data, in one place.
//
// ===========================================================================
// WHY IT IS NOT IN src/views/
// ===========================================================================
// That directory is the guarded predefined-view tree: a module in it may not
// build row markup or iterate, because a view is a LAYOUT CHOICE over the
// shared element library rather than a second renderer (memql#3319,
// portal_view_composition_test.go, which scans the whole directory rather
// than a file list precisely so a newly added file cannot slip past).
//
// This page is not a view. It renders no concept rows at all -- it is an
// INDEX OF SCREENS, and the sequence it iterates is a list of destinations.
// Putting it in that tree would have meant either failing the gate or
// weakening it, and the gate is right: the next file somebody adds there is
// exactly where the first hand-rendered row would land.
//
// ===========================================================================
// WHY THIS PAGE EXISTS
// ===========================================================================
// The five built-in views, the caller's composed ones and the door to
// composing another were three separate things in the rail -- two captions
// and a sub-section -- and to the person reading them they were one kind of
// thing the whole time. Decision D1 makes Views ONE destination; this is what
// it lands on.
//
// It also fixes the shape that made the old rail unbounded: a saved view was
// a permanent rail row, so a person with forty of them had a rail with forty
// extra rows in it. A gallery scales; a rail does not.
//
// ===========================================================================
// TWO BANDS, AND THE SPLIT IS PROVENANCE RATHER THAN CATEGORY
// ===========================================================================
// Both bands hold the same kind of thing -- a designed screen over rows --
// and the only difference is who designed it. That is why they are two bands
// on one page instead of two pages: the reader is choosing a screen, not
// choosing between two systems.

function ViewCard({
  to,
  name,
  blurb,
  meta,
}: {
  to: string;
  name: string;
  blurb?: string;
  meta?: string;
}): ReactNode {
  return (
    <li className="min-w-0">
      <Link
        to={to}
        className="motion-wash flex h-full flex-col gap-1.5 rounded-lg border border-line bg-surface p-4 hover:border-line-strong hover:bg-raised"
      >
        <span className="truncate text-sm font-medium text-fg">{name}</span>
        {blurb === undefined ? null : (
          <span className="line-clamp-3 text-xs text-muted">{blurb}</span>
        )}
        {meta === undefined ? null : <span className="mt-auto text-xs text-subtle">{meta}</span>}
      </Link>
    </li>
  );
}

function Gallery({ children }: { children: ReactNode }): ReactNode {
  return (
    <ul className="list-rise grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
      {children}
    </ul>
  );
}

export function ViewsGalleryPage(): ReactNode {
  const saved = useSavedViews();
  const navigate = useNavigate();
  // A Button that navigates rather than a ButtonLink: ButtonLink is a real
  // <a href>, which is right for a deep link the browser should own and wrong
  // for an in-app route -- it would reload the whole application to reach a
  // page the router already has.
  const newView = <Button onClick={() => navigate(composePath())}>New view</Button>;

  return (
    <Container>
      <section className="flex min-h-full flex-col gap-6 pb-8">
        <PageHeader
          pageId="views"
          title="Views"
          blurb="Screens over this cluster's data. Five ship with the product; the rest are ones you composed."
          actions={newView}
        />

        <Band title="Built in">
          <Gallery>
            {VIEWS.map((view) => (
              <ViewCard
                key={view.id}
                to={viewPath(view.id)}
                name={view.label}
                blurb={view.blurb}
              />
            ))}
          </Gallery>
        </Band>

        <Band title="Yours">
          {saved.error !== "" ? (
            <ErrorNotice
              sentence="Could not read your saved views."
              next="Reload the page to read them again."
              detail={saved.error}
            />
          ) : saved.loading ? (
            <Skeleton variant="rows" rows={2} />
          ) : saved.views.length === 0 ? (
            <EmptyState
              firstRun
              statement="You have not composed a view yet. Pick a few concepts and get a working screen of them without writing any code."
              action={newView}
            />
          ) : (
            <Gallery>
              {saved.views.map((view) => (
                <ViewCard
                  key={view.id}
                  to={composedViewPath(view.id)}
                  name={view.name}
                  {...(view.description === "" ? {} : { blurb: view.description })}
                  meta={`${view.conceptIds.length} ${view.conceptIds.length === 1 ? "concept" : "concepts"}`}
                />
              ))}
            </Gallery>
          )}
        </Band>
      </section>
    </Container>
  );
}
