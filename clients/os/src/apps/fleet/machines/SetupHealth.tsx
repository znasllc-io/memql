import { useSessionIfPresent } from "../../../chrome/access";
import { CopyField, Notice, type NoticeTone } from "../../../kit";
import { checksFor, STEADY_BEATS, type Check, type Draft } from "../addMachine/flow";
import { setupCommand } from "../addMachine/install";
import { localCockpitInstall } from "../addMachine/localInstall";
import { isRevoked, permissionDecision, type MachineRow } from "../rows";
import { setupNow, setupStage } from "./setup";

// What SETUP could not do on this machine, said where its owner looks
// (memql#6235).
//
// ===========================================================================
// THE WIZARD'S CHECKS, READ AGAIN ONCE THE WIZARD HAS GONE
// ===========================================================================
// Add a machine knows what was asked for and watches each step settle. Once
// it is closed, a permission that was skipped, a download that failed or an
// installer that stopped halfway was visible nowhere: the machine read Online
// and computer use or local models simply did not work. This is the same
// checks, run over what the machine itself reports -- computer use from the
// build it registered, local models and workbenches from the stages its
// installer planned -- so the two surfaces say the same thing in the same
// words, and only what needs a person is shown.
//
// IT RENDERS NOTHING WHEN THERE IS NOTHING TO DO, as CredentialHealth does:
// not while the installer is still working, not for a stage the machine
// cannot run, and not for a fact an older cockpit never reported. A standing
// "setup complete" would be one more green thing on a page whose job is to
// make the one problem findable.
//
// NO DOWNLOAD BUTTON HERE. The wizard's footer act has no place on this page:
// the Models tab installs models, and a model the installer could not fetch
// is retried by the same command that fetched it.

export interface SetupProblem {
  id: Check["id"];
  tone: NoticeTone;
  sentence: string;
  /** What to do, in the wizard's words where it has them. */
  next?: string;
  /** The installer's own words for why, verbatim. */
  detail?: string;
  /** The command that does it, run on the machine. */
  command?: string;
}

/** The draft the wizard would have had, read back from the machine. */
function draftFrom(machine: MachineRow, userLocal: boolean): Draft | null {
  const setup = machine.setup;
  const mac = machine.os === "darwin" || machine.displayServer === "quartz";
  const computerUse = mac && (machine.capabilities.includes("COMPUTERUSE") || machine.buildTag === "computeruse");
  const inference = setupStage(setup, "runtime") !== null || setupStage(setup, "models") !== null;
  const workbench = setupStage(setup, "workbench") !== null;
  if (!computerUse && !inference && !workbench) return null;
  return { name: "", platform: mac ? "mac" : "linux", computerUse, inference, workbench, userLocal };
}

const MODEL_SENTENCE: Record<string, { sentence: string; next: string }> = {
  "Download stopped": { sentence: "The model download stopped before it finished.", next: "Run this on the machine to pick up where it left off." },
  "Download failed": { sentence: "The models could not be downloaded.", next: "Run this on the machine to try again." },
};

export function setupProblems(machine: MachineRow, now: Date, userLocal = false): SetupProblem[] {
  if (isRevoked(machine)) return [];
  const draft = draftFrom(machine, userLocal);
  if (draft === null) return [];
  // Steady by construction: the machine page is never the first seconds
  // after a registration, which is the only time the wizard waits blind.
  const checks = checksFor(draft, machine, STEADY_BEATS, now);
  const problems: SetupProblem[] = [];
  for (const check of checks) {
    const needsPerson = check.state === "open" || check.state === "stopped";
    switch (check.id) {
      case "permissions": {
        if (check.state === "open" && check.footer !== undefined) {
          problems.push({ id: check.id, tone: "info", sentence: "Setup is waiting for you.", next: check.repair });
        } else if (needsPerson || check.state === "skipped") {
          const missing = [
            { name: "Accessibility", state: permissionDecision(machine.permissions, "accessibility") },
            { name: "Screen Recording", state: permissionDecision(machine.permissions, "screenRecording") },
          ].filter((p) => p.state !== "granted").map((p) => p.name);
          problems.push({
            id: check.id,
            tone: "warn",
            sentence: `${missing.join(" and ")} ${missing.length > 1 ? "are" : "is"} not allowed, so computer use cannot work yet.`,
            next: check.repair,
          });
        }
        break;
      }
      case "runtime":
        if (needsPerson) {
          problems.push({
            id: check.id,
            tone: "warn",
            sentence: check.state === "stopped" ? "The model runtime could not be installed." : "The model runtime is not installed.",
            next: check.repair,
            command: check.command,
          });
        }
        break;
      case "models":
        if (!needsPerson) break;
        if (check.act === "pullRecommended") {
          const words = MODEL_SENTENCE[check.answer] ?? { sentence: "No models are installed yet.", next: "Run this on the machine to download the recommended set." };
          problems.push({
            id: check.id,
            tone: "warn",
            sentence: words.sentence,
            next: words.next,
            // A failed download's repair is the installer's own sentence.
            detail: check.answer === "Download failed" ? check.repair : undefined,
            command: setupCommand(userLocal, true),
          });
        } else {
          problems.push({ id: check.id, tone: "warn", sentence: "The models were not downloaded.", next: check.repair });
        }
        break;
      case "workbench":
        if (needsPerson) {
          problems.push({ id: check.id, tone: "warn", sentence: "Fleet workbenches are not set up.", next: check.repair, command: check.command });
        }
        break;
    }
  }
  return problems;
}

/** Whether the machines list says Needs attention: something setup left for
 *  a person, and not an installer that is still at it. */
export function setupNeedsAttention(machine: MachineRow, now: Date): boolean {
  if (setupNow(machine.setup, now)?.state === "running") return false;
  return setupProblems(machine, now).some((p) => p.tone !== "info");
}

const COMMAND_LABEL: Partial<Record<Check["id"], string>> = {
  runtime: "the model runtime command",
  models: "the models command",
  workbench: "the workbench command",
};

export function SetupHealth({ machine, now }: { machine: MachineRow; now: Date }) {
  // Account-only installs are the local test default; the wizard printed the
  // same binary path for this cluster.
  const session = useSessionIfPresent();
  const userLocal = session !== null && localCockpitInstall(session.config.domain) !== null;
  const problems = setupProblems(machine, now, userLocal);
  if (problems.length === 0) return null;
  return (
    <div className="os-fleet-setup-health">
      {problems.map((problem) => (
        <Notice key={problem.id} tone={problem.tone} sentence={problem.sentence} detail={problem.detail}
          next={problem.command === undefined ? problem.next : undefined}>
          {problem.command === undefined ? null : <>
            {problem.next ? <p className="os-caption">{problem.next}</p> : null}
            <CopyField value={problem.command} label={COMMAND_LABEL[problem.id] ?? "the command"} />
          </>}
        </Notice>
      ))}
    </div>
  );
}
