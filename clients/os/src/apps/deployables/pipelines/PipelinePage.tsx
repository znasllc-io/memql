import { useEffect, useState } from "react";
import { ListChecks } from "lucide-react";

import { Fact, Facts, Head, Panel } from "../../../kit";
import { ActionBar, type Act } from "../../../kit/ActionBar";
import type { Breadcrumb } from "../../../kit/Breadcrumbs";
import { formatMoment } from "../../../kit/format";
import { useOsConnection } from "../../../live/connection";
import { sourceName } from "../list";
import type { PackageRow } from "../packages/rows";
import { ProblemNotice } from "../packages/ReportView";
import { toneFor } from "../packages/refusals";
import type { PartsHeld } from "../parts";
import { disconnectPipeline, problemFrom, type Problem } from "./calls";
import type { PipelineRow, RunRow } from "./rows";
import { computeShort, computeWords, deliveryShort, deliveryWords } from "./words";
import "./checks.css";

// PIPELINE SETTINGS (design record D14; issue memql#5501): a source's
// pipeline, read and changed -- the page the source's bar opens with
// "Pipeline settings".
//
// THE FACTS ARE WHAT THE PIPELINE IS: the check it writes on GitHub, the
// repository and branch it follows, how a change reaches it, where its steps
// run, which secrets its steps may read, the installation it acts through, and
// when it was connected. What it RUNS is not here -- the stages are the
// manifest's, read at each commit -- and its runs are one click away, on Runs.
//
// THE ACTS FOLLOW THE STATE, IN THE BAR (DESIGN.md rule 12). A pipeline that
// runs offers Disconnect and Change; a disconnected one, Connect again. Change
// and Connect again open the connect rail over this source, prefilled, and
// nothing is written until it is confirmed there. Disconnect asks first, in
// the bar, because it stops checks on every push of the repository.
//
// NOTHING IS FLIPPED HERE. A disconnect the cluster accepted is not yet a
// disconnected pipeline on this page: the row arrives on the root's feed and
// the page re-renders from it. Until it does, the bar says the cluster is
// working rather than drawing a state nobody has read.

export interface PipelinePageProps {
  pkg: PackageRow;
  pipeline: PipelineRow;
  /** The pipeline's runs. Read on Runs, which All runs opens; this page says nothing about them itself. */
  runs: readonly RunRow[];
  can: PartsHeld;
  backLabel: string;
  onBack: () => void;
  /** The trail, ending on this page; without it, the source and "Pipeline". */
  breadcrumbs?: Breadcrumb[];
  /** Reopen the connect rail over this source, prefilled: Change, or Connect again. */
  onChange: () => void;
  /** Open Runs refined to this pipeline. Without it -- Runs not drawn for this person -- there is no All runs. */
  onOpenRuns?: () => void;
}

export function PipelinePage({ pkg, pipeline, can, backLabel, onBack, breadcrumbs, onChange, onOpenRuns }: PipelinePageProps) {
  const connection = useOsConnection();
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  const [sent, setSent] = useState(false);
  const [problem, setProblem] = useState<Problem | null>(null);
  const active = pipeline.status === "active";

  // The wait ends when the row says so, and so does a question the row has
  // answered from elsewhere -- another window disconnecting it. A pipeline
  // that comes back active later, connected again, is a new state rather than
  // the end of this disconnect.
  useEffect(() => {
    if (active) return;
    setSent(false);
    setAsking(false);
  }, [active]);

  async function disconnect() {
    if (!connection) return;
    setBusy(true);
    setProblem(null);
    try {
      await disconnectPipeline(connection.query, pipeline.id);
      setSent(true);
    } catch (err) {
      setProblem(problemFrom(err));
    } finally {
      setBusy(false);
      setAsking(false);
    }
  }

  const waiting = sent && active;
  const bar: { state: string; detail: string; tone: "live" | "busy" | "none"; acts: Act[] } = asking && active
    ? {
        state: "Disconnect this pipeline?",
        detail: "It opens no more runs. Its runs stay as history.",
        tone: "live",
        acts: [
          { label: "Keep", text: true, onAct: () => setAsking(false) },
          { label: "Disconnect", tone: "danger", busy, onAct: () => void disconnect() },
        ],
      }
    : waiting
      ? { state: "Disconnecting", detail: "Waiting for the cluster to confirm.", tone: "busy", acts: [] }
      : active
        ? {
            state: "Checks on",
            detail: `${deliveryShort(pipeline.delivery)}, ${computeShort(pipeline.compute)}`,
            tone: "live",
            acts: can.connect
              ? [
                  { label: "Disconnect", text: true, onAct: () => { setProblem(null); setAsking(true); } },
                  { label: "Change", tone: "primary", onAct: onChange },
                ]
              : [],
          }
        : { state: "Disconnected", detail: "", tone: "none", acts: can.connect ? [{ label: "Connect again", tone: "primary", onAct: onChange }] : [] };

  const label = sourceName(pkg);
  return (
    // THE SOURCE PAGE'S OWN PANE (`deployable-source-view`), as History's is:
    // opened from the source, the page keeps its left edge and its gutters
    // rather than moving the title sideways into a card (DESIGN.md rule 9).
    <div className="os-deploy-pane deployable-source-view pipeline-settings-page" data-os-page-context={JSON.stringify({ page: "Pipeline", packageId: pkg.id, pipelineId: pipeline.id, source: label })}>
      <div className="os-deploy-scroll">
        <Panel label={`Pipeline of ${label}`}>
          <Head title="Pipeline" breadcrumbs={breadcrumbs ?? [{ label: backLabel, onSelect: onBack }, { label: "Pipeline" }]} back={{ label: backLabel, onSelect: onBack }} />
          <Facts>
            <Fact label="Check on GitHub" value={pipeline.name === "" ? "" : `MemQL / ${pipeline.name}`} />
            <Fact
              label="Repository"
              value={pipeline.repository === "" ? "" : <a href={`https://github.com/${pipeline.repository}`} target="_blank" rel="noopener noreferrer">{pipeline.repository}</a>}
            />
            <Fact label="Default branch" value={pipeline.defaultBranch} />
            <Fact label="How changes arrive" value={deliveryWords(pipeline.delivery)} />
            <Fact label="Where steps run" value={computeWords(pipeline.compute)} />
            {/* NAMES, never values: the allowlist of the cluster's secrets the
                steps may resolve. A value has no field to arrive in. */}
            <Fact label="Allowed secrets" value={pipeline.secretNames.length === 0 ? "None" : pipeline.secretNames.join(", ")} mono={pipeline.secretNames.length > 0} />
            <Fact label="Installation" value={pipeline.installationId} mono />
            <Fact label="Connected" value={formatMoment(pipeline.connectedAt)} />
          </Facts>

          {problem !== null ? <ProblemNotice problem={problem} tone={toneFor(problem.code)} /> : null}

          {onOpenRuns ? (
            <button type="button" className="os-deploy-history-line" onClick={onOpenRuns}>
              <ListChecks size={12} aria-hidden />
              <span>All runs</span>
              <span aria-hidden>&#9656;</span>
            </button>
          ) : null}
        </Panel>
      </div>

      <ActionBar state={bar.state} detail={bar.detail} tone={bar.tone} live acts={bar.acts} />
    </div>
  );
}
