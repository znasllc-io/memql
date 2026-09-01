import { render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { OS_REGISTRY } from "../../src/apps/registry";
import { appById, sectionsForRole } from "../../src/system/registry";
import { RESERVED_LABELS } from "../../src/apps/deployables/hostname";
import {
  NOTE,
  PDF,
  SHOP,
  ZIP,
  click,
  fakeConnection,
  type FakeConnection,
  type,
  withSession,
} from "./harness";

// The write half: who is offered it, what reaches the wire, and what a refusal
// looks like on the screen.

function memStore() {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
  });
}

function mount(
  connection: FakeConnection | null,
  opts: { section?: string; role?: string } = {},
) {
  h.connection = connection;
  return render(
    withSession(
      <DeployablesApp
        sectionId={opts.section ?? "actions"}
        navigate={vi.fn()}
        askContext={vi.fn()}
        store={memStore()}
      />,
      { role: opts.role ?? "owner" },
    ),
  );
}

beforeEach(() => {
  h.connection = null;
});

describe("who is offered the write half", () => {
  const app = appById(OS_REGISTRY, "deployables")!;

  it("hides Actions from a reader and a writer, and shows it to an admin", () => {
    // PACKAGES IS UNGATED, and that is deliberate rather than an omission
    // (epic memql#4794): v1:platform:package declares the composite owner
    // tier, so every signed-in person has packages of their own to read and
    // the ENGINE decides how far the list reaches. Gating the section would
    // hide somebody's own packages from them. Only the write controls inside
    // it are admin+, exactly as Sites gates publishing rather than the list.
    expect(sectionsForRole(app, "reader").map((s) => s.id)).toEqual(["map", "sites", "packages", "settings"]);
    expect(sectionsForRole(app, "writer").map((s) => s.id)).toEqual(["map", "sites", "packages", "settings"]);
    expect(sectionsForRole(app, "admin").map((s) => s.id)).toEqual([
      "map",
      "sites",
      "packages",
      "actions",
      "settings",
    ]);
    expect(sectionsForRole(app, "owner").map((s) => s.id)).toContain("actions");
  });

  it("hides it from an UNRESOLVED session too", () => {
    // "" is not a role we can rank, and a role we cannot rank must not unlock
    // anything -- which is also the right answer while access is still loading.
    expect(sectionsForRole(app, "").map((s) => s.id)).not.toContain("actions");
  });

  it("still admits the app itself to everybody", () => {
    // The concept's composite tier means every signed-in person has
    // deployables of their own; there is nothing to gate at the app level.
    expect(app.roles).toBeUndefined();
  });

  it("offers no publish control on a reader's detail panel", async () => {
    const connection = fakeConnection({ sites: [SHOP] });
    mount(connection, { section: "sites", role: "reader" });
    await click((await screen.findByText("shop.memql.example.com")).closest("button"));
    await screen.findByRole("region", { name: "Deployable shop.memql.example.com" });
    expect(screen.queryByRole("button", { name: "Publish from the Library" })).toBeNull();
  });

  it("offers it to an admin", async () => {
    const connection = fakeConnection({ sites: [SHOP] });
    mount(connection, { section: "sites", role: "admin" });
    await click((await screen.findByText("shop.memql.example.com")).closest("button"));
    expect(await screen.findByRole("button", { name: "Publish from the Library" })).toBeTruthy();
  });
});

describe("creating a deployable", () => {
  async function fillName(value: string) {
    await type(screen.getByLabelText("Name") as HTMLInputElement, value);
  }

  it("sends the composed hostname, a draft status and a placeholder bundle", async () => {
    const connection = fakeConnection();
    mount(connection);
    await fillName("shop");
    await click(screen.getByRole("button", { name: "Create" }));

    const calls = connection.callsNamed("createSite");
    expect(calls).toHaveLength(1);
    // The GENERATED BUILDER ran: this is the MemQL text the engine parses, not
    // the argument object a stubbed method would have recorded.
    expect(calls[0]).toMatch(
      /^mutation createSite\(siteId: "[^"]+", hostname: "shop\.memql\.example\.com", kind: "spa", bundleRef: "blob:\/\/sites\/[^"]+\/pending\/", status: "draft"\)$/,
    );
    // The site id in the call and in the bundle placeholder are the SAME id.
    const [, id] = /siteId: "([^"]+)"/.exec(calls[0]!)!;
    expect(calls[0]).toContain(`bundleRef: "blob://sites/${id}/pending/"`);
  });

  it("sends NO owner -- ownership is stamped from the verified actor", async () => {
    const connection = fakeConnection();
    mount(connection);
    await fillName("shop");
    await click(screen.getByRole("button", { name: "Create" }));
    expect(connection.callsNamed("createSite")[0]).not.toContain("ownerUserId");
  });

  it("previews the hostname before anything is sent", async () => {
    mount(fakeConnection());
    await fillName("shop");
    expect(screen.getByText("shop.memql.example.com")).toBeTruthy();
  });

  it("REFUSES EVERY RESERVED LABEL before the submit, naming the rule", async () => {
    const connection = fakeConnection();
    mount(connection);
    for (const label of RESERVED_LABELS) {
      await fillName(label);
      expect(screen.getByText(new RegExp(`"${label}" is reserved`))).toBeTruthy();
      expect((screen.getByRole("button", { name: "Create" }) as HTMLButtonElement).disabled).toBe(
        true,
      );
    }
    expect(connection.callsNamed("createSite")).toHaveLength(0);
  });

  it("renders the SERVER's refusal verbatim, because it names the rule we cannot mirror", async () => {
    // Cluster-wide uniqueness needs a read this browser is not allowed to make,
    // so a taken name passes here and is refused there.
    const refusal = 'createSite: hostname "shop.memql.example.com" is already claimed by site-other';
    const connection = fakeConnection({ createError: refusal });
    mount(connection);
    await fillName("shop");
    await click(screen.getByRole("button", { name: "Create" }));

    expect(await screen.findByText(refusal)).toBeTruthy();
    expect(screen.getByText("That deployable was not created.")).toBeTruthy();
  });

  it("does NOT insert the row locally -- it arrives on its own event", async () => {
    const connection = fakeConnection({ sites: [] });
    mount(connection, { section: "actions" });
    await fillName("shop");
    await click(screen.getByRole("button", { name: "Create" }));

    await screen.findByText("Created, as a draft.");
    // The Sites section is a different section, but the collection is the app's
    // and holds whatever the cluster sent. Nothing was pushed into it here.
    expect(connection.callsNamed("createSite")).toHaveLength(1);
    expect(connection.calls.filter((c) => c === "query sitesAll()")).toHaveLength(1);
  });

  it("asks a storefront for its store and the NAME of its token secret", async () => {
    const connection = fakeConnection();
    mount(connection);
    await fillName("shop");
    await click(screen.getByRole("radio", { name: /Shopify storefront/ }));

    expect((screen.getByRole("button", { name: "Create" }) as HTMLButtonElement).disabled).toBe(true);
    await type(screen.getByLabelText("Shopify store domain") as HTMLInputElement, "s.myshopify.com");
    await type(
      screen.getByLabelText("Storefront token secret name") as HTMLInputElement,
      "storefront-token",
    );
    await click(screen.getByRole("button", { name: "Create" }));

    const call = connection.callsNamed("createSite")[0] ?? "";
    expect(call).toContain('kind: "shopify_storefront"');
    // The nested object as the engine will PARSE it -- comma-separated members,
    // which is the form a hand-built template gets wrong (it lexes into one
    // identifier and fails at render, having passed both lint and boot).
    expect(call).toContain(
      'binding: {storeDomain: "s.myshopify.com", storefrontTokenRef: "storefront-token"}',
    );
  });

  it("says so rather than guessing when the cluster published no domain", async () => {
    h.connection = fakeConnection();
    render(
      withSession(
        <DeployablesApp
          sectionId="actions"
          navigate={vi.fn()}
          askContext={vi.fn()}
          store={memStore()}
        />,
        { domain: "" },
      ),
    );
    await type(screen.getByLabelText("Name") as HTMLInputElement, "shop");
    expect(screen.getByText(/<name>\.<domain unknown>/)).toBeTruthy();
  });
});

describe("publishing a bundle", () => {
  async function openPicker(connection: FakeConnection) {
    mount(connection, { section: "sites", role: "owner" });
    await click((await screen.findByText("shop.memql.example.com")).closest("button"));
    await click(await screen.findByRole("button", { name: "Publish from the Library" }));
  }

  it("offers only the zips, and reads the Library once", async () => {
    const connection = fakeConnection({ sites: [SHOP], artifacts: [ZIP, PDF, NOTE] });
    await openPicker(connection);

    expect(await screen.findByText("storefront-build.zip")).toBeTruthy();
    // A PDF and a note have no bytes a deployable could serve.
    expect(screen.queryByText("brief.pdf")).toBeNull();
    expect(screen.queryByText("Standup notes")).toBeNull();
    expect(connection.calls.filter((c) => c === "query libraryArtifacts()")).toHaveLength(1);
  });

  it("reads NOTHING from the Library until the picker is opened", async () => {
    const connection = fakeConnection({ sites: [SHOP], artifacts: [ZIP] });
    mount(connection, { section: "sites" });
    await click((await screen.findByText("shop.memql.example.com")).closest("button"));
    await screen.findByRole("region", { name: "Deployable shop.memql.example.com" });
    expect(connection.calls.filter((c) => c === "query libraryArtifacts()")).toHaveLength(0);
  });

  it("publishes the chosen bundle and summarises what the cluster did", async () => {
    const connection = fakeConnection({ sites: [SHOP], artifacts: [ZIP] });
    await openPicker(connection);
    await click(await screen.findByText("storefront-build.zip"));
    await click(screen.getByRole("button", { name: /^Publish storefront-build\.zip$/ }));

    expect(connection.callsNamed("sitePublishFromArtifact")).toEqual([
      'builtin sitePublishFromArtifact(siteId: "site-shop", artifactId: "artifact-zip")',
    ]);
    expect(await screen.findByText(/Published version v7f3c19a2bb01 -- 12 files, 2\.0 MB\./)).toBeTruthy();
    expect(screen.getByText("blob://sites/site-shop/v7f3c19a2bb01/")).toBeTruthy();
  });

  it("turns a stable refusal reason into a sentence, and never prints the token", async () => {
    const connection = fakeConnection({
      sites: [SHOP],
      artifacts: [ZIP],
      publishError:
        "sitePublishFromArtifact refused: bundle_missing_index -- bundle for site-shop has no index.html at its root",
    });
    await openPicker(connection);
    await click(await screen.findByText("storefront-build.zip"));
    await click(screen.getByRole("button", { name: /^Publish storefront-build\.zip$/ }));

    expect(await screen.findByText(/no index\.html at its top level/)).toBeTruthy();
    expect(screen.getByText(/still serving what it was serving/)).toBeTruthy();
    expect(screen.queryByText(/bundle_missing_index/)).toBeNull();
  });

  it("does NOT retry, and clears the refusal only when a different bundle is chosen", async () => {
    const zip2 = { ...ZIP, id: "artifact-zip-2", title: "second.zip" };
    const connection = fakeConnection({
      sites: [SHOP],
      artifacts: [ZIP, zip2],
      publishError: "sitePublishFromArtifact refused: artifact_not_a_zip -- not a zip",
    });
    await openPicker(connection);
    await click(await screen.findByText("storefront-build.zip"));
    await click(screen.getByRole("button", { name: /^Publish storefront-build\.zip$/ }));
    await screen.findByText(/has to be a zip/);
    expect(connection.callsNamed("sitePublishFromArtifact")).toHaveLength(1);

    await click(screen.getByText("second.zip"));
    await waitFor(() => expect(screen.queryByText(/has to be a zip/)).toBeNull());
    // Still one call: choosing does not publish.
    expect(connection.callsNamed("sitePublishFromArtifact")).toHaveLength(1);
  });

  it("keeps an UNKNOWN failure's own message -- inventing a friendly one hides a fault", async () => {
    const connection = fakeConnection({
      sites: [SHOP],
      artifacts: [ZIP],
      publishError: "stream closed before a reply arrived",
    });
    await openPicker(connection);
    await click(await screen.findByText("storefront-build.zip"));
    await click(screen.getByRole("button", { name: /^Publish storefront-build\.zip$/ }));
    expect(await screen.findByText("stream closed before a reply arrived")).toBeTruthy();
  });

  it("says there may be older bundles when the LIBRARY page was full, not when the zip list is", async () => {
    // The case a row-count condition gets wrong, and the one most likely to
    // matter: fifty Library entries of which two are zips is a full page and a
    // short list. Somebody whose bundle is older than those fifty entries would
    // otherwise be told nothing at all.
    const filler = Array.from({ length: 48 }, (_, i) => ({ ...PDF, id: `pdf-${i}` }));
    const connection = fakeConnection({
      sites: [SHOP],
      artifacts: [ZIP, { ...ZIP, id: "artifact-zip-2", title: "second.zip" }, ...filler],
    });
    await openPicker(connection);
    await screen.findByText("storefront-build.zip");
    expect(screen.getByText(/50 most recent Library entries/)).toBeTruthy();
    // ...and only the two zips are offered.
    expect(screen.getByText("second.zip")).toBeTruthy();
    expect(screen.queryByText("brief.pdf")).toBeNull();
  });

  it("stays quiet when the Library page was not full", async () => {
    const connection = fakeConnection({ sites: [SHOP], artifacts: [ZIP, PDF] });
    await openPicker(connection);
    await screen.findByText("storefront-build.zip");
    expect(screen.queryByText(/most recent Library entries/)).toBeNull();
  });

  it("will not publish before a bundle is chosen", async () => {
    const connection = fakeConnection({ sites: [SHOP], artifacts: [ZIP] });
    await openPicker(connection);
    const button = (await screen.findByRole("button", {
      name: "Pick a bundle",
    })) as HTMLButtonElement;
    expect(button.disabled).toBe(true);
  });
});

describe("errors render where the action is, never as a toast", () => {
  it("puts the create refusal inside the create panel", async () => {
    const connection = fakeConnection({ createError: "createSite: refused" });
    mount(connection);
    await type(screen.getByLabelText("Name") as HTMLInputElement, "shop");
    await click(screen.getByRole("button", { name: "Create" }));

    const panel = await screen.findByRole("region", { name: "Create a deployable" });
    expect(within(panel).getByText("createSite: refused")).toBeTruthy();
  });
});
