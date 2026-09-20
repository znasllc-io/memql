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
function card(hostname: string): HTMLElement {
  return screen.getByRole("heading", { name: hostname }).closest("article") as HTMLElement;
}

/**
 * A binding is a ROW in the domains list now, and its card is the page that
 * row opens -- so reaching the card means opening the row, which is what a
 * person does too. Already on the card's page: nothing to open.
 */
async function findCard(hostname: string): Promise<HTMLElement> {
  if (screen.queryByRole("heading", { name: hostname }) === null) {
    await click(await screen.findByRole("button", { name: opens(hostname) }));
  }
  await screen.findByRole("heading", { name: hostname });
  return card(hostname);
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

    // On the list there is a row, and the card's own heading is not drawn yet:
    // the list and its detail do not share a scroll column (DESIGN.md rule 11).
    await findRow("www.acme.com");
    expect(screen.queryByRole("heading", { name: "www.acme.com" })).toBeNull();
    expect(document.querySelector("article.os-domain")).toBeNull();

    await findCard("www.acme.com");
    expect(document.querySelector("article.os-domain")).not.toBeNull();
    await backToDomains();
    await findRow("www.acme.com");
    expect(document.querySelector("article.os-domain")).toBeNull();
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
    expect(within(list).queryByRole("button", { name: /^Open shop\.memql\.example\.com/ })).toBeNull();
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
    expect(screen.queryByRole("heading", { name: "other.example.net" })).toBeNull();
  });

  // ROWS SURVIVE REMOVAL, and the list keeps them: what a cluster served, and
  // when, is the audit, and a list that hid them would make it a fact only the
  // database remembers.
  it("keeps a removed binding visible with its terminal status", async () => {
    const connection = fakeConnection({
      sites: [SHOP],
      domains: [domainRow({ id: "cd-1", hostname: "gone.acme.com", status: "removed" })],
    });
    await openShop(connection);

    await findCard("gone.acme.com");
    expect(screen.getByText("removed")).toBeTruthy();
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
    expect(within(card("www.acme.com")).getByText("domain ready")).toBeTruthy();
    expect(within(card("www.acme.com")).queryByRole("link", { name: /Open/ })).toBeNull();
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
    // The binding's setup is a page of its own now, not a card in the list it
    // was chosen from -- still inside the Deployables page, and still no modal.
    expect(screen.getByRole("heading", { name: "Domain" })).toBeTruthy();
    expect(within(c).getByLabelText("Copy value: tok-xyz")).toBeTruthy();
    expect(within(c).getByLabelText("Copy name: _memql-verify.www.acme.com")).toBeTruthy();
    expect(within(c).queryByRole("button", { name: /DNS$/ })).toBeNull();
    await emit(connection, CUSTOM_DOMAIN_CONCEPT, domainRow({ id: "cd-1", hostname: "www.acme.com", token: "tok-xyz", status: "verifying", failureReason: "dns_not_pointing" }));
    await waitFor(() => expect(within(c).getByRole("heading", { name: "DNS" })).toBeTruthy());
    await waitFor(() => expect(within(c).getByLabelText("Copy value: 203.0.113.10")).toBeTruthy());
    expect(within(c).getByText("A")).toBeTruthy();
    await click(within(c).getByRole("button", { name: /Ownership$/ }));
    expect(within(c).getByLabelText("Copy value: tok-xyz")).toBeTruthy();
    await click(within(c).getByRole("button", { name: "Continue to DNS" }));
    expect(within(c).getByRole("heading", { name: "DNS" })).toBeTruthy();
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
    await waitFor(() => expect(within(c).getByRole("heading", { name: "Certificate" })).toBeTruthy());
    expect(within(c).getByText("Certificate pending")).toBeTruthy();
    expect(within(c).queryByRole("link", { name: /Open/ })).toBeNull();
    await emit(connection, CUSTOM_DOMAIN_CONCEPT, domainRow({ id: "cd-1", status: "live" }));
    await waitFor(() => expect(within(c).getByRole("link", { name: "Open www.acme.com" })).toBeTruthy());
    await emit(connection, CUSTOM_DOMAIN_CONCEPT, domainRow({ id: "cd-1", status: "verifying", failureReason: "dns_token_missing" }));
    await waitFor(() => expect(within(c).getByRole("heading", { name: "Ownership" })).toBeTruthy());
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
    expect(within(c).getByText(/does not point at this cluster yet/i)).toBeTruthy();
    // THE OBSERVATION, VERBATIM. The typed reason says which record is wrong;
    // this says what is in it, and somebody editing a zone file needs both.
    expect(within(c).getByText(/resolves to 198\.51\.100\.7/)).toBeTruthy();
    expect(within(c).getByText(/Last checked/)).toBeTruthy();
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
    // omission.
    expect(screen.getByText(/Checks run automatically every couple of minutes/i)).toBeTruthy();
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
    await waitFor(() => expect(screen.getByText("checking DNS")).toBeTruthy());
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
    expect(screen.getByText(/was not bound/i)).toBeTruthy();
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

    await screen.findByText(/bound and waiting for its DNS records/i);
    // The list still has nothing until the cluster says so.
    expect(screen.queryByLabelText(/Copy value:/)).toBeNull();
  });
});

// ===========================================================================
// Remove
// ===========================================================================

describe("removing a binding", () => {
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

  it("offers no remove on a row that is already on the removal path", async () => {
    const connection = fakeConnection({
      sites: [SHOP],
      domains: [
        domainRow({ id: "cd-1", hostname: "gone.acme.com", status: "removed" }),
        domainRow({ id: "cd-2", hostname: "going.acme.com", status: "removing" }),
      ],
    });
    await openShop(connection);

    await findCard("gone.acme.com");
    expect(screen.queryByRole("button", { name: "Remove gone.acme.com" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Remove going.acme.com" })).toBeNull();
  });
});
