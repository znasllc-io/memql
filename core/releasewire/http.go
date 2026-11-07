// Package releasewire defines the bounded read protocol used by installers
// whose retained credential is a personal access token. PATs belong to the
// Identity HTTP surface; this protocol grants no general query or write port.
package releasewire

const ReadPath = "/release/installation-read"

type Request struct {
	Operation   string `json:"operation"`
	CandidateID string `json:"candidateId,omitempty"`
	Cursor      string `json:"cursor,omitempty"`
	Limit       int    `json:"limit,omitempty"`
	Nonce       string `json:"nonce,omitempty"`
}
