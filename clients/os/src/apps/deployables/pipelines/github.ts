import type { RunRow } from "./rows";

// Where "Open on GitHub" goes (design record D13), pure.
//
// THE CHECK RUN FIRST, because it is what this run reported there -- its
// stage table, its failed steps' last lines -- and GitHub's page for it is
// `/<repository>/runs/<id>`. A run whose check run never landed (an
// installation that refused it, no app) has none, so the pull request it ran
// for, else the commit, is the honest next place.

const GITHUB = "https://github.com";

export function commitUrl(repository: string, sha: string): string {
  return `${GITHUB}/${repository}/commit/${sha}`;
}

export function pullRequestUrl(repository: string, number: number): string {
  return `${GITHUB}/${repository}/pull/${number}`;
}

export function checkRunUrl(repository: string, checkRunId: string): string {
  return `${GITHUB}/${repository}/runs/${checkRunId}`;
}

/** The one place "Open on GitHub" opens, or "" when the run names no repository. */
export function runGithubUrl(run: RunRow): string {
  if (run.repository === "") return "";
  if (run.checkRunState === "written" && /^\d+$/.test(run.checkRunId)) return checkRunUrl(run.repository, run.checkRunId);
  if (run.event === "pull_request" && run.pullRequest > 0) return pullRequestUrl(run.repository, run.pullRequest);
  return run.sha === "" ? `${GITHUB}/${run.repository}` : commitUrl(run.repository, run.sha);
}
