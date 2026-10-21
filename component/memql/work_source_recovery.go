package memql

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/znasllc-io/memql/core/common"
)

// These records describe retrieval, not task completion. Only exact tool-source
// pages carry the native digest; ordinary recalled conversation text is excluded.
type workRecoveredSource struct {
	SourceRunID     string `json:"sourceRunId"`
	Checkpoint      string `json:"checkpoint"`
	MessageIndex    int    `json:"messageIndex"`
	Operation       string `json:"operation"`
	Path            string `json:"path,omitempty"`
	SHA256          string `json:"sha256"`
	TotalCharacters int    `json:"totalCharacters"`
	ReadCharacters  int    `json:"readCharacters"`
	Complete        bool   `json:"complete"`
}

type workSourcePage struct {
	SourceRunID     string `json:"sourceRunId"`
	Checkpoint      string `json:"checkpoint"`
	Index           int    `json:"index"`
	Offset          int    `json:"offset"`
	TotalCharacters int    `json:"totalCharacters"`
	Source          string `json:"source"`
	Evidence        struct {
		SHA256    string `json:"sha256"`
		Operation string `json:"operation"`
		Path      string `json:"path"`
	} `json:"sourceEvidence"`
}

// Decode the two recorded tool transports only; do not recursively interpret
// arbitrary strings in a tool's data as more evidence.
func workSourceReadPages(text string) []workSourcePage {
	var envelope struct {
		IsError bool                          `json:"isError"`
		Content []struct{ Type, Text string } `json:"content"`
	}
	if json.Unmarshal([]byte(text), &envelope) != nil || envelope.IsError {
		return nil
	}
	texts := []string{text}
	if envelope.Content != nil {
		texts = nil
		for _, block := range envelope.Content {
			if block.Type == "text" {
				texts = append(texts, block.Text)
			}
		}
	}
	pages := []workSourcePage{}
	for _, text := range texts {
		var value any
		if json.Unmarshal([]byte(text), &value) != nil {
			continue
		}
		for _, row := range MaterializeRows(value) {
			for _, payload := range MaterializeRows(row) {
				if nodePayload, ok := payload["payload"].(map[string]any); ok {
					payload = nodePayload
				}
				raw, _ := json.Marshal(payload["matches"])
				var matches []workSourcePage
				if json.Unmarshal(raw, &matches) == nil {
					pages = append(pages, matches...)
				}
			}
		}
	}
	return pages
}

func workRecoveredSources(checkpoints []map[string]any, remaining *int) ([]workRecoveredSource, bool) {
	type group struct {
		evidence workRecoveredSource
		pages    map[int]string
		invalid  bool
	}
	groups := map[string]*group{}
	order := []string{}
	seen := map[string]bool{}
	omitted, chars := false, 0
	for _, checkpoint := range checkpoints {
		pairs, _ := workCheckpointPairsWithSourceReads(checkpoint, true)
		for _, pair := range pairs {
			if pair.call.Name != "recallWorkHistory" || seen[pair.call.ID] {
				continue
			}
			seen[pair.call.ID] = true
			var args struct {
				SourceRunID  string `json:"sourceRunId"`
				Checkpoint   string `json:"checkpoint"`
				MessageIndex int    `json:"messageIndex"`
			}
			if json.Unmarshal([]byte(pair.call.Arguments), &args) != nil || args.SourceRunID == "" || args.Checkpoint == "" {
				continue
			}
			for _, p := range workSourceReadPages(pair.result.Content) {
				count := utf8.RuneCountInString(p.Source)
				if BareShortId(p.SourceRunID) != BareShortId(args.SourceRunID) || BareShortId(p.Checkpoint) != BareShortId(args.Checkpoint) || p.Index != args.MessageIndex || p.Index%2 != 1 || len(p.Evidence.SHA256) != 64 || p.Evidence.Operation == "" || p.TotalCharacters <= 0 || p.TotalCharacters > 1024*1024 || p.Offset < 0 || count == 0 || p.Offset > p.TotalCharacters || count > p.TotalCharacters-p.Offset {
					continue
				}
				key := fmt.Sprintf("%s/%s/%d/%s", p.SourceRunID, p.Checkpoint, p.Index, p.Evidence.SHA256)
				g := groups[key]
				if g == nil {
					if len(groups) >= 16 {
						omitted = true
						continue
					}
					g = &group{evidence: workRecoveredSource{SourceRunID: p.SourceRunID, Checkpoint: p.Checkpoint, MessageIndex: p.Index, Operation: p.Evidence.Operation, Path: p.Evidence.Path, SHA256: p.Evidence.SHA256, TotalCharacters: p.TotalCharacters}, pages: map[int]string{}}
					groups[key], order = g, append(order, key)
				}
				if old, exists := g.pages[p.Offset]; exists {
					if old != p.Source {
						g.invalid = true
					}
					continue
				}
				if chars+count > 1024*1024 {
					omitted = true
					continue
				}
				chars += count
				g.pages[p.Offset] = p.Source
				g.invalid = g.invalid || g.evidence.TotalCharacters != p.TotalCharacters || g.evidence.Operation != p.Evidence.Operation || g.evidence.Path != p.Evidence.Path
			}
		}
	}
	result := []workRecoveredSource{}
	for _, key := range order {
		g := groups[key]
		offsets := make([]int, 0, len(g.pages))
		for offset := range g.pages {
			offsets = append(offsets, offset)
		}
		slices.Sort(offsets)
		var recovered []rune
		end := 0
		for _, offset := range offsets {
			page := []rune(g.pages[offset])
			g.evidence.ReadCharacters += max(0, offset+len(page)-max(offset, end))
			end = max(end, offset+len(page))
			if offset > len(recovered) {
				continue
			}
			overlap := min(len(page), len(recovered)-offset)
			if string(recovered[offset:offset+overlap]) != string(page[:overlap]) {
				g.invalid = true
			}
			recovered = append(recovered, page[overlap:]...)
		}
		var message common.ChatMessage
		text := string(recovered)
		g.evidence.Complete = !g.invalid && len(recovered) == g.evidence.TotalCharacters && fmt.Sprintf("%x", sha256.Sum256([]byte(text))) == g.evidence.SHA256 && json.Unmarshal([]byte(text), &message) == nil && message.Role == "tool" && message.Name == g.evidence.Operation
		// A source's content is never copied into the review prompt here.
		if len(g.evidence.Path) > 1024 || strings.ContainsRune(g.evidence.Path, 0) {
			g.evidence.Path = ""
		}
		raw, _ := json.Marshal(g.evidence)
		if len(raw) > *remaining {
			omitted = true
			continue
		}
		*remaining -= len(raw)
		result = append(result, g.evidence)
	}
	return result, omitted
}
