import type { ComponentType, ReactNode } from "react";
import { MachineDeviceArt } from "../src/apps/fleet/machines/DeviceArt";
import { DEVICE_LABELS, deviceKind, type DeviceKind } from "../src/apps/fleet/machines/device";
import { hardwareFrom } from "../src/apps/fleet/machines/hardware";

// `fleet-devices`: every machine drawing Fleet has, side by side, with the
// hardware a machine of that kind reports. The kind under each drawing is the
// one device.ts chose from that report, so a wrong choice shows here as a
// caption that does not match its picture.

const GiB = 1024 ** 3;

const MACHINES: { hardware: Record<string, unknown>; platform: string }[] = [
  { platform: "darwin/arm64", hardware: { chip: "Apple M4 Max", memoryBytes: 36 * GiB, cpuCores: 14, gpu: { name: "Apple M4 Max", backend: "metal", vramBytes: 0 }, device: { vendor: "Apple", model: "MacBook Pro (16-inch, 2024)", formFactor: "laptop" } } },
  { platform: "darwin/arm64", hardware: { chip: "Apple M4", memoryBytes: 32 * GiB, cpuCores: 10, gpu: { name: "Apple M4", backend: "metal", vramBytes: 0 }, device: { vendor: "Apple", model: "iMac (24-inch, 2024)", formFactor: "allInOne" } } },
  { platform: "darwin/arm64", hardware: { chip: "Apple M4 Pro", memoryBytes: 64 * GiB, cpuCores: 14, gpu: { name: "Apple M4 Pro", backend: "metal", vramBytes: 0 }, device: { vendor: "Apple", model: "Mac mini (2024)", formFactor: "mini" } } },
  { platform: "darwin/arm64", hardware: { chip: "Apple M4 Max", memoryBytes: 128 * GiB, cpuCores: 16, gpu: { name: "Apple M4 Max", backend: "metal", vramBytes: 0 }, device: { vendor: "Apple", model: "Mac Studio (2025)", formFactor: "desktop" } } },
  { platform: "darwin/arm64", hardware: { chip: "Apple M2 Ultra", memoryBytes: 192 * GiB, cpuCores: 24, gpu: { name: "Apple M2 Ultra", backend: "metal", vramBytes: 0 }, device: { vendor: "Apple", model: "Mac Pro (2023)", formFactor: "tower" } } },
  { platform: "linux/amd64", hardware: { chip: "Intel Core i9-14900HX", memoryBytes: 64 * GiB, cpuCores: 32, gpu: { name: "NVIDIA GeForce RTX 4090 Laptop GPU", backend: "cuda", vramBytes: 16 * GiB }, device: { vendor: "Micro-Star International Co., Ltd.", model: "Raider GE78 HX 14VIG", formFactor: "laptop" } } },
  { platform: "linux/amd64", hardware: { chip: "AMD Ryzen 9 9950X", memoryBytes: 64 * GiB, cpuCores: 32, gpu: { name: "NVIDIA GeForce RTX 5090", backend: "cuda", vramBytes: 32 * GiB }, device: { vendor: "", model: "", formFactor: "tower" } } },
  { platform: "linux/amd64", hardware: { chip: "AMD Ryzen Threadripper PRO 7985WX", memoryBytes: 256 * GiB, cpuCores: 128, gpu: { name: "NVIDIA RTX PRO 6000 Blackwell Workstation Edition", backend: "cuda", vramBytes: 96 * GiB }, device: { vendor: "Dell Inc.", model: "Precision 7875 Tower", formFactor: "tower" } } },
  { platform: "linux/amd64", hardware: { chip: "Intel Xeon Platinum 8480+", memoryBytes: 2048 * GiB, cpuCores: 224, gpu: { name: "NVIDIA H100 80GB HBM3", backend: "cuda", vramBytes: 80 * GiB }, device: { vendor: "NVIDIA", model: "DGX H100", formFactor: "server" } } },
  { platform: "linux/amd64", hardware: { chip: "Intel Core Ultra 7 155H", memoryBytes: 32 * GiB, cpuCores: 22, device: { vendor: "Dell Inc.", model: "XPS 14 9440", formFactor: "laptop" } } },
  { platform: "linux/amd64", hardware: { chip: "AMD Ryzen 7 8700G", memoryBytes: 32 * GiB, cpuCores: 16, device: { vendor: "", model: "", formFactor: "desktop" } } },
];

/** The sample machine of one kind, for `fleet-equipment-*&device=<kind>`. */
export function deviceSample(kind: string | null): { hardware: Record<string, unknown>; platform: string } | undefined {
  return MACHINES.find(m => deviceKind(hardwareFrom(m.hardware)) === (kind as DeviceKind));
}

function DeviceGallery() {
  return <div className="os-window-content" style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(280px, 1fr))", gap: "28px 32px", alignItems: "end" }}>
    {MACHINES.map((m, i) => {
      const hardware = hardwareFrom(m.hardware);
      // A wrapper per item: `.fleet-device` places itself in the equipment
      // rig's first cell, which in this grid would stack every drawing.
      return <section key={i} aria-label={DEVICE_LABELS[deviceKind(hardware)]}>
        <div className="fleet-device"><MachineDeviceArt hardware={hardware} platform={m.platform} /></div>
        <p className="os-caption" style={{ textAlign: "center", margin: 0 }}>{DEVICE_LABELS[deviceKind(hardware)]}</p>
      </section>;
    })}
  </div>;
}

type Frame = ComponentType<{ fallback: string; children: ReactNode }>;
export function fleetDeviceScenes(Frame: Frame) {
  return {
    "fleet-devices": { render: () => <Frame fallback="Machines"><DeviceGallery /></Frame> },
  };
}
