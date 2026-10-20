import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { ChannelsSettingsView, type ChannelRow } from "../../../src/apps/deployables/pipelines/ChannelsSettings";
import { pipelineFromRow } from "../../../src/apps/deployables/pipelines/rows";
import { pipelineRow } from "./fixtures";
import { readTheBarNow, useFakeDate } from "../../readTheBar";

const channel: ChannelRow = { id: "channel-1", name: "releases", kind: "email", recipients: ["owner@example.test"], secretRef: "", status: "active" };
function props() {
  return { channels: [channel], ready: true, readError: "", pipelines: [pipelineFromRow(pipelineRow({ channelIds: [channel.id] }))], pipelinesReady: true,
    onRetry: vi.fn(), onSave: vi.fn(async () => {}), onArchive: vi.fn(async () => {}), onAllow: vi.fn(async () => {}) };
}
function press(label: string) { readTheBarNow(); fireEvent.click(screen.getByRole("button", { name: label })); }
function open() { fireEvent.click(screen.getByRole("button", { name: "Manage channel releases" })); }

describe("pipeline channel settings", () => {
  useFakeDate();

  it("preserves an unsaved destination through live updates and a refused save, then retries it", async () => {
    const p = props();
    p.onSave.mockRejectedValueOnce(new Error("Destination refused"));
    const view = render(<ChannelsSettingsView {...p} />);
    open();
    fireEvent.change(screen.getByLabelText("Recipients, one address per line"), { target: { value: "new@example.test" } });
    view.rerender(<ChannelsSettingsView {...p} channels={[{ ...channel, recipients: ["elsewhere@example.test"] }]} />);
    press("Save");
    await screen.findByText("Destination refused");
    expect((screen.getByLabelText("Recipients, one address per line") as HTMLTextAreaElement).value).toBe("new@example.test");
    press("Save");
    await waitFor(() => expect(p.onSave).toHaveBeenCalledTimes(2));
    expect(p.onSave.mock.calls[1]).toEqual([expect.objectContaining({ name: "releases", recipients: "new@example.test" })]);
    await screen.findByRole("button", { name: "Manage channel releases" });
  });

  it("makes a pending save single-flight and does not dismiss the form", async () => {
    const p = props();
    let release!: () => void;
    p.onSave.mockImplementation(() => new Promise<void>(resolve => { release = resolve; }));
    render(<ChannelsSettingsView {...p} />);
    open();
    const save = screen.getByRole("button", { name: "Save" });
    readTheBarNow(); fireEvent.click(save); fireEvent.click(save);
    expect(p.onSave).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("button", { name: "Cancel" })).toBeNull();
    expect((screen.getByRole("group", { name: "Delivery" }) as HTMLFieldSetElement).disabled).toBe(true);
    await act(async () => release());
    await screen.findByRole("button", { name: "Manage channel releases" });
  });

  it("keeps a rejected revoke visibly granted and changes only the selected pipeline", async () => {
    const p = props();
    p.onAllow.mockRejectedValueOnce(new Error("Permission change refused"));
    render(<ChannelsSettingsView {...p} />);
    open();
    fireEvent.click(screen.getByRole("checkbox", { name: "acme/shop" }));
    await screen.findByText("Permission change refused");
    expect(p.onAllow).toHaveBeenCalledWith("pl-shop", "releases", false);
    expect((screen.getByRole("checkbox", { name: "acme/shop" }) as HTMLInputElement).checked).toBe(true);
    expect(p.onSave).not.toHaveBeenCalled();
  });

  it("asks before archiving, supports Keep, and explicitly resumes an archived channel", async () => {
    const p = props();
    const view = render(<ChannelsSettingsView {...p} />);
    open(); press("Archive");
    expect(p.onArchive).not.toHaveBeenCalled();
    expect(screen.getByText("Archive this channel?")).toBeTruthy();
    press("Keep");
    expect(screen.queryByText("Archive this channel?")).toBeNull();
    press("Archive"); press("Archive");
    await waitFor(() => expect(p.onArchive).toHaveBeenCalledWith("releases"));
    view.rerender(<ChannelsSettingsView {...p} channels={[{ ...channel, status: "archived" }]} />);
    await screen.findByRole("button", { name: "Manage channel releases" });
    open();
    expect(screen.queryByRole("button", { name: "Archive" })).toBeNull();
    press("Save and resume");
    await waitFor(() => expect(p.onSave).toHaveBeenCalledTimes(1));
  });

  it("does not show an unread channel list as empty and keeps Retry reachable", () => {
    const p = props();
    render(<ChannelsSettingsView {...p} channels={[]} ready={false} readError="Connection lost" />);
    expect(screen.queryByText("No notification channels.")).toBeNull();
    expect(screen.queryByRole("button", { name: "Add notification channel" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(p.onRetry).toHaveBeenCalledTimes(1);
  });
});
