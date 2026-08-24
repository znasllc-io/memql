// The deployables surface (memql#4346), end to end against a fake cluster.
//
// WHAT THIS FILE OWNS. component/memql's Go tests prove the hostname policy and
// the owner stamp are enforced server-side (platform_site_hostname_policy.go +
// site_hostname_policy_db_test.go), integrations/library proves
// sitePublishFromArtifact validates and refuses by name, and the dslconformance
// suite proves the constructs are shaped correctly. None of them can see what
// the browser sends or renders. This file asserts the wire form -- that the UI
// issues the named calls the DSL declares, with the arguments quoted -- and the
// five honesty properties this issue turns on:
//
//   1. THERE IS NO REFUSAL SCREEN ANY MORE. v1:platform:site declares the
//      composite tier, so an ordinary caller has deployables of their own.
//      sites/SitesRefused.tsx said "only the cluster owner may list, create or
//      change them", which is now false, and this file asserts a non-owner sees
//      the working page instead.
//   2. THE COMING-SOON KINDS COME FROM THE PORTAL, NOT THE SCHEMA. The engine
//      pins kind to exactly three values, so an Android entry CANNOT have been
//      read from an enum -- which is what makes the assertion below meaningful
//      rather than a restatement of the fixture.
//   3. THE SLUG IS VALIDATED BEFORE THE CALL. A reserved name must not reach
//      the wire at all, so the create test asserts zero createSite calls, not
//      merely that a message appeared.
//   4. A REFUSAL IS A SENTENCE. sitePublishFromArtifact answers with a stable
//      token; the person must never read it.
//   5. THE ROLLBACK PICKER OFFERS A GENUINELY OLDER ROW. Two identical entries
//      would not exercise the asOf walk at all.
//
// deployableHistory.test.ts covers the version-walk LOGIC in isolation and
// deployablePolicy.test.ts the two pure rule modules; this file covers that the
// screens are wired to them.

import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import {
  Result,
  type AccessSummary,
  type Concept,
  type Connection,
  type QueryClient,
  type Role,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { asQueryClient } from "./support/queryFake";

const SITE = "v1:platform:site";
const ARTIFACT = "v1:library:artifact";
const DOMAIN = "memql.localhost";

// The kind values v1:platform:site actually declares. Pinned engine-side by
// TestSiteKindEnumIsExactlyThreeValues (dsl/platform/concepts.memql), and
// repeated here for ONE purpose: anything the picker offers that is not in this
// set demonstrably did not come from the schema. That is the whole property in
// design D5 -- Android / iOS / macOS get no enum value, ever, because there is
// nothing for the edge to resolve.
const DECLARED_KIND_ENUM = ["spa", "static", "shopify_storefront"];

// The REAL display card from dsl/platform/concepts.memql, because that is what
// RowList composes against -- see components/RowList.tsx's header on why
// nothing here may fork or reimplement that rendering.
const CONCEPTS: Concept[] = [
  {
    id: SITE,
    version: "v1",
    domain: "platform",
    entity: "site",
    type: "concept",
    description: "A web surface this cluster serves at a hostname",
    displayCard: { primary: "hostname", secondary: "kind", tertiary: "title", status: "status" },
  },
];

// Wire-shaped node: payload NESTED alongside the intrinsics, which is what a
// shaped named-query result flattens on the way into a Row.
function node(concept: string, id: string, payload: Record<string, unknown>, createdAt: string): Row {
  return { id, concept, type: "concept", createdBy: "system", createdAt, payload };
}

const PORTAL_ROW = node(
  SITE,
  "site-portal",
  {
    // Cluster-owned: the seeded portal row's ownerUserId is EMPTY, which is the
    // meaningful state rather than a missing value.
    ownerUserId: "",
    hostname: `portal.${DOMAIN}`,
    kind: "spa",
    bundleRef: "file:///app/portal",
    status: "live",
    apiProxy: false,
    systemOwned: true,
    title: "Portal",
  },
  "2026-08-01T00:00:00.000Z",
);

const SHOP_CURRENT = {
  bundleRef: "blob://sites/site-shop/v2abc/",
  createdAt: "2026-08-10T12:00:00.000Z",
  status: "live",
};
const SHOP_PRIOR = {
  bundleRef: "blob://sites/site-shop/v1abc/",
  createdAt: "2026-08-05T09:00:00.000Z",
  status: "live",
};

function shopPayload(version: typeof SHOP_CURRENT): Record<string, unknown> {
  return {
    ownerUserId: "user-1",
    hostname: `shop.${DOMAIN}`,
    kind: "static",
    bundleRef: version.bundleRef,
    status: version.status,
    apiProxy: false,
    systemOwned: false,
    title: "Shop",
  };
}

const SHOP_ROW_CURRENT = node(SITE, "site-shop", shopPayload(SHOP_CURRENT), SHOP_CURRENT.createdAt);

// A storefront, so the detail page's binding band has something to render.
const STORE_ROW = node(
  SITE,
  "site-store",
  {
    ownerUserId: "user-1",
    hostname: `store.${DOMAIN}`,
    kind: "shopify_storefront",
    bundleRef: "blob://sites/site-store/v9/",
    status: "live",
    systemOwned: false,
    title: "Store",
    binding: { storeDomain: "example.myshopify.com", storefrontTokenRef: "shopify-storefront-token" },
  },
  "2026-08-09T00:00:00.000Z",
);

// A deployable created but never deployed to: the state createSite leaves.
const DRAFT_ROW = node(
  SITE,
  "site-draft",
  {
    ownerUserId: "user-1",
    hostname: `blog.${DOMAIN}`,
    kind: "spa",
    bundleRef: "blob://sites/site-draft/pending/",
    status: "draft",
    systemOwned: false,
    title: "Blog",
  },
  "2026-08-11T00:00:00.000Z",
);

// ---- the Library, as the deploy picker sees it -------------------------

const ZIP_ARTIFACT = node(
  ARTIFACT,
  "artifact-zip",
  {
    ownerUserId: "user-1",
    lens: "artifact",
    kind: "file",
    mimeType: "application/zip",
    title: "shop-build.zip",
    sourceConceptRef: "v1:library:file:file-1",
  },
  "2026-08-12T00:00:00.000Z",
);

// A file artifact that is NOT a zip. Present in the Library, absent from the
// picker -- the property that tells a narrowed picker from an unfiltered one.
const PDF_ARTIFACT = node(
  ARTIFACT,
  "artifact-pdf",
  {
    ownerUserId: "user-1",
    lens: "artifact",
    kind: "file",
    mimeType: "application/pdf",
    title: "Q3 budget.pdf",
    sourceConceptRef: "v1:library:file:file-2",
  },
  "2026-08-12T00:00:00.000Z",
);

// A Library row with no file behind it at all.
const NOTE_ARTIFACT = node(
  ARTIFACT,
  "artifact-note",
  {
    ownerUserId: "user-1",
    lens: "record",
    kind: "note",
    title: "Standup notes",
    sourceConceptRef: "v1:library:note:note-1",
  },
  "2026-08-12T00:00:00.000Z",
);

const CLUSTER_CONFIG = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
  domain: DOMAIN,
};

interface Harness {
  query: QueryClient;
  calls: string[];
  callsNamed: (construct: string) => string[];
}

interface HarnessOptions {
  role?: Role;
  sites?: Row[];
  artifacts?: Row[];
  // Fails the next sitePublishFromArtifact with this server message.
  publishError?: string;
}

function harness(overrides: HarnessOptions = {}): Harness {
  const calls: string[] = [];
  const sites = overrides.sites ?? [PORTAL_ROW, SHOP_ROW_CURRENT, STORE_ROW, DRAFT_ROW];
  const artifacts = overrides.artifacts ?? [ZIP_ARTIFACT, PDF_ARTIFACT, NOTE_ARTIFACT];

  const access: AccessSummary = {
    requestId: "r1",
    userId: "user-1",
    primaryEmail: "person@example.com",
    clusterRole: overrides.role ?? "owner",
    sessionId: "",
    displayName: "A Person",
  };

  const executeNamed = vi.fn(async (_name: string, call: string) => {
    calls.push(call);

    if (call === "query sitesAll()") {
      return new Result({ bundle: { nodes: sites }, meta: { cursor: "" } });
    }
    if (call === "query libraryArtifacts()") {
      return new Result({ bundle: { nodes: artifacts }, meta: { cursor: "" } });
    }

    // The rollback walk's asOf-wrapped reads (deployables/calls.ts).
    const asOfMatch = /^asOf\(siteById\(siteId: "([^"]*)"\), "([^"]+)"\)$/.exec(call);
    if (asOfMatch) {
      const [, siteId, at] = asOfMatch;
      if (siteId === "site-shop" && at !== undefined && at < SHOP_CURRENT.createdAt) {
        return new Result({
          bundle: {
            nodes: [node(SITE, "site-shop", shopPayload(SHOP_PRIOR), SHOP_PRIOR.createdAt)],
          },
        });
      }
      return new Result({ bundle: { nodes: [] } });
    }

    // The plain (unwrapped) siteById read -- the current row.
    const plainMatch = /^query siteById\(siteId: "([^"]*)"\)$/.exec(call);
    if (plainMatch) {
      const found = sites.find((row) => row.id === plainMatch[1]);
      return new Result({ bundle: { nodes: found ? [found] : [] } });
    }

    if (call.startsWith("builtin sitePublishFromArtifact(")) {
      if (overrides.publishError !== undefined) throw new Error(overrides.publishError);
      return new Result({
        bundle: {
          nodes: [
            node(
              "v1:library:sitePublishResult",
              "publish-1",
              {
                siteId: "site-shop",
                artifactId: "artifact-zip",
                fileId: "file-1",
                version: "v7f3c19a2bb01",
                bundleRef: "blob://sites/site-shop/v7f3c19a2bb01/",
                fileCount: 12,
                totalBytes: 2097152,
              },
              "2026-08-13T00:00:00.000Z",
            ),
          ],
        },
      });
    }

    // Mutations: nothing reads the reply, so an empty envelope is enough.
    return new Result({ bundle: { nodes: [] }, meta: { cursor: "" } });
  });

  const query = asQueryClient({
    listConcepts: vi.fn(async () => CONCEPTS),
    getMyAccess: vi.fn(async () => access),
    executeNamed,
  });

  return {
    query,
    calls,
    callsNamed: (construct: string) => calls.filter((c) => c.includes(`${construct}(`)),
  };
}

function renderAt(h: Harness, path: string) {
  const dial = vi.fn(
    async () =>
      ({
        nodeId: "bff-test",
        serverVersion: "0.0.0-test",
        query: h.query,
        subscriptions: {
          subscribeGraph: () => () => {},
        },
        dispatcher: { sendAndWait: vi.fn(async () => ({})) },
        close: vi.fn(),
        done: vi.fn(() => new Promise<void>(() => {})),
      }) as unknown as Connection,
  ) as unknown as typeof Connection.dial;

  return render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider
        config={CLUSTER_CONFIG}
        fetchImpl={async () => {
          throw new Error("the deployables tests must make no identity calls");
        }}
        storage={null}
        navigate={() => {}}
        redirectUri="https://api.example.com/portal/auth/callback"
      >
        <ClusterProvider dial={dial}>
          <AppRoutes />
        </ClusterProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
}

describe("the deployables list", () => {
  it("lists what the cluster hosts, through the shared row-list renderer", async () => {
    const h = harness();
    renderAt(h, "/deployables");
    await waitFor(() => expect(screen.getAllByText(`portal.${DOMAIN}`).length).toBeGreaterThan(0));
    expect(screen.getAllByText(`shop.${DOMAIN}`).length).toBeGreaterThan(0);
    // Read through the NAMED query, not the generic browse: sitesAll is what
    // carries isNotDeleted, and deleteSite is a soft delete.
    expect(h.calls).toContain("query sitesAll()");
  });

  it("links each deployable to where it actually is", async () => {
    const h = harness();
    renderAt(h, "/deployables");
    const link = await screen.findByTitle(`Open shop.${DOMAIN}`);
    // https, always. The front door terminates TLS with mkcert locally and a
    // real certificate in the cloud; deriving the scheme from the portal's own
    // origin would only ever be a way to be wrong on a dev server.
    expect(link.getAttribute("href")).toBe(`https://shop.${DOMAIN}/`);
  });

  // The composite tier's second half: a cluster owner's list is EVERY
  // deployable in the cluster, so naming the owner is the difference between
  // "these are yours" and "these are everyone's". For anyone else the column
  // would say the same thing on every row.
  it("names the owner for a cluster owner, and does not for anybody else", async () => {
    const owner = harness({ role: "owner" });
    const asOwner = renderAt(owner, "/deployables");
    await waitFor(() => expect(screen.getAllByText(`shop.${DOMAIN}`).length).toBeGreaterThan(0));
    expect(screen.getAllByTitle("Owner").length).toBeGreaterThan(0);
    // Three of the four fixture rows are user-1's, so this is getAll -- a
    // getBy here would fail on the plural rather than on the property.
    expect(screen.getAllByText("user-1").length).toBe(3);
    // The portal row is cluster-owned (empty ownerUserId), and "cluster" is the
    // honest rendering of that -- blank would read as unknown.
    expect(screen.getByText("cluster")).toBeTruthy();
    asOwner.unmount();

    const writer = harness({ role: "writer" });
    renderAt(writer, "/deployables");
    await waitFor(() => expect(screen.getAllByText(`shop.${DOMAIN}`).length).toBeGreaterThan(0));
    expect(screen.queryByTitle("Owner")).toBeNull();
  });

  // THE change from the Sites screen. The vacuous pass here would be "the page
  // rendered something", so this asserts the working controls are present AND
  // that the old refusal's own words are absent.
  it("shows a non-owner the working page, not a cluster-owner refusal", async () => {
    const h = harness({ role: "writer" });
    renderAt(h, "/deployables");
    await waitFor(() =>
      expect(screen.getByRole("heading", { name: "Deployables", level: 1 })).toBeTruthy(),
    );
    expect(screen.getByRole("button", { name: "Create deployable" })).toBeTruthy();
    expect(screen.queryByText(/cluster-owner surface/)).toBeNull();
    expect(screen.queryByText(/only the cluster owner may list/)).toBeNull();
  });
});

describe("the kind picker", () => {
  it("offers the three live kinds, and the three that are not, disabled", async () => {
    const h = harness();
    renderAt(h, "/deployables");
    const picker = await screen.findByRole("combobox", { name: "Kind" });

    for (const label of ["Single-page app", "Website", "Shopify storefront"]) {
      const option = within(picker).getByRole("option", { name: label }) as HTMLOptionElement;
      expect(option.disabled, `${label} should be selectable`).toBe(false);
      expect(DECLARED_KIND_ENUM).toContain(option.value);
    }

    for (const label of ["Android app", "iOS app", "macOS app"]) {
      const option = within(picker).getByRole("option", {
        name: `${label} — coming soon`,
      }) as HTMLOptionElement;
      expect(option.disabled, `${label} should not be selectable`).toBe(true);
      // THE property (design D5): the entry carries no kind value at all, so it
      // cannot have come from the concept's enum -- there is nothing in the
      // schema to read, and adding one would be a value the edge can never
      // resolve.
      expect(option.value).toBe("");
      expect(DECLARED_KIND_ENUM).not.toContain(option.value);
    }
  });

  it("asks for the storefront's binding only when the kind is a storefront", async () => {
    const h = harness();
    renderAt(h, "/deployables");
    const picker = await screen.findByRole("combobox", { name: "Kind" });

    expect(screen.queryByPlaceholderText("example.myshopify.com")).toBeNull();

    fireEvent.change(picker, { target: { value: "shopify_storefront" } });
    await waitFor(() => expect(screen.getByPlaceholderText("example.myshopify.com")).toBeTruthy());
    expect(screen.getByPlaceholderText("shopify-storefront-token")).toBeTruthy();
    // The control says what it takes: the NAME of a secret, never the token.
    expect(screen.getByText(/NAME of a global secret/)).toBeTruthy();
    expect(screen.getByText(/Never an Admin API token/)).toBeTruthy();
  });
});

describe("creating a deployable", () => {
  it("derives the hostname from the name and the cluster's own domain, before you submit", async () => {
    const h = harness();
    renderAt(h, "/deployables");
    const name = await screen.findByPlaceholderText("shop");
    fireEvent.change(name, { target: { value: "newsite" } });
    await waitFor(() => expect(screen.getByText(`Lives at newsite.${DOMAIN}`)).toBeTruthy());
  });

  // Property 3: a reserved name never reaches the wire. Asserting the message
  // alone would pass for a form that shows a warning and submits anyway.
  it("refuses a reserved name in the form, and makes no call at all", async () => {
    const h = harness();
    renderAt(h, "/deployables");
    const name = await screen.findByPlaceholderText("shop");
    fireEvent.change(name, { target: { value: "identity" } });

    await waitFor(() => expect(screen.getByText(/"identity" is reserved/)).toBeTruthy());
    const create = screen.getByRole("button", { name: "Create deployable" }) as HTMLButtonElement;
    expect(create.disabled).toBe(true);
    fireEvent.click(create);
    expect(h.callsNamed("createSite").length).toBe(0);
  });

  it("refuses a name that is not one label, for the reason the wildcard gives", async () => {
    const h = harness();
    renderAt(h, "/deployables");
    const name = await screen.findByPlaceholderText("shop");
    fireEvent.change(name, { target: { value: "shop.eu" } });
    await waitFor(() => expect(screen.getByText(/One label only/)).toBeTruthy());
    expect(h.callsNamed("createSite").length).toBe(0);
  });

  it("creates a DRAFT at the derived hostname and opens it", async () => {
    const h = harness();
    renderAt(h, "/deployables");
    const name = await screen.findByPlaceholderText("shop");
    fireEvent.change(name, { target: { value: "newsite" } });
    fireEvent.change(screen.getByPlaceholderText("Shop"), { target: { value: "New site" } });
    fireEvent.click(screen.getByRole("button", { name: "Create deployable" }));

    await waitFor(() => expect(h.callsNamed("createSite").length).toBe(1));
    const call = h.callsNamed("createSite")[0] ?? "";
    expect(call.startsWith("mutation createSite(")).toBe(true);
    expect(call).toContain(`hostname: "newsite.${DOMAIN}"`);
    expect(call).toContain('kind: "spa"');
    expect(call).toContain('title: "New site"');
    // A brand-new deployable has nothing published: createSite requires a
    // bundleRef, so it takes the documented placeholder prefix and stays in
    // draft, which the edge answers 404 for BEFORE any file lookup.
    expect(call).toContain('status: "draft"');
    expect(call).toMatch(/bundleRef: "blob:\/\/sites\/[^"]+\/pending\/"/);
    // ownerUserId is STAMPED from the actor server-side and is not an argument
    // -- a caller-supplied owner over a declared owner tier is a guarantee
    // nothing provides.
    expect(call).not.toContain("ownerUserId");

    // Deploying is what anyone does next, so the create lands on the detail
    // page rather than leaving a draft row in a list to be found again.
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Create deployable" })).toBeNull(),
    );
  });

  it("carries the storefront's binding, naming the secret rather than holding it", async () => {
    const h = harness();
    renderAt(h, "/deployables");
    const picker = await screen.findByRole("combobox", { name: "Kind" });
    fireEvent.change(picker, { target: { value: "shopify_storefront" } });
    fireEvent.change(screen.getByPlaceholderText("shop"), { target: { value: "mystore" } });
    await waitFor(() => expect(screen.getByPlaceholderText("example.myshopify.com")).toBeTruthy());
    fireEvent.change(screen.getByPlaceholderText("example.myshopify.com"), {
      target: { value: "example.myshopify.com" },
    });
    fireEvent.change(screen.getByPlaceholderText("shopify-storefront-token"), {
      target: { value: "shopify-storefront-token" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create deployable" }));

    await waitFor(() => expect(h.callsNamed("createSite").length).toBe(1));
    const call = h.callsNamed("createSite")[0] ?? "";
    expect(call).toContain('kind: "shopify_storefront"');
    // renderMemQLValue emits an object as a MemQL literal with BARE keys, sorted
    // (sdk/ts/src/client/memqlValue.ts) -- asserting the composed string is what
    // makes this a wire-form test rather than a restatement of the form state.
    expect(call).toContain(
      'binding: {storeDomain: "example.myshopify.com", storefrontTokenRef: "shopify-storefront-token"}',
    );
  });
});

describe("deploying from the Library", () => {
  it("offers zip bundles only -- not every file, and not every Library row", async () => {
    const h = harness();
    renderAt(h, "/deployables/site-shop");
    const picker = await screen.findByRole("combobox", { name: "Bundle" });
    expect(within(picker).getByRole("option", { name: "shop-build.zip" })).toBeTruthy();
    // A PDF is a file artifact and a note is not a file at all. The capability
    // would refuse both by name; a picker that offered them would be teaching
    // people to discover that the hard way.
    expect(within(picker).queryByRole("option", { name: "Q3 budget.pdf" })).toBeNull();
    expect(within(picker).queryByRole("option", { name: "Standup notes" })).toBeNull();
  });

  it("deploys through sitePublishFromArtifact and names the version it produced", async () => {
    const h = harness();
    renderAt(h, "/deployables/site-shop");
    const picker = await screen.findByRole("combobox", { name: "Bundle" });
    fireEvent.change(picker, { target: { value: "artifact-zip" } });
    fireEvent.click(screen.getByRole("button", { name: "Deploy" }));

    await waitFor(() => expect(h.callsNamed("sitePublishFromArtifact").length).toBe(1));
    expect(h.callsNamed("sitePublishFromArtifact")[0]).toBe(
      'builtin sitePublishFromArtifact(siteId: "site-shop", artifactId: "artifact-zip")',
    );

    // "Deployed" with no version is indistinguishable from "nothing happened",
    // and the version is the handle a rollback names.
    await waitFor(() => expect(screen.getByText("v7f3c19a2bb01")).toBeTruthy());
    expect(screen.getByText(/12 files/)).toBeTruthy();
    expect(screen.getByText("blob://sites/site-shop/v7f3c19a2bb01/")).toBeTruthy();
  });

  // Property 4. The vacuous pass is "an error appeared"; what matters is that
  // the token did NOT.
  it("explains a refusal instead of printing its reason token", async () => {
    const h = harness({
      publishError:
        "sitePublishFromArtifact refused: bundle_missing_index -- bundle for site-shop has no index.html at its root",
    });
    renderAt(h, "/deployables/site-shop");
    const picker = await screen.findByRole("combobox", { name: "Bundle" });
    fireEvent.change(picker, { target: { value: "artifact-zip" } });
    fireEvent.click(screen.getByRole("button", { name: "Deploy" }));

    await waitFor(() => expect(screen.getByText(/no index\.html at its top level/)).toBeTruthy());
    expect(screen.queryByText(/bundle_missing_index/)).toBeNull();
  });

  it("says where a bundle comes from when the Library has none", async () => {
    const h = harness({ artifacts: [NOTE_ARTIFACT] });
    renderAt(h, "/deployables/site-shop");
    await waitFor(() => expect(screen.getByText(/No zip bundles in your Library yet/)).toBeTruthy());
    // Scoped to <main>: the nav rail carries its own Artifacts link, and
    // counting both would make this assertion about the shell.
    const main = screen.getByRole("main");
    expect(within(main).getByRole("link", { name: "Artifacts" })).toBeTruthy();
  });
});

describe("publishing, rolling back and status", () => {
  it("points at a bundle reference through updateSiteBundle", async () => {
    const h = harness();
    renderAt(h, "/deployables/site-shop");
    await waitFor(() => expect(screen.getAllByText(SHOP_CURRENT.bundleRef).length).toBeGreaterThan(0));

    fireEvent.change(screen.getByPlaceholderText(SHOP_CURRENT.bundleRef), {
      target: { value: "blob://sites/site-shop/v3/" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Publish" }));

    await waitFor(() => expect(h.callsNamed("updateSiteBundle").length).toBe(1));
    const call = h.callsNamed("updateSiteBundle")[0] ?? "";
    expect(call).toContain('siteId: "site-shop"');
    expect(call).toContain('bundleRef: "blob://sites/site-shop/v3/"');
  });

  // Property 5: the picker offers a version that is a DIFFERENT row from the
  // current one -- distinct bundleRef, distinct timestamp -- not a second copy
  // of the same data.
  it("offers a prior version distinct from the current one, and rolling back publishes it", async () => {
    const h = harness();
    renderAt(h, "/deployables/site-shop");
    await waitFor(() => expect(screen.getByText(SHOP_PRIOR.bundleRef)).toBeTruthy());
    expect(screen.getByText(SHOP_PRIOR.bundleRef)).not.toBe(
      screen.getAllByText(SHOP_CURRENT.bundleRef)[0],
    );
    expect(screen.getByText(new RegExp(SHOP_PRIOR.createdAt))).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Roll back to this" }));
    await waitFor(() => expect(h.callsNamed("updateSiteBundle").length).toBe(1));
    expect(h.callsNamed("updateSiteBundle")[0]).toContain(`bundleRef: "${SHOP_PRIOR.bundleRef}"`);
  });

  it("changes status through updateSiteStatus", async () => {
    const h = harness();
    renderAt(h, "/deployables/site-shop");
    const status = await screen.findByRole("combobox", { name: "Status" });
    fireEvent.change(status, { target: { value: "disabled" } });

    await waitFor(() => expect(h.callsNamed("updateSiteStatus").length).toBe(1));
    expect(h.callsNamed("updateSiteStatus")[0]).toBe(
      'mutation updateSiteStatus(siteId: "site-shop", status: "disabled")',
    );
  });

  it("says plainly that a draft serves nothing yet", async () => {
    const h = harness();
    renderAt(h, "/deployables/site-draft");
    await waitFor(() => expect(screen.getByText(/This is a draft/)).toBeTruthy());
    expect(screen.getByText(/answers 404/)).toBeTruthy();
  });

  it("shows a storefront's binding, naming the secret and not holding it", async () => {
    const h = harness();
    renderAt(h, "/deployables/site-store");
    await waitFor(() => expect(screen.getByText("example.myshopify.com")).toBeTruthy());
    expect(screen.getByText("shopify-storefront-token")).toBeTruthy();
    expect(screen.getByText(/named here, never stored here/)).toBeTruthy();
  });
});

describe("delete", () => {
  // The UI block is a courtesy -- component/memql/platform_site_delete_guard_test.go
  // proves the SERVER refuses regardless. This proves the courtesy control
  // reflects that: disabled AND explained, not merely present.
  it("disables delete for a systemOwned deployable and says why", async () => {
    const h = harness();
    renderAt(h, "/deployables/site-portal");
    await waitFor(() => expect(screen.getByRole("heading", { name: "Portal" })).toBeTruthy());

    const button = screen.getByRole("button", { name: "Delete deployable" }) as HTMLButtonElement;
    expect(button.disabled).toBe(true);
    expect(screen.getByText(/cannot be deleted/)).toBeTruthy();
    expect(screen.getByText(/re-seeded at boot/)).toBeTruthy();

    fireEvent.click(button);
    expect(h.callsNamed("deleteSite").length).toBe(0);
  });

  it("deletes an ordinary deployable through deleteSite and returns to the list", async () => {
    const h = harness();
    renderAt(h, "/deployables/site-shop");
    await waitFor(() => expect(screen.getByRole("heading", { name: "Shop" })).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Delete deployable" }));
    await waitFor(() => expect(screen.getByRole("dialog")).toBeTruthy());
    fireEvent.click(
      within(screen.getByRole("dialog")).getByRole("button", { name: "Delete deployable" }),
    );

    await waitFor(() => expect(h.callsNamed("deleteSite").length).toBe(1));
    expect(h.callsNamed("deleteSite")[0]).toBe('mutation deleteSite(siteId: "site-shop")');
    await waitFor(() =>
      expect(screen.getByRole("heading", { name: "Deployables", level: 1 })).toBeTruthy(),
    );
  });
});

describe("the retired /sites address", () => {
  it("lands a bookmarked /sites on the deployables list", async () => {
    const h = harness();
    renderAt(h, "/sites");
    await waitFor(() =>
      expect(screen.getByRole("heading", { name: "Deployables", level: 1 })).toBeTruthy(),
    );
    // Redirected, not 404'd -- whoever bookmarked it did nothing wrong.
    expect(screen.queryByText(/Not found/i)).toBeNull();
  });

  // The tail is the half that is easy to drop, and the deep link is the one
  // worth keeping: /sites/:siteId is the page somebody keeps open while rolling
  // a release back.
  it("carries the tail, so a bookmarked site's detail page lands on that deployable", async () => {
    const h = harness();
    renderAt(h, "/sites/site-shop");
    await waitFor(() => expect(screen.getByRole("heading", { name: "Shop" })).toBeTruthy());
    expect(screen.getAllByText(`shop.${DOMAIN}`).length).toBeGreaterThan(0);
  });
});
