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
      return <svg className="fleet-device-box" viewBox="0 0 54 100" aria-hidden>
        <rect className="dv-body" x="0.5" y="0.5" width="53" height="99" rx="3" />
        <rect className="dv-window" x="6" y="8" width="36" height="84" rx="2" />
        <circle className="dv-line" cx="24" cy="30" r="10" />
        <circle className="dv-line" cx="24" cy="30" r="3" />
        <circle className="dv-line" cx="24" cy="62" r="10" />
        <circle className="dv-line" cx="24" cy="62" r="3" />
        <path className="dv-strip" d="M46.5 12v76" />
        <circle className="dv-glow" cx="47" cy="5" r="1.4" />
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

export function DeviceArt({ kind, children }: { kind: DeviceKind; children: ReactNode }) {
  const label = DEVICE_LABELS[kind];
  const screen = <div className="fleet-device-screen">{children}</div>;
  switch (kind) {
    case "macbook":
    case "laptop":
      return <figure className={`fleet-device-art is-laptop is-${kind}`} aria-label={label}>
        {screen}
        <div className="fleet-device-base" aria-hidden />
      </figure>;
    case "gamingLaptop":
      return <figure className="fleet-device-art is-laptop is-gamingLaptop" aria-label={label}>
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
      return <figure className={`fleet-device-art is-desk is-${kind}`} aria-label={label}>
        <div className="fleet-device-monitor">
          {screen}
          <div className="fleet-device-stand" aria-hidden />
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
