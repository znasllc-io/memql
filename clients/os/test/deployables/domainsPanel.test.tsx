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

// The Domains panel, through the real LiveCollection and the real generated
// builders (epic memql#4805, task memql#4804).
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
        sectionId="sites"
        navigate={vi.fn()}
        askContext={vi.fn()}
        store={memStore()}
      />,
      { role: opts.role ?? "owner", userId: "u-me" },
    ),
  );
}

/** Opens the deployable's detail, which is where the panel is mounted. */
async function openShop(connection: FakeConnection, opts: { role?: string } = {}) {
  mount(connection, opts);
  await screen.findByText("shop.memql.example.com");
  await click(screen.getByText("shop.memql.example.com"));
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

async function findCard(hostname: string): Promise<HTMLElement> {
  await screen.findByRole("heading", { name: hostname });
  return card(hostname);
}

beforeEach(() => {
  h.connection = null;
});

// ===========================================================================
// The gate
// ===========================================================================

describe("who sees the panel", () => {
  it("renders on the deployable detail for an operator", async () => {
    const connection = fakeConnection({ sites: [SHOP], domains: [domainRow({ id: "cd-1" })] });
    await openShop(connection);

    await screen.findByText("Domains");
    expect(connection.calls).toContain("query customDomainsAll()");
  });

  // PRESENTATION, NEVER THE BOUNDARY. The concept's clusterOwner tier and the
  // three Go guards are the enforcement; hiding the panel from a reader who
  // cannot use it is a courtesy.
  it("renders nowhere for a reader who is not an operator", async () => {
    const connection = fakeConnection({ sites: [SHOP], domains: [domainRow({ id: "cd-1" })] });
    await openShop(connection, { role: "reader" });

    expect(screen.queryByText("Domains")).toBeNull();
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
    expect(within(card("www.acme.com")).getByText("serving")).toBeTruthy();
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
  it("shows both records in the registrar's own vocabulary, each copyable", async () => {
    const connection = fakeConnection({
      sites: [SHOP],
      domains: [domainRow({ id: "cd-1", hostname: "www.acme.com", token: "tok-xyz" })],
    });
    await openShop(connection);

    const c = await findCard("www.acme.com");
    // The registrar's three field names.
    expect(within(c).getAllByText("Type").length).toBe(2);
    expect(within(c).getAllByText("Name").length).toBe(2);
    expect(within(c).getAllByText("Value").length).toBe(2);
    // The ownership record, with the row's own minted token.
    expect(within(c).getByText("_memql-verify.www.acme.com")).toBeTruthy();
    expect(within(c).getByText("tok-xyz")).toBeTruthy();
    // The pointing record: a CNAME, because this is a subdomain.
    expect(within(c).getByText("CNAME")).toBeTruthy();
    expect(within(c).getByText("os.memql.example.com")).toBeTruthy();
    // Each part is its own copy control, because the task is three fields in
    // another application.
    expect(screen.getByLabelText("Copy value: tok-xyz")).toBeTruthy();
    expect(screen.getByLabelText("Copy name: _memql-verify.www.acme.com")).toBeTruthy();
  });

  it("asks an apex for an ALIAS rather than a CNAME", async () => {
    const connection = fakeConnection({
      sites: [SHOP],
      domains: [domainRow({ id: "cd-1", hostname: "acme.com" })],
    });
    await openShop(connection);

    const c = await findCard("acme.com");
    expect(within(c).getByText("ALIAS")).toBeTruthy();
    expect(within(c).queryByText("CNAME")).toBeNull();
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
    expect(screen.getByText(/every couple of minutes, so there is nothing to press/i)).toBeTruthy();
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
