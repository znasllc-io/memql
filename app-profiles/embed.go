// Package appprofiles embeds the app-profile markdown files into the
// compiled binary so every node-type (BFF, cognition, agent, planner)
// carries them regardless of filesystem layout. The profiles are
// curated nav + glossary blobs for the products memQL hosts (today
// just "copresent"); they get injected into the agent-reply prompt
// for operator-enabled turns as a compact app map, cutting how far
// the agent has to dig into RAG chunks for basic UI facts.
//
// Why embed vs. COPY in Dockerfile: the previous approach read from
// "./app-profiles/<name>.md" relative to the process's working dir.
// The Dockerfile never COPY'd this directory into the distroless
// runtime image, so every containerised node fell through to an
// empty string and the agent ran with no curated map -- the logs
// showed `appProfile found=false, bytes=0` on every turn. Embedding
// makes the files travel with the binary: no Dockerfile edits, no
// Cloud Run mount, no test-setup symlinks.
package appprofiles

import (
	"embed"
	"strings"
)

//go:embed *.md
var profileFiles embed.FS

// Load returns the embedded profile for the given name ("copresent",
// etc.), or an empty string when no matching file ships with this
// build. Callers treat empty as "no profile available" and proceed
// without it.
func Load(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	data, err := profileFiles.ReadFile(name + ".md")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
