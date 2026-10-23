package worker

import (
	"path/filepath"
	"strings"
)

// sessionWorkerPaths is what no app session may touch because it is this
// worker's own (appsession.Options.WorkerPaths):
//
//   - the directories of worker.yaml and workers.yaml -- ~/.memql, which
//     holds the worker tokens and policy.yaml, whose apps.allow is the app
//     consent gate;
//   - the state directories;
//   - a --token-file, wherever it lives;
//   - the consent socket, which may live outside ~/.memql
//     (MEMQL_WORKER_CONSENT_SOCKET).
//
// A session refuses a workspace that overlaps one, and its app is told to
// neither read nor write any. Each comes back absolute -- a relative flag is
// read from where the worker started, so that is where it is -- and once.
func sessionWorkerPaths(configPath, workersPath string, stateDirs []string, tokenFile, consentSocket string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		abs, err := filepath.Abs(path)
		if err != nil || seen[abs] {
			return
		}
		seen[abs] = true
		out = append(out, abs)
	}
	for _, config := range []string{configPath, workersPath} {
		if strings.TrimSpace(config) != "" {
			add(filepath.Dir(config))
		}
	}
	for _, dir := range stateDirs {
		add(dir)
	}
	add(tokenFile)
	add(consentSocket)
	return out
}
