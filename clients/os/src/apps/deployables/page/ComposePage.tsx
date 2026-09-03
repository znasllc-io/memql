import { ArrowLeft, Sparkles } from "lucide-react";

import { Button, Caption, Head, Panel } from "../../../kit";
import { ReportView } from "../packages/ReportView";
import { sourceLabel, type DeploymentRow, type PackageRow } from "../packages/rows";
import type { CredentialRow } from "../sources/rows";
import { Rail } from "./Rail";
import { headActionFor, type ComposeInput, type RailProblem, type RailStage } from "./rail";

// The compose reading (epic memql#4885, design D4): the rail as the form.
//
// ===========================================================================
// THE SEAM, AND WHAT FILLS IT
// ===========================================================================
// New deployable opens this in place of the list: the section's Head title
// becomes "New deployable", a quiet Back returns to the list, and beneath the
// Head the same five stops the page reads as facts are rendered as INPUTS --
// Source open first, the rest not reachable yet, the Head's one action
// following the state (Analyze, disabled until the source is complete). What
// each stop asks, the probes it runs and the credential picker are the
// compose task's (memql#4891); this file is the frame those stops mount into,
// and it is deliberately small so that task can read it in one sitting.
//
// A PARKED RUN REOPENS THE SAME READING. A "will serve" row on the list -- an
// app a parked run's report names that has no site yet -- lands here with
// `parked`, and the rail reads Source complete (the source's own label),
// What it is open with the report in place, and the refusal that parked it,
// if any, at the stop it belongs to. The Head reads Deploy, disabled until
// the placements are answered, which is the compose task's Where-it-lives
// stop.

export interface ComposePageProps {
  clusterDomain: string;
  /** Rank >= 200; the app computes it once. */
  canWrite: boolean;
  /** A client's own domain and a CI-pushed source are cluster-owner acts. */
  isClusterOwner: boolean;
  viewerUserId: string;
  /** The caller's credential cards, from the root feed, for the Source stop's picker. */
  credentials: readonly CredentialRow[];
  /** The quiet Back: the list is what this replaced. */
  onBack: () => void;
  onAsk?: (tag: string) => void;
  /** A parked run and its source, when a "will serve" row reopened the reading. */
  parked?: { pkg: PackageRow; run: DeploymentRow };
}

const CHOOSE_SOURCE = "Choose where it comes from";

export function ComposePage(props: ComposePageProps) {
  const { onBack, onAsk, parked } = props;

  const input: ComposeInput = parked
    ? {
        mode: "compose",
        answered: ["source"],
        open: "whatItIs",
        probeReason: "",
        report: parked.run.report,
        problem: problemOf(parked.run),
        answers: { source: sourceLabel(parked.pkg) },
      }
    : { mode: "compose", answered: [], open: "source", probeReason: "", report: null, problem: null };

  const action = headActionFor(
    parked ? { at: "awaiting_confirm", placementsComplete: false } : { at: "composing", sourceComplete: false },
  );

  const stopBody = (stage: RailStage) => {
    if (stage.id === "source" && stage.state === "open") return <Caption>{CHOOSE_SOURCE}</Caption>;
    if (stage.id === "whatItIs" && parked?.run.report) {
      return (
        <div className="os-stop-body">
          <ReportView report={parked.run.report} />
        </div>
      );
    }
    return null;
  };

  return (
    <Panel label="New deployable">
      <Head title="New deployable">
        <Button tone="quiet" onClick={onBack}>
          <ArrowLeft size={13} aria-hidden /> Back
        </Button>
        {onAsk ? (
          <Button onClick={() => onAsk(parked ? `app:deployables package:${parked.pkg.name || parked.pkg.id}` : "app:deployables compose")} ariaLabel="Ask about this deployable">
            <Sparkles size={13} aria-hidden /> Ask
          </Button>
        ) : null}
        {action === null ? null : (
          <Button tone={action.tone} disabled={action.disabled}>
            {action.label}
          </Button>
        )}
      </Head>

      <Rail input={input} stopBody={stopBody} />
    </Panel>
  );
}

/** The refusal that parked the run: its error, or the report's first fatal problem. */
function problemOf(run: DeploymentRow): RailProblem | null {
  if (run.error !== null && run.error.message.trim() !== "") return run.error;
  const fatal = (run.report?.problems ?? []).find((p) => p.fatal);
  return fatal ? { code: fatal.code, message: fatal.message, scope: fatal.scope } : null;
}
