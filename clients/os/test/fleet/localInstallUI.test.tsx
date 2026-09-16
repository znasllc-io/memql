import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
vi.mock("../../src/apps/fleet/addMachine/localInstall", async importOriginal => ({
  ...await importOriginal<object>(),
  localCockpitInstall: () => ({ base: "http://127.0.0.1:4330", version: "0.15.0-dev.43ed63b" }),
}));
import { InstallStop } from "../../src/apps/fleet/addMachine/stops/Install";
import { MachineStop } from "../../src/apps/fleet/addMachine/stops/Machine";
import { EMPTY_DRAFT } from "../../src/apps/fleet/addMachine/flow";
import { MachineDetail } from "../../src/apps/fleet/machines/MachineDetail";
import { machineFromRow } from "../../src/apps/fleet/rows";
import { machineRow, withSession } from "./harness";
afterEach(cleanup);

it("makes saved-data removal explicit and describes app permission cleanup without claiming credentials are deleted", () => {
  const machine = machineFromRow(machineRow({ id: "cleanup-machine", name: "Cleanup Mac", platformInfo: { os: "darwin", arch: "arm64" } }));
  const writes = { busyId: "", actionError: "", rename: vi.fn(), setOperatorLabels: vi.fn(), revoke: vi.fn(), setSharing: vi.fn() };
  render(withSession(<MachineDetail machine={machine} writes={writes} now={new Date()} view="details" />));
  fireEvent.click(screen.getByRole("button", { name: "Remove this machine" }));
  const command = () => (screen.getByLabelText("the uninstall command") as HTMLInputElement).value;
  expect(command()).not.toContain("--purge");
  fireEvent.click(screen.getByRole("switch", { name: "Remove saved worker data" }));
  expect(command()).toContain("--purge");
  expect(command()).toContain("--cluster=");
  expect(screen.getByText(/Accessibility and Screen Recording authorizations are reset/)).toBeTruthy();
  expect(screen.getByText(/CLI credentials.*retained in either mode/)).toBeTruthy();
  expect(writes.revoke).not.toHaveBeenCalled();
});

it("puts the frozen local installer command into the actual copy field with the current token and cluster", () => {
  render(<InstallStop draft={{ ...EMPTY_DRAFT, computerUse: true, userLocal: true }} domain="memql.localhost" token="synthetic-ui-test-token" />);
  expect((screen.getByLabelText("the install command") as HTMLInputElement).value).toBe("curl -fsSL http://127.0.0.1:4330/scripts/install/install-mac.sh | MEMQL_INSTALL_RAW_BASE=http://127.0.0.1:4330/scripts/install MEMQL_INSTALL_VERSION=0.15.0-dev.43ed63b MEMQL_INSTALL_ALLOW_LOOPBACK_HTTP=1 bash -s -- --token synthetic-ui-test-token --cluster https://api.memql.localhost --computeruse --user-local --download-base=http://127.0.0.1:4330/releases/download/v0.15.0-dev.43ed63b");
  expect(screen.getByText(/Local test build 0.15.0-dev.43ed63b/)).toBeTruthy();
});

it("makes the required native test choices visible and fixed while retaining optional model setup", () => {
  render(<MachineStop draft={{ ...EMPTY_DRAFT, computerUse: true, userLocal: true }} localTest={{ base: "http://127.0.0.1:4330", version: "0.15.0-dev.43ed63b" }} connected mintError="" onDraft={vi.fn()} onMint={vi.fn()} />);
  for (const name of ["Computer use", "Install for my account only"]) {
    const input = screen.getByRole("switch", { name }) as HTMLInputElement;
    expect(input.checked).toBe(true); expect(input.disabled).toBe(true);
  }
  expect((screen.getByRole("switch", { name: "Run local models" }) as HTMLInputElement).disabled).toBe(false);
});
