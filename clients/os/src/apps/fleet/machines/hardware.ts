import { formatBytes } from "../../../kit/format";

// What a machine IS, as its cockpit reported it (epic memql#5146, D1).
//
// ===========================================================================
// THIS FILE'S WHOLE JOB IS TELLING THREE ABSENCES APART
// ===========================================================================
// Everything on the Hardware group is a reported fact or the lack of one, and
// there are three different lacks. They lead to different actions and a page
// that renders them alike is worse than one that says nothing:
//
//   NOT REPORTED   the cockpit predates the field. Nothing is wrong, nothing
//                  to fix, and the machine is working normally.
//   UNDER THE FLOOR the machine reported and the answer is no. There is a
//                  fix and it costs money.
//   NOT MEASURED   nobody has probed this model here yet. There is a fix and
//                  it is a button.
//
// The generic rendering collapses all three into a blank or a zero. Keeping
// them apart is the design, so every reader here returns a state rather than a
// value, and the group renders a SENTENCE per state rather than a dash.

/** The accelerator a machine can actually reach. */
export interface MachineGpu {
  name: string;
  vramBytes: number;
  /** metal / cuda / rocm / none -- what the machine can RUN, not what is
   *  installed. A card whose driver does not work reports `none`. */
  backend: string;
}

/** One runtime the machine has, with its version. */
export interface MachineRuntime {
  name: string;
  version: string;
}

export interface MachineHardware {
  /** False when the cockpit has not reported. NOT the same as a machine with
   *  nothing -- see the header. */
  present: boolean;
  chip: string;
  memoryBytes: number;
  gpu: MachineGpu;
  cpuCores: number;
  osVersion: string;
  diskFreeBytes: number;
  runtimes: MachineRuntime[];
  reportedAt: string;
}

const ABSENT: MachineHardware = {
  present: false,
  chip: "",
  memoryBytes: 0,
  gpu: { name: "", vramBytes: 0, backend: "" },
  cpuCores: 0,
  osVersion: "",
  diskFreeBytes: 0,
  runtimes: [],
  reportedAt: "",
};

/**
 * Read `registration.hardware`.
 *
 * PRESENCE IS DECIDED BY CONTENT, never by whether the key existed. A cockpit
 * that sent the object with nothing in it has said as much as one that sent
 * none, and the honest reading of both is silence -- the same rule the Go side
 * applies at the wire.
 */
export function hardwareFrom(row: unknown): MachineHardware {
  if (row === null || typeof row !== "object") return ABSENT;
  const obj = row as Record<string, unknown>;
  const gpuObj = (obj.gpu ?? {}) as Record<string, unknown>;
  const gpu: MachineGpu = {
    name: str(gpuObj.name),
    vramBytes: num(gpuObj.vramBytes),
    backend: str(gpuObj.backend),
  };
  const runtimes: MachineRuntime[] = Array.isArray(obj.runtimes)
    ? obj.runtimes
        .map((item) => {
          const entry = (item ?? {}) as Record<string, unknown>;
          return { name: str(entry.name), version: str(entry.version) };
        })
        .filter((r) => r.name !== "")
    : [];

  const hardware: MachineHardware = {
    present: false,
    chip: str(obj.chip),
    memoryBytes: num(obj.memoryBytes),
    gpu,
    cpuCores: num(obj.cpuCores),
    osVersion: str(obj.osVersion),
    diskFreeBytes: num(obj.diskFreeBytes),
    runtimes,
    reportedAt: str(obj.reportedAt),
  };
  hardware.present =
    hardware.chip !== "" ||
    hardware.memoryBytes > 0 ||
    hardware.cpuCores > 0 ||
    gpu.backend !== "" ||
    gpu.name !== "" ||
    runtimes.length > 0;
  return hardware.present ? hardware : ABSENT;
}

/**
 * The accelerator, in a sentence.
 *
 * `none` says NO ACCELERATOR THE RUNTIMES CAN REACH, and never "no GPU". The
 * machine may well have a card; what it does not have is a driver a runtime
 * can use, and the repair for that lives on the machine rather than in a shop.
 *
 * A unified-memory machine reports zero dedicated VRAM, so the sentence names
 * the POOL rather than the card's own memory -- reading that zero as "a card
 * with no memory" is the mistake that would describe every Apple Silicon
 * machine as having nothing.
 */
export function acceleratorSentence(hw: MachineHardware): string {
  if (!hw.present) return "";
  const name = hw.gpu.name || "The accelerator";
  switch (hw.gpu.backend) {
    case "metal":
      return `${name}, sharing ${formatBytes(hw.memoryBytes)} of unified memory`;
    case "cuda":
      return `${name}, ${formatBytes(hw.gpu.vramBytes)} of dedicated memory`;
    case "rocm":
      return `${name}, ${formatBytes(hw.gpu.vramBytes)} of dedicated memory over ROCm`;
    case "none":
      return "No accelerator the runtimes can reach. Models run on the processor, which works and is slow.";
    default:
      return hw.gpu.name || "Not reported";
  }
}

/**
 * The class line: one word, and the figure that produced it.
 *
 * IT IS THE ONE DERIVED VALUE on a group of reported facts, which is why it
 * sits above them rather than among them. A reader scanning a Facts list is
 * reading what the machine SAID; the class is what the cluster CONCLUDED, and
 * putting it in the same list would make the conclusion look like a report.
 */
export function classSentence(machineClass: string, usableBytes: number): string {
  if (machineClass === "") return "";
  if (machineClass === "unsupported") {
    return `Under the floor for local models, with ${formatBytes(usableBytes)} usable.`;
  }
  return `A ${machineClass} GB machine, with ${formatBytes(usableBytes)} usable.`;
}

/**
 * What the page says when the machine has not reported.
 *
 * It is a SENTENCE and not a dash, because the reader's question is "is
 * something wrong with my machine" and the answer is no. A dash answers
 * nothing and invites them to go looking.
 */
export const NOT_REPORTED =
  "This machine's cockpit has not reported what hardware it has. An older cockpit does not send it, and the machine is working normally -- updating the cockpit is what fills this in.";

/** The floor, in the words the wizard and the catalog both use. */
export const FLOOR_SENTENCE =
  "Local models need Apple Silicon with 16 GB, or a discrete GPU with 8 GB.";

/** A runtime's name as a person writes it. */
export function runtimeLabel(name: string): string {
  switch (name.toLowerCase()) {
    case "ollama":
      return "Ollama";
    case "mlx":
      return "MLX";
    case "whispercpp":
      return "whisper.cpp";
    case "kokoro":
      return "Kokoro";
    case "mflux":
      return "MFLUX";
    case "docker":
      return "Docker";
    case "nemo":
      return "NeMo";
    case "comfyui":
      return "ComfyUI";
    default:
      return name;
  }
}

function str(v: unknown): string {
  return typeof v === "string" ? v.trim() : "";
}

/**
 * A byte count off the wire.
 *
 * A NEGATIVE READS AS ZERO rather than through, for the reason the Go side
 * gives: the alternative on that side wraps to an enormous positive, and here
 * it would render a machine with minus four gigabytes of memory. Neither is a
 * number anybody should see.
 */
function num(v: unknown): number {
  if (typeof v === "number") return Number.isFinite(v) && v > 0 ? v : 0;
  if (typeof v === "string") {
    const parsed = Number(v);
    return Number.isFinite(parsed) && parsed > 0 ? parsed : 0;
  }
  return 0;
}
