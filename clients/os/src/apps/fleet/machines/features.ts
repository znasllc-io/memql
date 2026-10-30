import { computerUseStatus, type MachineApp, type MachineRow } from "../rows";
import { isWorkerOnline } from "../online";
import { machineModelsFrom } from "./models";

export interface MachineFeatureState { key: string; label: string; answer: string; detail: string }

/** Connection and each capability answer independently; desktop permissions
 * never determine whether models, isolated workbenches or commands are ready. */
export function machineFeatureStates(machine: MachineRow, now: Date): MachineFeatureState[] {
  const online = isWorkerOnline(machine, now) && !machine.revokedAt;
  const available = (present: boolean) => !online ? "Offline" : present ? "Available" : "Not set up";
  const privateFeature = (enabled: boolean | undefined, present: boolean) => enabled === false ? "Disabled" : available(present);
  const desktop = computerUseStatus(machine);
  return [
    { key: "models", label: "Model inference", answer: available(machine.capabilities.includes("MODEL") && machineModelsFrom(machine.reportedLabels).length > 0), detail: "May be shared. Availability is reported by Cockpit; a successful model response verifies execution." },
    { key: "workbench", label: "Fleet workbenches", answer: available(machine.capabilities.includes("WORKBENCH") && (machine.workbenchContract ?? 0) >= 1), detail: "Isolated workspaces on this machine. May be shared; separate from cloud workbenches." },
    { key: "headless", label: "Commands and files", answer: privateFeature(machine.ownerFeatures?.headless, machine.capabilities.includes("HEADLESS")), detail: "Owner only. Each operation still requires the appropriate permission." },
    { key: "apps", label: "CLIs and apps", answer: privateFeature(machine.ownerFeatures?.apps, machine.apps.some(app => app.runnable)), detail: "Owner only. Apps must also be installed, allowed and signed in." },
    { key: "computerUse", label: "Computer use", answer: machine.ownerFeatures?.computerUse === false ? "Disabled" : !online ? "Offline" : desktop.state === "available" ? "Available" : desktop.state === "unknown" ? "Not verified" : "Needs setup", detail: `Owner only. ${desktop.answer}` },
  ];
}

/** Where one app on one machine stands, in the words every Fleet surface uses. Ready is what
 * the machine measured. Only a Cockpit that predates the checks reads Not checked, and it is
 * the one case still used without an answer (the engine's AppInfo.Runnable). */
export function appStatus(machine: MachineRow, app: MachineApp | undefined, now: Date): string {
  if (machine.revokedAt) return "Machine removed";
  if (machine.ownerFeatures?.apps === false) return "Disabled in Settings";
  if (!isWorkerOnline(machine, now)) return "Machine offline";
  if (!machine.appsReported) return "Not reported";
  if (!app) return "Not installed";
  if (!app.allowed) return "Permission needed";
  if (!app.signedIn) return "Sign-in needed";
  if (app.readiness === "out_of_usage") return "Out of usage";
  if (app.readiness === "ready") return "Ready";
  if (app.readiness === "") return "Not checked";
  return app.checkedAt ? "Could not check" : "Checking";
}
