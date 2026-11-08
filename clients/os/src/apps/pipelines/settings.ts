import type { OsAppSection } from "../../system/registry";

/** Navigation is shell metadata and must not load the delivery workspace. */
export const PIPELINES_SECTIONS: OsAppSection[] = [
  { id: "overview", name: "Overview" },
  { id: "repositories", name: "Repositories", requires: "app:pipelines/repositories" },
  { id: "changes", name: "Changes", requires: "app:pipelines/changes" },
  { id: "runs", name: "Runs", requires: "app:pipelines/runs" },
  { id: "releases", name: "Releases", requires: "app:pipelines/releases" },
  { id: "logs", name: "Logs", requires: "app:pipelines/logs" },
  { id: "settings", name: "Settings" },
];
