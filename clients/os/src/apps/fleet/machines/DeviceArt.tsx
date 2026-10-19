import { useId, type ReactNode } from "react";
import { Monitor } from "lucide-react";
import { formatBytes } from "../../../kit";
import { DEVICE_LABELS, deviceKind, type DeviceKind } from "./device";
import type { MachineHardware } from "./hardware";

// The machine, drawn: a screen carrying the reported facts, inside the body of
// the kind of machine it is (device.ts). Laptops and the iMac carry their own
// screen; desktops and towers stand beside a monitor; a rack server shows a
// console drawer between its units. Every body is line art in the OS tokens, so
// the drawings follow the theme and the light and dark modes like the rest of
// the page, and only the screen carries text.
//
// A Mac stands beside an Apple display (rounded, on a slim neck and foot). A PC
// stands beside a PC monitor: squarer, with a chin carrying a power light, on a
// V stand. PC laptops take the squarer screen too.

const PC_KINDS: ReadonlySet<DeviceKind> = new Set<DeviceKind>(["laptop", "gamingLaptop", "tower", "workstation", "desktop"]);

/** A Mac mini, Mac Studio, Mac Pro, PC tower, workstation or generic box. */
function Box({ kind }: { kind: DeviceKind }) {
  const lattice = `fleet-device-lattice-${useId().replace(/:/g, "")}`;
  switch (kind) {
    case "macMini":
      return <svg className="fleet-device-box" viewBox="0 0 64 22" aria-hidden>
        <rect className="dv-body" x="0.5" y="2.5" width="63" height="19" rx="6" />
        <path className="dv-line" d="M7 6.5h50" />
        <circle className="dv-glow" cx="55" cy="14" r="1.4" />
      </svg>;
    case "macStudio":
      return <svg className="fleet-device-box" viewBox="0 0 58 40" aria-hidden>
        <rect className="dv-body" x="0.5" y="0.5" width="57" height="39" rx="7" />
        <rect className="dv-slot" x="9" y="23" width="3" height="7" rx="1.5" />
        <rect className="dv-slot" x="15" y="23" width="3" height="7" rx="1.5" />
        <rect className="dv-slot" x="24" y="25" width="11" height="2.5" rx="1.2" />
        <circle className="dv-glow" cx="49" cy="27" r="1.4" />
      </svg>;
    case "macPro":
      return <svg className="fleet-device-box" viewBox="0 0 54 100" aria-hidden>
        <defs>
          <pattern id={lattice} width="5" height="5" patternUnits="userSpaceOnUse">
            <circle className="dv-hole" cx="2.5" cy="2.5" r="1.3" />
          </pattern>
        </defs>
        <rect className="dv-line" x="12" y="0.5" width="7" height="6" rx="3" />
        <rect className="dv-line" x="35" y="0.5" width="7" height="6" rx="3" />
        <rect className="dv-body" x="0.5" y="5.5" width="53" height="94" rx="4" />
        <rect x="5" y="10" width="44" height="85" rx="2" fill={`url(#${lattice})`} />
      </svg>;
    case "tower":
      // A slab with an inset face, intake slits down its edges, a crest and a
      // light along the foot of the face: a boutique tower, not its insides.
      return <svg className="fleet-device-box" viewBox="0 0 56 112" aria-hidden>
        <rect className="dv-foot" x="6" y="106" width="10" height="5" rx="1.5" />
        <rect className="dv-foot" x="40" y="106" width="10" height="5" rx="1.5" />
        <rect className="dv-body" x="0.5" y="0.5" width="55" height="107" rx="5" />
        <rect className="dv-panel" x="5.5" y="5.5" width="45" height="97" rx="3" />
        <path className="dv-slits" d="M10.5 14v78M45.5 14v78" />
        <path className="dv-crest" d="M22.5 18.5l5.5 4.5 5.5-4.5" />
        <circle className="dv-glow" cx="43.5" cy="10" r="1.3" />
        <path className="dv-strip" d="M21 96.5h14" />
      </svg>;
    case "workstation":
      return <svg className="fleet-device-box" viewBox="0 0 58 104" aria-hidden>
        <rect className="dv-body" x="0.5" y="0.5" width="57" height="103" rx="3" />
        <path className="dv-line" d="M7 9h44M7 15h44" />
        <path className="dv-vent" d="M11 26v66M17 26v66M23 26v66M29 26v66M35 26v66M41 26v66M47 26v66" />
        <circle className="dv-glow" cx="50" cy="98" r="1.4" />
      </svg>;
    default:
      return <svg className="fleet-device-box" viewBox="0 0 44 74" aria-hidden>
        <rect className="dv-body" x="0.5" y="0.5" width="43" height="73" rx="3" />
        <path className="dv-line" d="M7 10h30M7 16h30" />
        <circle className="dv-glow" cx="22" cy="62" r="1.6" />
      </svg>;
  }
}

/** One rack unit's face: drive bays and status lights. */
function RackUnit({ bays }: { bays: number }) {
  const width = 200;
  const pitch = 14;
  return <svg className="fleet-device-unit" viewBox={`0 0 ${width} 22`} preserveAspectRatio="none" aria-hidden>
    <rect className="dv-body" x="0.5" y="0.5" width={width - 1} height="21" rx="2" />
    {Array.from({ length: bays }, (_, i) => <rect key={i} className="dv-slot" x={10 + i * pitch} y="6" width={pitch - 4} height="10" rx="1" />)}
    <circle className="dv-glow" cx={width - 22} cy="11" r="1.6" />
    <circle className="dv-dim" cx={width - 14} cy="11" r="1.6" />
  </svg>;
}

/** A PC monitor's V stand: a short neck and two legs splayed to the sides. */
function PcStand() {
  return <svg className="fleet-device-pcstand" viewBox="0 0 120 24" aria-hidden>
    <rect className="dv-body" x="55" y="0.5" width="10" height="15" rx="1" />
    <path className="dv-deck" d="M60 14 L12 21.5 L15 23.5 L60 17.5 L105 23.5 L108 21.5 Z" />
  </svg>;
}

export function DeviceArt({ kind, children }: { kind: DeviceKind; children: ReactNode }) {
  const label = DEVICE_LABELS[kind];
  const pc = PC_KINDS.has(kind) ? " is-pc" : "";
  const screen = <div className="fleet-device-screen">{children}</div>;
  switch (kind) {
    case "macbook":
    case "laptop":
      return <figure className={`fleet-device-art is-laptop is-${kind}${pc}`} aria-label={label}>
        {screen}
        <div className="fleet-device-base" aria-hidden />
      </figure>;
    case "gamingLaptop":
      return <figure className={`fleet-device-art is-laptop is-gamingLaptop${pc}`} aria-label={label}>
        {screen}
        <svg className="fleet-device-deck" viewBox="0 0 240 16" preserveAspectRatio="none" aria-hidden>
          <path className="dv-deck" d="M0.5 0.5h239l-7 15H7.5z" />
          <path className="dv-vent" d="M14 5h26M200 5h26" />
          <path className="dv-strip" d="M76 11.5h88" />
        </svg>
      </figure>;
    case "imac":
      return <figure className="fleet-device-art is-imac" aria-label={label}>
        {screen}
        <div className="fleet-device-stand" aria-hidden />
      </figure>;
    case "server":
      return <figure className="fleet-device-art is-server" aria-label={label}>
        <RackUnit bays={8} />
        {screen}
        <RackUnit bays={8} />
        <RackUnit bays={12} />
      </figure>;
    default:
      return <figure className={`fleet-device-art is-desk is-${kind}${pc}`} aria-label={label}>
        <div className="fleet-device-monitor">
          {screen}
          {pc ? <PcStand /> : <div className="fleet-device-stand" aria-hidden />}
        </div>
        <Box kind={kind} />
      </figure>;
  }
}

/** A machine's drawing with its reported facts on the screen. */
export function MachineDeviceArt({ hardware: hw, platform }: { hardware: MachineHardware; platform: string }) {
  return <DeviceArt kind={deviceKind(hw)}>
    <Monitor size={35} strokeWidth={1.2} aria-hidden />
    <strong>{hw.chip || platform || "Hardware not reported"}</strong>
    {hw.device.model ? <small>{hw.device.model}</small> : null}
    <dl>
      <div><dt>Memory</dt><dd>{hw.memoryBytes > 0 ? formatBytes(hw.memoryBytes) : "Unknown"}</dd></div>
      <div><dt>CPU cores</dt><dd>{hw.cpuCores || "Unknown"}</dd></div>
    </dl>
  </DeviceArt>;
}
