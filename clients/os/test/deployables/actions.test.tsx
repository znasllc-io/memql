import { render, screen, within } from "@testing-library/react";
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
import { click, fakeConnection, type FakeConnection, type, withSession } from "./harness";

// The write half: who is offered it, what reaches the wire, and what a refusal
// looks like on the screen. Deploying a zip to a deployable is the page's
// Source stop now, and is tested in deployables.test.tsx.

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
