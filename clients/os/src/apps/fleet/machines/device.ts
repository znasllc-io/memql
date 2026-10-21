import type { MachineHardware } from "./hardware";

// Which picture Fleet draws for a machine.
//
// The set is the machines that can run the whole cluster, from
// docs/public/operate/minimum-requirements.md: Apple silicon Macs, and Linux
// on x86_64 with a discrete GPU. The MacBook Air has no picture of its own (it
// is drawn as a MacBook), and arm64 Linux such as DGX Spark has none until
// the installer supports it. Three generic kinds catch everything else.
//
// A PICTURE IS NOT A CLAIM. The kind is chosen from what the machine reported
// about itself (vendor, model, chassis) and, where it said nothing, from its
// chip and GPU. A guess only ever changes the drawing; the facts on the screen
// beside it stay what the machine reported.

export type DeviceKind =
  | "macbook"
  | "imac"
  | "macMini"
  | "macStudio"
  | "macPro"
  | "gamingLaptop"
  | "laptop"
  | "tower"
  | "workstation"
  | "server"
  | "desktop";

export const DEVICE_KINDS: readonly DeviceKind[] = [
  "macbook",
  "imac",
  "macMini",
  "macStudio",
  "macPro",
  "gamingLaptop",
  "laptop",
  "tower",
  "workstation",
  "server",
  "desktop",
];

/** What a person would call each kind, for the drawing's accessible name. */
export const DEVICE_LABELS: Record<DeviceKind, string> = {
  macbook: "MacBook",
  imac: "iMac",
  macMini: "Mac mini",
  macStudio: "Mac Studio",
  macPro: "Mac Pro",
  gamingLaptop: "Laptop with a graphics card",
  laptop: "Laptop",
  tower: "Desktop tower",
  workstation: "Workstation",
  server: "Rack server",
  desktop: "Desktop",
};

// Data-center accelerators live in servers.
const SERVER_GPU = /\b(H100|H200|H800|B100|B200|B300|GB200|A100|A800|A30|A40|L4|L40S?|MI\d{3}X?)\b/i;
// Professional cards and workstation processors live in workstations.
const WORKSTATION_GPU = /\b(RTX PRO|RTX A\d{4}|RTX \d{4} Ada|Quadro|Radeon PRO)\b/i;
const WORKSTATION_CHIP = /\b(Xeon|Threadripper)\b/i;
const SERVER_CHIP = /\bEPYC\b/i;

/** The Apple product line a model name belongs to, when it names one. */
function appleKind(model: string): DeviceKind | undefined {
  const m = model.toLowerCase();
  if (m.includes("macbook")) return "macbook";
  if (m.includes("imac")) return "imac";
  if (m.includes("mac mini")) return "macMini";
  if (m.includes("mac studio")) return "macStudio";
  if (m.includes("mac pro")) return "macPro";
  return undefined;
}

function isApple(hw: MachineHardware): boolean {
  return /^apple\b/i.test(hw.device.vendor) || /^apple\b/i.test(hw.chip) || hw.gpu.backend === "metal";
}

function hasDiscreteGpu(hw: MachineHardware): boolean {
  return hw.gpu.vramBytes > 0 || hw.gpu.backend === "cuda" || hw.gpu.backend === "rocm";
}

/** The desktop kind for a non-Apple machine, from its parts. */
function pcDesktopKind(hw: MachineHardware): DeviceKind {
  if (SERVER_GPU.test(hw.gpu.name) || SERVER_CHIP.test(hw.chip)) return "server";
  if (WORKSTATION_GPU.test(hw.gpu.name) || WORKSTATION_CHIP.test(hw.chip)) return "workstation";
  return hasDiscreteGpu(hw) ? "tower" : "desktop";
}

/** The picture for a machine's reported hardware. */
export function deviceKind(hw: MachineHardware): DeviceKind {
  const { model, formFactor } = hw.device;
  if (isApple(hw)) {
    const named = appleKind(model);
    if (named !== undefined) return named;
    switch (formFactor) {
      case "allInOne":
        return "imac";
      case "mini":
        return "macMini";
      case "desktop":
      case "tower":
        return "macStudio";
      default:
        // Every Mac Fleet has drawn so far was a MacBook, and a Mac that does
        // not say otherwise keeps that picture.
        return "macbook";
    }
  }
  switch (formFactor) {
    case "server":
      return "server";
    case "laptop":
      return hasDiscreteGpu(hw) ? "gamingLaptop" : "laptop";
    case "tower":
    case "desktop":
    case "mini":
    case "allInOne":
      return pcDesktopKind(hw);
  }
  // No chassis reported. NVIDIA names its laptop parts ("... Laptop GPU"), and
  // a data-center or professional card says where it lives.
  if (/laptop gpu/i.test(hw.gpu.name)) return "gamingLaptop";
  if (hasDiscreteGpu(hw)) return pcDesktopKind(hw);
  return "laptop";
}
