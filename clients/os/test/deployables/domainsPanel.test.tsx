import { render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { CUSTOM_DOMAIN_CONCEPT } from "../../src/apps/deployables/domains";
import {
  SHOP,
  click,
  siteRow,
  type,
  domainRow,
  emit,
  fakeConnection,
  withSession,
  type FakeConnection,
} from "./harness";

// The Domains content -- the Where-it-lives stop of the deployable page
// (epic memql#4885), which is where the Domains panel's body lives now --
// through the real LiveCollection and the real generated builders (epic
// memql#4805, task memql#4804). The sentences are the panel's; the surface
// that renders them is the page, opened as a cluster owner.
//
// Every call the panel makes reaches `executeNamed` as MemQL TEXT, so a
// mutation whose argument list does not render fails here rather than on a
// cluster nobody is watching.

function memStore() {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
  });
}

function mount(connection: FakeConnection, opts: { role?: string } = {}) {
  h.connection = connection;
  return render(
    withSession(
      <DeployablesApp
        sectionId="deployables"
        navigate={vi.fn()}
        askContext={vi.fn()}
        store={memStore()}
      />,
      { role: opts.role ?? "owner", userId: "u-me" },
    ),
  );
}

/** Opens the deployable's page, whose Where-it-lives stop mounts the content. */
async function openShop(connection: FakeConnection, opts: { role?: string } = {}) {
  mount(connection, opts);
  // An ARCHIVED deployable is found under the list's quiet archived flip
  // (memql#4889): the default population is what serves. The wait is for the
  // feed rather than for the row, so "not on the active list" is a real
  // answer rather than "has not arrived yet".
  await waitFor(() =>
    expect(document.querySelector("[data-os-livelist]")?.getAttribute("data-state")).toBe("live"),
  );
  if (screen.queryAllByText("shop.memql.example.com").length === 0) {
    const flip = screen.queryByRole("button", { name: /Show archived/ });
    if (flip !== null) await click(flip);
  }
  await click(await screen.findByText("shop.memql.example.com"));
  await openWhereItLives();
}

/**
 * Opens the Where-it-lives stop.
 *
 * DOMAINS ARE THAT STOP (epic memql#4937, design section C), and a settled
 * stop is one line now -- mark, label, its answer, a chevron. The address, the
 * client and the client's own domain are one question asked in one place, so
 * opening it is what a person does too.
 */
async function openWhereItLives(): Promise<void> {
  const page = await screen.findByRole("region", { name: /^Deployable / });
  await click(within(page).getByRole("button", { name: /^Cluster address/ }));
}

/**
 * Matches a binding's row by its accessible name, "Open <hostname>, <state>".
 *
 * A PLAIN STRING COMPARISON, not a RegExp built from the hostname. A hostname
 * is full of dots, and a pattern assembled from one either matches more than
 * was meant or needs an escaper that has to be right about every
 * metacharacter -- for a lookup that only ever wanted "starts with".
 */
function opens(hostname: string): (name: string) => boolean {
  return (name) => name.startsWith(`Open ${hostname},`);
}

/**
 * The card for one binding, found by its HEADING.
 *
 * A hostname appears TWICE on a card -- as the row's title, and as the Name of
 * its pointing record -- so a bare text query is ambiguous by construction.
 * That ambiguity is the design working: the record strip renders the exact
 * string somebody pastes into a registrar, which is the same string that names
 * the row.
 */
function card(_hostname: string): HTMLElement {
  return document.querySelector(".os-domain-page") as HTMLElement;
}

/**
 * A binding is a ROW in the domains list, and what the row opens is THE
 * WIZARD, come back to -- the same surface that added it, at whatever stage
 * the cluster has walked it to. So reaching it means opening the row, which
 * is what a person does too. Already on it: nothing to open.
 *
 * "Read" is the hostname being on the page: as the Domain step's answer while
 * it is being set up, as the title once it serves or is on its way out.
 */
async function findCard(hostname: string): Promise<HTMLElement> {
  if (document.querySelector(".os-domain-page") === null) {
    await click(await screen.findByRole("button", { name: opens(hostname) }));
  }
  await waitFor(() => expect(document.querySelector(".os-domain-page")).not.toBeNull());
  await waitFor(() => expect(within(card(hostname)).getAllByText(hostname).length).toBeGreaterThan(0));
  return card(hostname);
}

/** The states of the setup steps on the page, in order. */
function setupStates(): (string | null)[] {
  return [...document.querySelectorAll(".os-domain-page .os-rail > li")].map((li) => li.getAttribute("data-state"));
}

/** The floor of the page: whose turn it is, and what can be done. */
function floor(): HTMLElement {
  return document.querySelector(".os-domain-page .os-actbar") as HTMLElement;
}

/** The row for one binding, in the domains list. */
async function findRow(hostname: string): Promise<HTMLElement> {
  return screen.findByRole("button", { name: opens(hostname) });
}

/** Back from a binding's page (or the add form) to the domains list. */
async function backToDomains(): Promise<void> {
  await click(screen.getByRole("button", { name: "Back to Addresses and client" }));
}

/** The list's Add control opens the add form as its own page. */
async function openAddDomain(): Promise<void> {
  await click(await screen.findByRole("button", { name: "Add a domain" }));
}

beforeEach(() => {
  h.connection = null;
});

// ===========================================================================
// The gate
// ===========================================================================

describe("who sees the panel", () => {
  it("renders on the deployable page for a cluster owner", async () => {
    const connection = fakeConnection({ sites: [SHOP], domains: [domainRow({ id: "cd-1" })] });
    await openShop(connection);

    await screen.findByText("Domains");
    expect(connection.calls).toContain("query customDomainsAll()");
  });

  // PRESENTATION, NEVER THE BOUNDARY. The concept's clusterOwner tier and the
  // three Go guards are the enforcement; hiding the content from a reader who
  // cannot use it is a courtesy.
  // A READER SEES THE ADDRESS, AND ONLY THE ADDRESS. The cluster address was
  // always shown to everybody -- it stood alone above the panel -- so it is in
  // the list for everybody. What the `domains` part decides is the BINDINGS:
  // the concept is clusterOwner tier, so the feed is not even mounted for
  // somebody who may not read it, and no Add control is offered.
  it("shows a reader the cluster address, and no binding and no Add control", async () => {
    const connection = fakeConnection({ sites: [SHOP], domains: [domainRow({ id: "cd-1", hostname: "www.acme.com" })] });
    await openShop(connection, { role: "reader" });

    const list = await screen.findByRole("region", { name: /^Domains for / });
    expect(within(list).getByText("shop.memql.example.com")).toBeTruthy();
    expect(within(list).getByText(/Cluster address/)).toBeTruthy();
    expect(screen.queryByText("www.acme.com")).toBeNull();
    expect(screen.queryByRole("button", { name: "Add a domain" })).toBeNull();
  });
});

// ===========================================================================
// Every name the deployable answers on, as one list
// ===========================================================================

describe("the domains, as a list", () => {
  // THE REGRESSION THIS PINS. The address a deployable answers at stood alone
  // as a bare link while "Domains" listed only the custom ones, so a deployable
  // with no binding read "No custom domains" with its own domain a few lines up.
  it("always lists the cluster address, so a deployable is never shown with no domain", async () => {
    const connection = fakeConnection({ sites: [SHOP], domains: [] });
    await openShop(connection);

    const list = await screen.findByRole("region", { name: /^Domains for / });
    expect(within(list).getByRole("link", { name: "shop.memql.example.com" })).toBeTruthy();
    expect(within(list).getByText(/Cluster address/)).toBeTruthy();
    expect(screen.queryByText(/No custom domains/i)).toBeNull();
  });

  it("draws a binding as a row that opens its setup, not as a card in the list", async () => {
    const connection = fakeConnection({ sites: [SHOP], domains: [domainRow({ id: "cd-1", hostname: "www.acme.com" })] });
    await openShop(connection);

    // On the list there is a row, and its setup is not drawn yet: the list
    // and its detail do not share a scroll column (DESIGN.md rule 11).
    await findRow("www.acme.com");
    expect(document.querySelector(".os-domain-page")).toBeNull();

    await findCard("www.acme.com");
    expect(document.querySelector(".os-domain-page")).not.toBeNull();
    await backToDomains();
    await findRow("www.acme.com");
    expect(document.querySelector(".os-domain-page")).toBeNull();
  });

  // A SEEDED deployable -- MemQL OS, the VS Code site -- keeps its OWN address
  // fixed: that row is the site's, re-seeded at every boot. It still takes
  // bindings. A binding is a separate record pointing at the site, and nothing
  // in the custom-domain policy refuses a seeded one -- so hiding Add here was
  // the panel inventing a rule the server does not have.
  it("keeps a seeded deployable's own address fixed, and still lets a domain be added", async () => {
    const seeded = siteRow({ id: "site-shop", hostname: "shop.memql.example.com", status: "live", systemOwned: true });
    const connection = fakeConnection({ sites: [seeded], domains: [] });
    await openShop(connection);

    const list = await screen.findByRole("region", { name: /^Domains for / });
    expect(within(list).getByRole("link", { name: "shop.memql.example.com" })).toBeTruthy();
    expect(within(list).getByText(/Cluster address . built in$/)).toBeTruthy();
    // The built-in address is a plain line: nothing opens it, nothing removes it.
    expect(within(list).queryByRole("button", { name: opens("shop.memql.example.com") })).toBeNull();
    // Said ONCE (DESIGN.md rule 7).
    expect(within(list).getAllByText(/cannot be changed/)).toHaveLength(1);

    await openAddDomain();
    await type(screen.getByLabelText("Domain to bind") as HTMLInputElement, "www.acme.com");
    await click(screen.getByRole("button", { name: /add domain/i }));
    await waitFor(() => expect(connection.callsNamed("customDomainAdd")).toHaveLength(1));
    expect(connection.callsNamed("customDomainAdd")[0]).toContain('siteId: "site-shop"');
  });

  it("shows a seeded deployable's bound domain as a row like any other", async () => {
    const seeded = siteRow({ id: "site-shop", hostname: "shop.memql.example.com", status: "live", systemOwned: true });
    const connection = fakeConnection({ sites: [seeded], domains: [domainRow({ id: "cd-1", hostname: "www.acme.com" })] });
    await openShop(connection);
    await findRow("www.acme.com");
    await findCard("www.acme.com");
  });

  it("offers Add on a deployable that is not seeded, as the list's one control", async () => {
    const connection = fakeConnection({ sites: [SHOP], domains: [] });
    await openShop(connection);
    await screen.findByRole("region", { name: /^Domains for / });
    // The form is not standing above the list any more -- it is the page Add opens.
    expect(screen.queryByLabelText("Domain to bind")).toBeNull();
    await openAddDomain();
    expect(screen.getByLabelText("Domain to bind")).toBeTruthy();
  });
});

// ===========================================================================
// The list
// ===========================================================================

describe("the list", () => {
  it("shows only the bindings on the deployable being looked at", async () => {
    const connection = fakeConnection({
      sites: [SHOP],
      domains: [
        domainRow({ id: "cd-1", hostname: "www.acme.com" }),
        domainRow({ id: "cd-2", hostname: "other.example.net", siteId: "site-elsewhere" }),
      ],
    });
    await openShop(connection);

    await findCard("www.acme.com");
    expect(screen.queryByText("other.example.net")).toBeNull();
  });

  // A DOMAIN SOMEBODY REMOVED OR CANCELLED LEAVES THE LIST. The row survives in
  // the cluster -- what it served, and when, is the engine's to keep -- but the
  // list is the names this deployable answers on, and the owner's word on a
  // cancelled one sitting there was "it should remove the item from the list".
  it("does not list a removed binding, or one the cluster is quietly taking down", async () => {
    const connection = fakeConnection({
      sites: [SHOP],
      domains: [
        domainRow({ id: "cd-1", hostname: "gone.acme.com", status: "removed" }),
        domainRow({ id: "cd-2", hostname: "going.acme.com", status: "removing" }),
        domainRow({ id: "cd-3", hostname: "www.acme.com", status: "verifying" }),
      ],
    });
    await openShop(connection);

    await findRow("www.acme.com");
    expect(screen.queryByText("gone.acme.com")).toBeNull();
    expect(screen.queryByText("going.acme.com")).toBeNull();
  });

  // ONLY `removed` FREES A HOSTNAME, so a removal the cluster could not finish
  // is the one that stays: out of sight it would refuse that name to whoever
  // added it again, with nothing on the page to say why.
  it("keeps a removal the cluster could not finish, and says what it reported", async () => {
    const connection = fakeConnection({
      sites: [SHOP],
      domains: [domainRow({
        id: "cd-1", hostname: "stuck.acme.com", status: "removing", failureReason: "removal_failed",
        failureDetail: "ingresses.networking.k8s.io is forbidden", lastCheckedAt: "2026-09-01T12:00:00Z",
      })],
    });
    await openShop(connection);

    const listed = await findRow("stuck.acme.com");
    expect(within(listed).getByText(/Could not be removed yet/)).toBeTruthy();

    await findCard("stuck.acme.com");
    expect(screen.getByRole("heading", { name: "stuck.acme.com" })).toBeTruthy();
    expect(within(floor()).getByText("Could not be removed yet")).toBeTruthy();
    expect(floor().getAttribute("data-tone")).toBe("paused");
    expect(screen.getByText(/could not take this domain down/)).toBeTruthy();
    expect(screen.getByText("ingresses.networking.k8s.io is forbidden")).toBeTruthy();
    // No steps to take and nothing to remove twice.
    expect(setupStates()).toEqual([]);
    expect(within(floor()).queryByRole("button", { name: /^Remove|^Cancel/ })).toBeNull();
  });
});

// ===========================================================================
// The deployable's own status
// ===========================================================================

describe("what the domain's status does not say", () => {
  // A BINDING REACHES `live` ON ITS OWN MERITS -- both DNS records check out
  // and the certificate is issued -- and that says nothing about whether a
  // visitor gets anything. The edge decides that from the DEPLOYABLE's status,
  // before any file lookup. Without this notice the panel says "serving" about
  // a hostname the internet 404s, which was true of the epic as it shipped.
  it("says nothing is served when the deployable is not live, even with a live domain", async () => {
    const connection = fakeConnection({
      sites: [siteRow({ id: "site-shop", hostname: "shop.memql.example.com", status: "draft" })],
      domains: [domainRow({ id: "cd-1", hostname: "www.acme.com", status: "live" })],
    });
    await openShop(connection);

    await findCard("www.acme.com");
    // The domain's own status is unchanged -- it IS live, and saying otherwise
    // would be a different lie.
    expect(within(floor()).getByText("Ready")).toBeTruthy();
    expect(within(floor()).getByText(/make the deployable live/)).toBeTruthy();
    // The last step is the person's, and it is not on this page.
    expect(setupStates()).toEqual(["done", "done", "done", "done", "open"]);
    expect(within(card("www.acme.com")).queryByRole("link", { name: /Open/ })).toBeNull();
    expect(within(floor()).queryByRole("button", { name: /^Open / })).toBeNull();
    // And the panel says what that does and does not mean.
    expect(screen.getByText(/This deployable is draft, so nothing is served/i)).toBeTruthy();
  });

  it("names a paused deployable in its own words", async () => {
    const connection = fakeConnection({
      sites: [siteRow({ id: "site-shop", hostname: "shop.memql.example.com", status: "disabled" })],
      domains: [domainRow({ id: "cd-1", status: "live" })],
    });
    await openShop(connection);

    await screen.findByText(/This deployable is disabled, so nothing is served/i);
  });

  // NAMED BY WHAT SERVES, and this is the case that pays for it. `archived`
  // arrived with the packages epic (memql#4794) AFTER this notice was written,
  // and the notice covered it with no edit: `live` is the one status that
  // serves, so every value added later is on the warned side by construction.
  // That is the same inversion component/edge's own switch carries, and the
  // reason a future enum addition cannot silently start claiming to serve.
  it("names a status added after this notice was written", async () => {
    const connection = fakeConnection({
      sites: [siteRow({ id: "site-shop", hostname: "shop.memql.example.com", status: "archived" })],
      domains: [domainRow({ id: "cd-1", status: "live" })],
    });
    await openShop(connection);

    await screen.findByText(/This deployable is archived, so nothing is served/i);
  });

  // AND A VALUE THIS BUILD HAS NEVER SEEN still gets the notice, unnamed.
  // `siteFromRow` (rows.ts) narrows the wire string through SITE_STATUSES and
  // normalises anything outside it to the EMPTY STRING, so an undeclared value
  // reaches this component as "". The notice is what a person needs either way
  // -- nothing is served -- and it says "not live" rather than inventing a word
  // for a status the build cannot describe.
  it("covers a status this build does not recognise", async () => {
    const connection = fakeConnection({
      sites: [siteRow({ id: "site-shop", hostname: "shop.memql.example.com", status: "quarantined" })],
      domains: [domainRow({ id: "cd-1", status: "live" })],
    });
    await openShop(connection);

    await screen.findByText(/This deployable is not live, so nothing is served/i);
  });

  // THE OTHER DIRECTION, which is what stops this becoming a standing banner.
  it("says nothing when the deployable is live", async () => {
    const connection = fakeConnection({
      sites: [SHOP],
      domains: [domainRow({ id: "cd-1", status: "live" })],
    });
    await openShop(connection);

    await findCard("www.acme.com");
    expect(screen.queryByText(/nothing is served at any of its domains/i)).toBeNull();
  });
});

// ===========================================================================
// The guidance
// ===========================================================================

describe("the records to create", () => {
  it("withholds targets on a failed read and loads them on retry", async () => {
    const seed = { sites: [SHOP], domains: [domainRow({ id: "cd-1", hostname: "acme.com", status: "verifying", failureReason: "dns_not_pointing" })], domainDNSError: "Routing address lookup failed" };
    const connection = fakeConnection(seed);
    await openShop(connection);
    const c = await findCard("acme.com");
    await waitFor(() => expect(within(c).getByText("DNS targets could not be loaded.")).toBeTruthy());
    expect(within(c).queryByLabelText("Copy value: 203.0.113.10")).toBeNull();
    seed.domainDNSError = "";
    await click(within(c).getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(within(c).getByLabelText("Copy value: 203.0.113.10")).toBeTruthy());
    expect(connection.calls.some(call => call.startsWith("mutation"))).toBe(false);
  });

  it("guides ownership before DNS, with copyable values and no modal", async () => {
    const connection = fakeConnection({ sites: [SHOP], domains: [domainRow({ id: "cd-1", hostname: "www.acme.com", token: "tok-xyz" })] });
    await openShop(connection);
    const c = await findCard("www.acme.com");
    expect(screen.queryByRole("dialog")).toBeNull();
    // COME BACK TO, IT IS THE WIZARD THAT ADDED IT -- the same title, the name
    // as a fact, and the stage the cluster has walked it to standing open.
    expect(screen.getByRole("heading", { name: "Add a domain" })).toBeTruthy();
    expect(setupStates()).toEqual(["done", "current", "waiting", "ahead", "ahead"]);
    expect(within(c).getByLabelText("Copy value: tok-xyz")).toBeTruthy();
    expect(within(c).getByLabelText("Copy name: _memql-verify.www.acme.com")).toBeTruthy();
    // The DNS records are one click away before ownership passes; the stages
    // after them are names.
    expect(within(c).getByRole("button", { name: /^DNS/ }).getAttribute("aria-expanded")).toBe("false");
    expect(within(c).queryByRole("button", { name: /^Certificate/ })).toBeNull();

    await emit(connection, CUSTOM_DOMAIN_CONCEPT, domainRow({ id: "cd-1", hostname: "www.acme.com", token: "tok-xyz", status: "verifying", failureReason: "dns_not_pointing" }));
    // THE RAIL FOLLOWS THE FEED: ownership passed, so DNS opens by itself.
    await waitFor(() => expect(setupStates()).toEqual(["done", "done", "current", "ahead", "ahead"]));
    await waitFor(() => expect(within(c).getByLabelText("Copy value: 203.0.113.10")).toBeTruthy());
    expect(within(c).getByText("A")).toBeTruthy();
    // A settled step can be read again, and the step the flow is at is one
    // click back -- there is no separate "continue" control to find.
    await click(within(c).getByRole("button", { name: /^Ownership/ }));
    expect(within(c).getByLabelText("Copy value: tok-xyz")).toBeTruthy();
    expect(within(c).queryByLabelText("Copy value: 203.0.113.10")).toBeNull();
    await click(within(c).getByRole("button", { name: /^DNS/ }));
    expect(within(c).getByLabelText("Copy value: 203.0.113.10")).toBeTruthy();
  });

  it("offers a GoDaddy-compatible A record and keeps alternate choices read-only", async () => {
    const connection = fakeConnection({ sites: [SHOP], domains: [domainRow({ id: "cd-1", hostname: "acme.com", status: "verifying", failureReason: "dns_not_pointing" })] });
    await openShop(connection);
    const c = await findCard("acme.com");
    await waitFor(() => expect(within(c).getByLabelText("Copy value: 203.0.113.10")).toBeTruthy());
    expect(within(c).getByText("A")).toBeTruthy();
    expect(within(c).queryByRole("combobox", { name: "Domain location" })).toBeNull();
    await click(within(c).getByRole("combobox", { name: "DNS record type" }));
    await click(screen.getByRole("option", { name: "CNAME — subdomain or provider flattening" }));
    expect(within(c).getByText("CNAME")).toBeTruthy();
    expect(within(c).getByLabelText("Copy name: @")).toBeTruthy();
    expect(within(c).getByLabelText("Copy value: routing.example.net")).toBeTruthy();
    expect(within(c).getByText(/GoDaddy does not support root CNAME/)).toBeTruthy();
    const writeText = vi.fn().mockResolvedValue(undefined);
    const previousClipboard = Object.getOwnPropertyDescriptor(navigator, "clipboard");
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
    try {
      await click(within(c).getByLabelText("Copy name: @"));
      await click(within(c).getByLabelText("Copy value: routing.example.net"));
      expect(writeText.mock.calls).toEqual([["@"], ["routing.example.net"]]);
    } finally {
      if (previousClipboard) Object.defineProperty(navigator, "clipboard", previousClipboard);
      else Reflect.deleteProperty(navigator, "clipboard");
    }

    expect(within(c).queryByRole("button", { name: /Certificate$/ })).toBeNull();
    expect(connection.calls.some(call => call.startsWith("mutation"))).toBe(false);
  });

  it("advances through certificate readiness only on live server updates and returns to ownership after a failed check", async () => {
    const connection = fakeConnection({ sites: [SHOP], domains: [domainRow({ id: "cd-1", status: "verifying", failureReason: "dns_not_pointing" })] });
    await openShop(connection);
    const c = await findCard("www.acme.com");
    await emit(connection, CUSTOM_DOMAIN_CONCEPT, domainRow({ id: "cd-1", status: "issuing", failureReason: "issuance_failed", failureDetail: "Certificate pending" }));
    // A REASON THE SWEEP CANNOT OUTWAIT IS A STOP, said under the step that stopped.
    await waitFor(() => expect(setupStates()).toEqual(["done", "done", "done", "stopped", "ahead"]));
    expect(within(c).getByText("Certificate pending")).toBeTruthy();
    expect(within(floor()).getByText("Needs attention")).toBeTruthy();
    expect(floor().getAttribute("data-tone")).toBe("paused");
    expect(within(c).queryByRole("link", { name: /Open/ })).toBeNull();

    await emit(connection, CUSTOM_DOMAIN_CONCEPT, domainRow({ id: "cd-1", status: "live" }));
    await waitFor(() => expect(within(c).getByRole("link", { name: "Open www.acme.com" })).toBeTruthy());
    // The adding is over, so the page is the domain's own.
    expect(screen.getByRole("heading", { name: "www.acme.com" })).toBeTruthy();
    expect(within(floor()).getByText("Serving")).toBeTruthy();

    // A REGRESSION IS FOLLOWED TOO: completion is the feed's to say, both ways.
    await emit(connection, CUSTOM_DOMAIN_CONCEPT, domainRow({ id: "cd-1", status: "verifying", failureReason: "dns_token_missing" }));
    await waitFor(() => expect(setupStates()).toEqual(["done", "current", "waiting", "ahead", "ahead"]));
    expect(within(c).queryByRole("link", { name: /Open/ })).toBeNull();
  });

});

// ===========================================================================
// The typed failure
// ===========================================================================

describe("a typed failure", () => {
  it("names which record is wrong and shows what the cluster saw", async () => {
    const connection = fakeConnection({
      sites: [SHOP],
      domains: [
        domainRow({
          id: "cd-1",
          status: "verifying",
          failureReason: "dns_not_pointing",
          failureDetail: "www.acme.com resolves to 198.51.100.7; it needs a CNAME to os.memql.example.com",
          lastCheckedAt: "2026-09-01T12:00:00Z",
        }),
      ],
    });
    await openShop(connection);

    const c = await findCard("www.acme.com");
    // WAITING FOR A RECORD IS A WAIT. The reason arrives in `failureReason`,
    // and it used to open this page on a red box and a stopped step -- for a
    // record that simply is not there YET. It names the step, in the tone of a
    // wait, and the sweep will look again.
    expect(setupStates()).toEqual(["done", "done", "current", "ahead", "ahead"]);
    expect(within(c).getByText("Waiting for the records to point here")).toBeTruthy();
    expect(floor().getAttribute("data-tone")).toBe("busy");
    expect(within(floor()).getByText("Waiting for DNS to point here")).toBeTruthy();
    // THE OBSERVATION, VERBATIM, under the step it is about: somebody editing
    // a zone file needs to know what IS published, not only that it is wrong.
    expect(within(c).getByText(/resolves to 198\.51\.100\.7/)).toBeTruthy();
    // ...and the record it is about is marked as the one still looked for.
    await waitFor(() => expect(c.querySelector('.os-record[data-awaited="true"]')).not.toBeNull());
    expect(within(floor()).getByText(/^checked /)).toBeTruthy();
  });

  // AN UNRECOGNISED REASON KEEPS ITS OWN TOKEN. Inventing a friendly sentence
  // for a failure this build does not know is how a real fault gets mistaken
  // for a user error.
  it("renders a reason this build does not recognise as itself", async () => {
    const connection = fakeConnection({
      sites: [SHOP],
      domains: [domainRow({ id: "cd-1", status: "verifying", failureReason: "some_new_reason" })],
    });
    await openShop(connection);

    await findCard("www.acme.com");
    expect(screen.getByText("some_new_reason")).toBeTruthy();
  });

  // THERE IS NO RE-CHECK BUTTON ANYWHERE (design D5). Retries ride the sweep's
  // schedule; a button would invite hammering a resolver and an ACME endpoint.
  it("offers no way to re-check", async () => {
    const connection = fakeConnection({
      sites: [SHOP],
      domains: [domainRow({ id: "cd-1", status: "verifying", failureReason: "dns_token_missing" })],
    });
    await openShop(connection);

    await findCard("www.acme.com");
    for (const label of [/re-?check/i, /check now/i, /retry/i, /refresh/i]) {
      expect(screen.queryByRole("button", { name: label })).toBeNull();
    }
    // And it says WHY the control is absent, so it does not read as an
    // omission: the floor says the cluster checks by itself, and when it did.
    expect(within(floor()).getByText(/this keeps checking if you leave/)).toBeTruthy();
    expect(within(floor()).getByText(/^checked |^not checked yet$/)).toBeTruthy();
  });
});

// ===========================================================================
// Live
// ===========================================================================

describe("live", () => {
  it("ticks when a status flips under the person watching", async () => {
    const connection = fakeConnection({
      sites: [SHOP],
      domains: [domainRow({ id: "cd-1", status: "issuing" })],
    });
    await openShop(connection);

    await screen.findByText("getting a certificate");
    await emit(connection, CUSTOM_DOMAIN_CONCEPT, domainRow({ id: "cd-1", status: "live" }));
    await waitFor(() => expect(screen.getByText("serving")).toBeTruthy());
  });

  // A HEARTBEAT IS NOT NEWS. `lastCheckedAt` moves for every non-terminal
  // binding every two minutes forever; announcing it would make the panel a
  // strobe. The status must be unchanged and no cue must fire.
  it("does not announce a bare lastCheckedAt bump", async () => {
    const connection = fakeConnection({
      sites: [SHOP],
      domains: [domainRow({ id: "cd-1", status: "verifying", lastCheckedAt: "2026-09-01T12:00:00Z" })],
    });
    const { container } = mount(connection);
    await screen.findByText("shop.memql.example.com");
    await click(screen.getByText("shop.memql.example.com"));
    await openWhereItLives();
    await findCard("www.acme.com");

    await emit(
      connection,
      CUSTOM_DOMAIN_CONCEPT,
      domainRow({ id: "cd-1", status: "verifying", lastCheckedAt: "2026-09-01T12:02:00Z" }),
    );
    await waitFor(() => expect(within(floor()).getByText("Waiting for the ownership record")).toBeTruthy());
    // The cue's own marker, which LiveList renders on an arrival.
    expect(container.querySelectorAll("[data-tick]").length).toBe(0);
  });
});

// ===========================================================================
// Add
// ===========================================================================

describe("adding a domain", () => {
  it("calls the capability, with the hostname normalised", async () => {
    const connection = fakeConnection({ sites: [SHOP], domains: [] });
    await openShop(connection);

    await screen.findByText("Domains");
    await openAddDomain();
    const input = screen.getByLabelText("Domain to bind") as HTMLInputElement;
    await type(input, "  WWW.Acme.com. ");
    await click(screen.getByRole("button", { name: /add domain/i }));

    await waitFor(() => {
      const calls = connection.callsNamed("customDomainAdd");
      expect(calls.length).toBe(1);
      // The generated builder ran, and this is the text that reached the wire.
      expect(calls[0]).toContain('hostname: "www.acme.com"');
      expect(calls[0]).toContain('siteId: "site-shop"');
    });
    // THE TOKEN IS NOT SENT. A caller who chooses their own verification token
    // proves nothing by publishing it, which is the whole reason the create is
    // a capability rather than a mutation.
    expect(connection.callsNamed("customDomainAdd")[0]).not.toContain("token:");
  });

  // THE SERVER'S SENTENCE, VERBATIM. The three guards a browser cannot mirror
  // -- the cluster's own domain, a collision, the per-site maximum -- name the
  // colliding row and the rule, and a friendlier paraphrase would drop the one
  // fact that helps.
  it("renders a guard refusal verbatim, in surface", async () => {
    const refusal =
      'v1:platform:customDomain: "shop.memql.example.com" is under this cluster\'s own domain';
    const connection = fakeConnection({ sites: [SHOP], domains: [], addDomainError: refusal });
    await openShop(connection);

    await screen.findByText("Domains");
    await openAddDomain();
    await type(screen.getByLabelText("Domain to bind") as HTMLInputElement, "shop.memql.example.com");
    await click(screen.getByRole("button", { name: /add domain/i }));

    await screen.findByText(refusal);
    // THE ACT KEEPS ITS NAME. The control said "Add domain", so the refusal
    // says it was not added -- and the bar says the same, with what to do.
    expect(screen.getByText(/was not added/i)).toBeTruthy();
    const bar = document.querySelector(".os-actbar") as HTMLElement;
    expect(within(bar).getByText("Not added")).toBeTruthy();
    // Nothing was written, so the step is still the name and Cancel is still Cancel.
    expect(screen.getByLabelText("Domain to bind")).toBeTruthy();
    expect(within(bar).getByRole("button", { name: "Cancel" })).toBeTruthy();
  });

  // NOTHING IS INSERTED LOCALLY. The row arrives on its own broadcast, with
  // the arrival cue, exactly like one somebody else created.
  it("inserts no row of its own on success", async () => {
    const connection = fakeConnection({ sites: [SHOP], domains: [] });
    await openShop(connection);

    await screen.findByText("Domains");
    await openAddDomain();
    await type(screen.getByLabelText("Domain to bind") as HTMLInputElement, "www.acme.com");
    await click(screen.getByRole("button", { name: /add domain/i }));

    // THE WIZARD MOVES ON with the server's own answer -- the token it minted
    // is the ownership record, on screen the instant the bind returns.
    await screen.findByLabelText("Copy value: tok-minted-server-side");
    // The LIST still has nothing until the cluster says so.
    await click(screen.getByRole("button", { name: "Leave" }));
    const list = await screen.findByRole("region", { name: /^Domains for / });
    expect(within(list).queryByRole("button", { name: opens("www.acme.com") })).toBeNull();
  });
});

// ===========================================================================
// The add wizard -- one flow from the name to the moment it serves
// ===========================================================================

describe("the add wizard", () => {
  const stepStates = () =>
    [...screen.getByRole("list", { name: "Adding a domain" }).querySelectorAll(":scope > li")].map((li) => li.getAttribute("data-state"));
  const bar = () => document.querySelector(".os-actbar") as HTMLElement;

  it("opens on the name, with every later step ahead and no forward act yet", async () => {
    await openShop(fakeConnection({ sites: [SHOP], domains: [] }));
    await openAddDomain();
    expect(screen.getByRole("heading", { name: "Add a domain" })).toBeTruthy();
    expect(stepStates()).toEqual(["open", "ahead", "ahead", "ahead", "ahead"]);
    // ABSENT, NEVER DISABLED: the bar says what is missing instead.
    expect(within(bar()).getByText("Name the domain")).toBeTruthy();
    expect(within(bar()).queryByRole("button", { name: "Add domain" })).toBeNull();
    expect(within(bar()).getByRole("button", { name: "Cancel" })).toBeTruthy();
  });

  it("offers the forward act only for a name that could be a domain", async () => {
    await openShop(fakeConnection({ sites: [SHOP], domains: [] }));
    await openAddDomain();
    await type(screen.getByLabelText("Domain to bind") as HTMLInputElement, "acme");
    expect(within(bar()).queryByRole("button", { name: "Add domain" })).toBeNull();
    expect(within(bar()).getByText(/needs at least one dot/)).toBeTruthy();
    await type(screen.getByLabelText("Domain to bind") as HTMLInputElement, "www.acme.com");
    expect(within(bar()).getByRole("button", { name: "Add domain" })).toBeTruthy();
    expect(within(bar()).getByText("Ready to add")).toBeTruthy();
  });

  it("keeps the forward act on the bar and nowhere in the step", async () => {
    await openShop(fakeConnection({ sites: [SHOP], domains: [] }));
    await openAddDomain();
    await type(screen.getByLabelText("Domain to bind") as HTMLInputElement, "www.acme.com");
    expect(screen.getAllByRole("button", { name: "Add domain" })).toHaveLength(1);
    expect(bar().contains(screen.getByRole("button", { name: "Add domain" }))).toBe(true);
  });

  it("turns the name into a fact and waits, visibly, once it is bound", async () => {
    await openShop(fakeConnection({ sites: [SHOP], domains: [] }));
    await openAddDomain();
    await type(screen.getByLabelText("Domain to bind") as HTMLInputElement, "www.acme.com");
    await click(screen.getByRole("button", { name: "Add domain" }));

    await waitFor(() => expect(stepStates()).toEqual(["done", "current", "waiting", "ahead", "ahead"]));
    // The name is answered: no field, and its step is a line, not a control.
    expect(screen.queryByLabelText("Domain to bind")).toBeNull();
    expect(screen.queryByRole("button", { name: /^Domain/ })).toBeNull();
    // THE WAIT IS THE BAR'S, in the busy tone that draws the thread, and it
    // says when the cluster last looked.
    expect(bar().getAttribute("data-tone")).toBe("busy");
    expect(within(bar()).getByText("Waiting for the ownership record")).toBeTruthy();
    expect(within(bar()).getByText("not checked yet")).toBeTruthy();
    // LEAVE IS THE BUTTON NOW: going keeps it. Cancel -- which would REMOVE it
    // -- waits the beat until the cluster has sent the row a removal is asked
    // about; this harness never sends one, so Leave is the whole floor.
    expect(within(bar()).getAllByRole("button").map((b) => b.textContent)).toEqual(["Leave"]);
  });

  // FOUND BY BINDING A REAL DOMAIN. The wizard sat on "not checked yet" through
  // two sweeps while the list behind it said "checking DNS": it was still
  // drawing the stand-in it builds from the add's reply, because it looked for
  // the arrived row by an id the two seams do not promise to spell alike.
  it("follows the binding's own row once the cluster sends it", async () => {
    const connection = fakeConnection({ sites: [SHOP], domains: [] });
    await openShop(connection);
    await openAddDomain();
    await type(screen.getByLabelText("Domain to bind") as HTMLInputElement, "www.acme.com");
    await click(screen.getByRole("button", { name: "Add domain" }));
    await waitFor(() => expect(within(bar()).getByText("not checked yet")).toBeTruthy());

    await emit(connection, CUSTOM_DOMAIN_CONCEPT, domainRow({
      id: "cd-new", hostname: "www.acme.com", status: "verifying", failureReason: "dns_token_missing",
      failureDetail: "looked up TXT _memql-verify.www.acme.com: no such record", lastCheckedAt: new Date().toISOString(),
    }), "NODE_CREATED");

    await waitFor(() => expect(within(bar()).getByText(/^checked /)).toBeTruthy());
    // The row is here, so the whole thing can be cancelled by name.
    expect(within(bar()).getAllByRole("button").map((b) => b.textContent)).toEqual(["Cancel", "Leave"]);
    expect(within(bar()).getByRole("button", { name: "Cancel adding www.acme.com" })).toBeTruthy();
    // STILL A WAIT, not a failure: the record has simply not been seen yet.
    expect(bar().getAttribute("data-tone")).toBe("busy");
    expect(stepStates()).toEqual(["done", "current", "waiting", "ahead", "ahead"]);
    expect(screen.getByText("looked up TXT _memql-verify.www.acme.com: no such record")).toBeTruthy();
  });

  it("follows it by its name when the two seams spell the id differently", async () => {
    const connection = fakeConnection({ sites: [SHOP], domains: [] });
    await openShop(connection);
    await openAddDomain();
    await type(screen.getByLabelText("Domain to bind") as HTMLInputElement, "www.acme.com");
    await click(screen.getByRole("button", { name: "Add domain" }));
    await waitFor(() => expect(within(bar()).getByText("not checked yet")).toBeTruthy());

    await emit(connection, CUSTOM_DOMAIN_CONCEPT, domainRow({
      id: "some-other-spelling", hostname: "www.acme.com", status: "issuing", lastCheckedAt: new Date().toISOString(),
    }), "NODE_CREATED");

    await waitFor(() => expect(within(bar()).getByText("Getting a certificate")).toBeTruthy());
    expect(stepStates()).toEqual(["done", "done", "done", "current", "ahead"]);
  });

  it("never mistakes a removed binding of the same name for the one it just made", async () => {
    const gone = domainRow({ id: "cd-old", hostname: "www.acme.com", status: "removed", lastCheckedAt: "2026-01-01T00:00:00Z" });
    const connection = fakeConnection({ sites: [SHOP], domains: [gone] });
    await openShop(connection);
    await openAddDomain();
    await type(screen.getByLabelText("Domain to bind") as HTMLInputElement, "www.acme.com");
    await click(screen.getByRole("button", { name: "Add domain" }));
    // The history row has the name and a check time; the new binding has neither yet.
    await waitFor(() => expect(within(bar()).getByText("not checked yet")).toBeTruthy());
    expect(stepStates()).toEqual(["done", "current", "waiting", "ahead", "ahead"]);
  });

  it("lets the DNS records be read before ownership has passed", async () => {
    await openShop(fakeConnection({ sites: [SHOP], domains: [] }));
    await openAddDomain();
    await type(screen.getByLabelText("Domain to bind") as HTMLInputElement, "www.acme.com");
    await click(screen.getByRole("button", { name: "Add domain" }));
    await screen.findByLabelText("Copy value: tok-minted-server-side");
    // Both records are created in the same place, so the second is one click
    // away while the first is still being checked -- a disclosure, closed.
    const dns = screen.getByRole("button", { name: /^DNS/ });
    expect(dns.getAttribute("aria-expanded")).toBe("false");
    // A stage nobody can act on yet is a name, not a control.
    expect(screen.queryByRole("button", { name: /^Certificate/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Serving/ })).toBeNull();
  });
});

// ===========================================================================
// Remove
// ===========================================================================

describe("leaving a binding that is still being set up", () => {
  it("keeps it, and its row opens the wizard again where it was left", async () => {
    const connection = fakeConnection({ sites: [SHOP], domains: [domainRow({ id: "cd-1", hostname: "www.acme.com", token: "tok-xyz" })] });
    await openShop(connection);
    await findCard("www.acme.com");

    await click(within(floor()).getByRole("button", { name: "Leave" }));
    await findRow("www.acme.com");
    expect(connection.callsNamed("removeCustomDomain")).toHaveLength(0);

    await findCard("www.acme.com");
    expect(screen.getByRole("heading", { name: "Add a domain" })).toBeTruthy();
    expect(setupStates()).toEqual(["done", "current", "waiting", "ahead", "ahead"]);
    expect(within(card("www.acme.com")).getByLabelText("Copy value: tok-xyz")).toBeTruthy();
  });
});

describe("removing a binding", () => {
  // THE FLOOR'S TWO VERBS. Leave goes and everything stays; CANCEL undoes the
  // whole thing, which after the bind means the domain is removed -- so it
  // asks first, by name, in the words of THIS state: nothing is served yet.
  it("cancels one that is still being set up, and goes back to the list", async () => {
    const connection = fakeConnection({ sites: [SHOP], domains: [domainRow({ id: "cd-1", hostname: "www.acme.com" })] });
    await openShop(connection);
    await findCard("www.acme.com");

    // ONE BUTTON ON THE FLOOR. The way out beside it is a label, not a second
    // button to be weighed against the first.
    expect(within(floor()).getAllByRole("button").map((b) => b.textContent)).toEqual(["Cancel", "Leave"]);
    expect(floor().querySelectorAll(".os-button")).toHaveLength(1);
    expect(floor().querySelector(".os-button")?.textContent).toBe("Leave");
    expect(floor().querySelector(".os-actbar-text")?.textContent).toBe("Cancel");

    await click(within(floor()).getByRole("button", { name: "Cancel adding www.acme.com" }));
    expect(within(floor()).getByText("Cancel adding www.acme.com? It is removed, and its setup stops here.")).toBeTruthy();
    expect(within(floor()).getAllByRole("button").map((b) => b.textContent)).toEqual(["Keep", "Remove"]);
    expect(floor().querySelectorAll(".os-button")).toHaveLength(1);

    await click(within(floor()).getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(connection.callsNamed("removeCustomDomain")).toHaveLength(1));
    const list = await screen.findByRole("region", { name: /^Domains for / });
    expect(document.querySelector(".os-domain-page")).toBeNull();

    // THE ITEM LEAVES THE LIST the moment the cluster says it is coming down --
    // not two minutes later when the sweep has tidied up, and not never.
    await emit(connection, CUSTOM_DOMAIN_CONCEPT, domainRow({ id: "cd-1", hostname: "www.acme.com", status: "removing" }));
    await waitFor(() => expect(within(list).queryByText("www.acme.com")).toBeNull());
    expect(within(list).getByText(/Cluster address/)).toBeTruthy();
  });

  it("confirms in surface, naming the hostname, and cancel is a no-op", async () => {
    const connection = fakeConnection({
      sites: [SHOP],
      domains: [domainRow({ id: "cd-1", hostname: "www.acme.com", status: "live" })],
    });
    await openShop(connection);

    await findCard("www.acme.com");
    await click(screen.getByRole("button", { name: "Remove www.acme.com" }));

    // IN SURFACE, never a browser dialog: window.confirm blocks the whole
    // shell and looks like a tab.
    await screen.findByText("Stop serving www.acme.com?");
    await click(screen.getByRole("button", { name: "Keep" }));

    await waitFor(() => expect(screen.queryByText("Stop serving www.acme.com?")).toBeNull());
    expect(connection.callsNamed("removeCustomDomain").length).toBe(0);
  });

  it("writes the removal when confirmed", async () => {
    const connection = fakeConnection({
      sites: [SHOP],
      domains: [domainRow({ id: "cd-1", hostname: "www.acme.com", status: "live" })],
    });
    await openShop(connection);

    await findCard("www.acme.com");
    await click(screen.getByRole("button", { name: "Remove www.acme.com" }));
    await screen.findByText("Stop serving www.acme.com?");
    const confirm = within(card("www.acme.com")).getByRole("button", { name: "Remove" });
    await click(confirm);

    await waitFor(() => {
      const calls = connection.callsNamed("removeCustomDomain");
      expect(calls.length).toBe(1);
      expect(calls[0]).toContain('domainId: "cd-1"');
    });
  });

  // A ROW ALREADY ON THE REMOVAL PATH OFFERS NO SECOND REMOVAL. A quiet one is
  // not listed at all, so there is nothing to press; the one that IS listed --
  // a removal the cluster could not finish -- offers none either, because the
  // sweep is already retrying and a second request would only rewrite the row.
  it("offers no remove on a row that is already on the removal path", async () => {
    const connection = fakeConnection({
      sites: [SHOP],
      domains: [
        domainRow({ id: "cd-1", hostname: "gone.acme.com", status: "removed" }),
        domainRow({ id: "cd-2", hostname: "going.acme.com", status: "removing" }),
        domainRow({ id: "cd-3", hostname: "stuck.acme.com", status: "removing", failureReason: "removal_failed" }),
      ],
    });
    await openShop(connection);

    await findCard("stuck.acme.com");
    expect(screen.queryByRole("button", { name: /^Remove/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Cancel/ })).toBeNull();
    await backToDomains();
    await findRow("stuck.acme.com");
    expect(screen.queryByText("gone.acme.com")).toBeNull();
    expect(screen.queryByText("going.acme.com")).toBeNull();
  });
});
