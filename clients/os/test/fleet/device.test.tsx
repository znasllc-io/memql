import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { deviceKind, type DeviceKind } from "../../src/apps/fleet/machines/device";
import { hardwareFrom } from "../../src/apps/fleet/machines/hardware";

afterEach(cleanup);

const GiB = 1024 ** 3;
const kindOf = (hardware: Record<string, unknown>): DeviceKind => deviceKind(hardwareFrom(hardware));
const apple = (model: string, formFactor = "") => ({ chip: "Apple M4 Pro", memoryBytes: 48 * GiB, gpu: { name: "Apple M4 Pro", backend: "metal", vramBytes: 0 }, device: { vendor: "Apple", model, formFactor } });
const pc = (formFactor: string, gpu?: { name: string; vramBytes: number }, chip = "AMD Ryzen 9 9950X") => ({
  chip, memoryBytes: 64 * GiB,
  ...(gpu ? { gpu: { ...gpu, backend: "cuda" } } : {}),
  device: { vendor: "", model: "", formFactor },
});

describe("deviceKind", () => {
  it("draws a Mac as the product line its model names", () => {
    expect(kindOf(apple("MacBook Pro (16-inch, 2024)"))).toBe("macbook");
    expect(kindOf(apple("MacBook Air (15-inch, M4, 2025)"))).toBe("macbook");
    expect(kindOf(apple("iMac (24-inch, 2024)"))).toBe("imac");
    expect(kindOf(apple("Mac mini (2024)"))).toBe("macMini");
    expect(kindOf(apple("Mac Studio (2025)"))).toBe("macStudio");
    expect(kindOf(apple("Mac Pro (2023)"))).toBe("macPro");
  });

  it("falls back to the chassis for a Mac whose model says nothing, and to a MacBook", () => {
    expect(kindOf(apple("", "allInOne"))).toBe("imac");
    expect(kindOf(apple("", "mini"))).toBe("macMini");
    expect(kindOf(apple("", "desktop"))).toBe("macStudio");
    // Every Mac Fleet drew before this was a MacBook; one that reports
    // nothing about itself keeps that picture.
    expect(kindOf({ chip: "Apple M1", memoryBytes: 16 * GiB })).toBe("macbook");
  });

  it("draws a PC from its chassis and its parts", () => {
    expect(kindOf(pc("laptop", { name: "NVIDIA GeForce RTX 4090 Laptop GPU", vramBytes: 16 * GiB }))).toBe("gamingLaptop");
    expect(kindOf(pc("laptop"))).toBe("laptop");
    expect(kindOf(pc("tower", { name: "NVIDIA GeForce RTX 5090", vramBytes: 32 * GiB }))).toBe("tower");
    expect(kindOf(pc("tower", { name: "NVIDIA RTX PRO 6000 Blackwell Workstation Edition", vramBytes: 96 * GiB }))).toBe("workstation");
    expect(kindOf(pc("desktop", undefined, "AMD Ryzen Threadripper PRO 7985WX"))).toBe("workstation");
    expect(kindOf(pc("server", { name: "NVIDIA H100 80GB HBM3", vramBytes: 80 * GiB }))).toBe("server");
    expect(kindOf(pc("desktop"))).toBe("desktop");
  });

  it("reads the GPU when no chassis was reported", () => {
    expect(kindOf({ chip: "Intel Core i9-14900HX", gpu: { name: "NVIDIA GeForce RTX 4080 Laptop GPU", backend: "cuda", vramBytes: 12 * GiB } })).toBe("gamingLaptop");
    expect(kindOf({ chip: "AMD EPYC 9654", gpu: { name: "NVIDIA A100-SXM4-80GB", backend: "cuda", vramBytes: 80 * GiB } })).toBe("server");
    expect(kindOf({ chip: "AMD Ryzen 9 7950X", gpu: { name: "NVIDIA GeForce RTX 4090", backend: "cuda", vramBytes: 24 * GiB } })).toBe("tower");
    // Nothing to go on: the laptop Fleet has always drawn.
    expect(kindOf({ chip: "Intel Core Ultra 7 155H", memoryBytes: 32 * GiB })).toBe("laptop");
    expect(kindOf({})).toBe("laptop");
  });
});

describe("hardwareFrom", () => {
  it("reads the device a cockpit reports, and counts it as a report", () => {
    const hw = hardwareFrom({ device: { vendor: "Apple", model: "Mac mini (2024)", formFactor: "mini" } });
    expect(hw.present).toBe(true);
    expect(hw.device).toEqual({ vendor: "Apple", model: "Mac mini (2024)", formFactor: "mini" });
    expect(hardwareFrom({ chip: "Apple M4" }).device).toEqual({ vendor: "", model: "", formFactor: "" });
  });
});

describe("the Equipment drawing", () => {
  it("draws the machine as its kind, with the reported model on the screen", async () => {
    vi.resetModules();
    const { MachineEquipment } = await import("../../src/apps/fleet/FleetWorkspace");
    const { machineFromRow } = await import("../../src/apps/fleet/rows");
    const { machineRow } = await import("./harness");
    const machine = machineFromRow(machineRow({ id: "mini", hardware: { chip: "Apple M4 Pro", memoryBytes: 64 * GiB, cpuCores: 14, device: { vendor: "Apple", model: "Mac mini (2024)", formFactor: "mini" } } }));
    render(<MachineEquipment machine={machine} now={new Date()} onInspect={vi.fn()} />);
    const drawing = within(screen.getByLabelText("Machine hardware")).getByRole("figure", { name: "Mac mini" });
    expect(within(drawing).getByText("Apple M4 Pro")).toBeTruthy();
    expect(within(drawing).getByText("Mac mini (2024)")).toBeTruthy();
    expect(within(drawing).getByText("64.0 GiB")).toBeTruthy();
  });

  it("keeps the MacBook for a machine that reports no device", async () => {
    vi.resetModules();
    const { MachineEquipment } = await import("../../src/apps/fleet/FleetWorkspace");
    const { machineFromRow } = await import("../../src/apps/fleet/rows");
    const { machineRow } = await import("./harness");
    const machine = machineFromRow(machineRow({ id: "mac", hardware: { chip: "Apple M4 Max", memoryBytes: 36 * GiB, cpuCores: 14 } }));
    render(<MachineEquipment machine={machine} now={new Date()} onInspect={vi.fn()} />);
    expect(within(screen.getByLabelText("Machine hardware")).getByRole("figure", { name: "MacBook" })).toBeTruthy();
  });
});
