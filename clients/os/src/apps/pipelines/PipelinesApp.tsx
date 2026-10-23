import type { OsAppProps, OsAppSection } from "../../system/registry";
import { DeployablesApp } from "../deployables/DeployablesApp";

/** The delivery workspace is generic: workflows and authority arrive from the cluster. */
export const PIPELINES_SECTIONS: OsAppSection[] = [
  { id: "overview", name: "Overview" },
  { id: "repositories", name: "Repositories", requires: "app:pipelines/repositories" },
  { id: "changes", name: "Changes", requires: "app:pipelines/changes" },
  { id: "runs", name: "Runs", requires: "app:pipelines/runs" },
  { id: "releases", name: "Releases", requires: "app:pipelines/releases" },
  { id: "logs", name: "Logs", requires: "app:pipelines/logs" },
  { id: "settings", name: "Settings" },
];

export function PipelinesApp(props: OsAppProps) {
  return <DeployablesApp {...props} surface="pipelines" />;
}
