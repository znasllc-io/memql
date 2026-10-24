package inference

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// allow.go edits the lists in policy.yaml: models.allow (`setup
// --inference`, `worker models --allow`, the cluster's pull) and each
// cluster's apps.homes.<home>.allow (`worker apps --allow/--deny`). One
// editor, addressed by a key path, because the rules below are the same for
// every list this machine's owner writes consent into.
//
// IT IS A TEXTUAL EDIT, AND THAT IS THE WHOLE DESIGN. The obvious
// implementation -- unmarshal the file, append to a slice, marshal it back
// -- CANNOT meet the requirement, and it fails in a way nobody notices in
// review: yaml.v3 re-serialises the document from its own node tree, which
// re-indents nested sequences, drops blank lines between sections, and
// re-quotes scalars to its own taste. An operator's policy.yaml has
// comments in it and a shape they chose. A command run to add one model
// must not be the thing that reformats the file describing what this
// machine is allowed to do.
//
// So the YAML parse here decides only WHAT to write -- which ids are
// already listed, where the block is, which line the last entry sits on --
// and every byte outside the lines this file inserts or deletes is carried
// through untouched. yaml.Node's Line and Column are what make that exact
// rather than a regular expression's guess at where a block begins.
//
// WHERE IT CANNOT DO THAT, IT REFUSES. A flow mapping, a list key that is
// not a sequence, a root that is not a mapping: each of those could be
// handled by falling back to a re-serialisation, and the price would be an
// operator's comments deleted by a command they ran to add one model. A
// refusal names the line and names the ids, which costs them thirty
// seconds; the fallback costs them the file.
//
// ADDING MERGES, NEVER REPLACES. The ids already listed are the models
// this machine is serving, or the apps a cluster may run, right now, and a
// rewrite that dropped one takes it away at the next reload -- with nothing
// in the output of the command that did it saying so. Removing is its own
// call (RemoveFromList), and only ever removes the ids it was given.

const (
	// policyFileMode is the care worker.yaml gets (persistence.go).
	// policy.yaml holds no secret, but it decides what this machine will
	// execute, serve and upload, and that is not a file to leave writable
	// by anyone who can reach the disk.
	policyFileMode = 0o600
	policyDirMode  = 0o700
)

// ErrPolicyNotEditable is a policy.yaml this command will not rewrite.
// The wrapped message names the line and the ids, so the person can make
// the change by hand in the shape they already chose.
var ErrPolicyNotEditable = errors.New("this policy.yaml is written in a shape this command will not edit without reformatting the whole file")

// modelsAllowPath is where Allow writes.
var modelsAllowPath = []string{"models", "allow"}

// Allow merges model ids into models.allow, creating the file if needed.
//
// Nothing is written when there is nothing to add, which is the ordinary
// second run of `setup --inference`: an idempotent command that rewrote
// the file every time would churn its mtime, its mode and any backup
// watching it for no change at all.
func Allow(policyPath string, ids ...string) error {
	wanted := cleanIDs(ids)
	if len(wanted) == 0 {
		if strings.TrimSpace(policyPath) == "" {
			return errors.New("no policy.yaml path was given, so there is nowhere to record the model")
		}
		return nil
	}
	return EditPolicy(policyPath, func(body string) (string, bool, error) {
		return MergeList(body, modelsAllowPath, wanted)
	})
}

// EditPolicy applies edit to policy.yaml's text and writes the result back
// -- atomically, and only when edit reports a change. A missing file is
// the empty string, which is what a fresh machine has. Several edits that
// must land together compose inside one edit function, so the file is
// never left with half of them.
func EditPolicy(policyPath string, edit func(body string) (string, bool, error)) error {
	if strings.TrimSpace(policyPath) == "" {
		return errors.New("no policy.yaml path was given, so there is nowhere to record the change")
	}
	raw, err := os.ReadFile(policyPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading %s: %w", policyPath, err)
	}
	updated, changed, err := edit(string(raw))
	if err != nil {
		return fmt.Errorf("%s: %w", policyPath, err)
	}
	if !changed {
		return nil
	}
	return writePolicyAtomic(policyPath, []byte(updated))
}

// MergeList returns body with ids merged into the list at path -- for
// example models.allow, or apps.homes.<home>.allow -- and whether anything
// changed. Every mapping on the path that does not exist yet is created,
// nested under the deepest one that does.
//
// A pure function of the bytes, so every shape below is asserted on a
// string in the tests rather than on a file, which is what makes "byte for
// byte outside the list" something a test can actually claim.
func MergeList(body string, path []string, wanted []string) (string, bool, error) {
	if len(path) == 0 {
		return "", false, errors.New("no key path to merge into")
	}
	wanted = cleanIDs(wanted)
	if len(wanted) == 0 {
		return body, false, nil
	}
	lines := splitLines(body)
	root, err := parseRoot(body)
	if err != nil {
		return "", false, err
	}
	if root == nil {
		// Empty, missing, or comments only. The comments survive, because
		// the fresh block is appended to the lines that are there.
		return joinLines(appendBlock(lines, path, wanted, len(lines) == 0)), true, nil
	}
	refuse := func(line int) (string, bool, error) { return notEditable(line, "add", wanted, "to", path) }
	if root.Kind != yaml.MappingNode || root.Style&yaml.FlowStyle != 0 {
		return refuse(root.Line)
	}

	// Walk the mappings on the path, stopping at the first one missing.
	parent, parentKey := root, (*yaml.Node)(nil)
	for i, key := range path[:len(path)-1] {
		k, v := findKey(parent, key)
		switch {
		case k == nil:
			return joinLines(insertUnder(lines, parent, parentKey, path[i:], wanted)), true, nil
		case isNull(v):
			return joinLines(insertLines(lines, k.Line, nestedBlock(k.Column-1+2, path[i+1:], wanted))), true, nil
		case v.Kind != yaml.MappingNode || v.Style&yaml.FlowStyle != 0:
			return refuse(k.Line)
		}
		parent, parentKey = v, k
	}

	listKey := path[len(path)-1]
	k, v := findKey(parent, listKey)
	if k == nil {
		return joinLines(insertUnder(lines, parent, parentKey, path[len(path)-1:], wanted)), true, nil
	}

	add := missing(wanted, sequenceValues(v))
	if len(add) == 0 {
		return body, false, nil
	}

	switch {
	case isNull(v):
		// `allow:` with nothing under it -- what somebody leaves behind
		// after deleting the last entry, and what RemoveFromList leaves.
		return joinLines(insertLines(lines, k.Line, items(k.Column-1+2, add))), true, nil

	case v.Kind == yaml.SequenceNode && v.Style&yaml.FlowStyle != 0:
		m := flowSequenceOn(lines, k.Line, listKey)
		if m == nil {
			return refuse(k.Line)
		}
		quoted := make([]string, 0, len(add))
		for _, id := range add {
			quoted = append(quoted, yamlScalar(id))
		}
		inside := strings.Join(quoted, ", ")
		if strings.TrimSpace(m[2]) != "" {
			inside = strings.TrimRight(m[2], " ") + ", " + inside
		}
		out := append([]string(nil), lines...)
		out[k.Line-1] = m[1] + inside + m[3]
		return joinLines(out), true, nil

	case v.Kind == yaml.SequenceNode:
		last := v.Content[len(v.Content)-1]
		if last.Kind != yaml.ScalarNode || last.Line < 1 || last.Line > len(lines) {
			return refuse(k.Line)
		}
		// The `- ` prefix is COPIED from the last entry rather than
		// rebuilt, so a list indented under its key and one indented level
		// with it both keep the shape they had.
		prefix := blockItemPrefix.FindStringSubmatch(lines[last.Line-1])
		if prefix == nil {
			return refuse(last.Line)
		}
		out := make([]string, 0, len(add))
		for _, id := range add {
			out = append(out, prefix[1]+yamlScalar(id))
		}
		return joinLines(insertLines(lines, last.Line, out)), true, nil
	}
	return refuse(k.Line)
}

// RemoveFromList returns body with ids removed from the list at path, and
// whether anything changed. Ids match without regard to case or
// surrounding space. A list, or any mapping above it, that does not exist
// removes nothing: what is not listed is not allowed.
//
// Only the removed entries' lines go. A list left empty is left as its key
// with nothing under it (or `[]`), which reads as nothing listed -- the
// same default-deny as no key at all, and MergeList fills it back in.
func RemoveFromList(body string, path []string, unwanted []string) (string, bool, error) {
	if len(path) == 0 {
		return "", false, errors.New("no key path to remove from")
	}
	unwanted = cleanIDs(unwanted)
	if len(unwanted) == 0 {
		return body, false, nil
	}
	lines := splitLines(body)
	root, err := parseRoot(body)
	if err != nil {
		return "", false, err
	}
	if root == nil {
		return body, false, nil
	}
	refuse := func(line int) (string, bool, error) { return notEditable(line, "remove", unwanted, "from", path) }
	k, v, line, ok := walkTo(root, path)
	if !ok {
		return refuse(line)
	}
	if k == nil || isNull(v) {
		return body, false, nil
	}
	if v.Kind != yaml.SequenceNode {
		return refuse(k.Line)
	}

	drop := map[int]bool{}
	var keep []string
	for i, item := range v.Content {
		if item.Kind == yaml.ScalarNode && containsFold(unwanted, item.Value) {
			drop[i] = true
			continue
		}
		keep = append(keep, item.Value)
	}
	if len(drop) == 0 {
		return body, false, nil
	}

	if v.Style&yaml.FlowStyle != 0 {
		m := flowSequenceOn(lines, k.Line, path[len(path)-1])
		// The line must hold exactly the entries the parse found, each a
		// plain value, or the rewrite below would be a guess about which
		// text is which entry.
		if m == nil || len(splitFlow(m[2])) != len(v.Content) {
			return refuse(k.Line)
		}
		for _, item := range v.Content {
			if !singleLineScalar(item) {
				return refuse(k.Line)
			}
		}
		quoted := make([]string, 0, len(keep))
		for _, id := range keep {
			quoted = append(quoted, yamlScalar(id))
		}
		out := append([]string(nil), lines...)
		out[k.Line-1] = m[1] + strings.Join(quoted, ", ") + m[3]
		return joinLines(out), true, nil
	}

	var gone []int
	for i := range drop {
		item := v.Content[i]
		// One entry per line, and nothing but the entry on it: anything
		// else and deleting the line deletes something that was not asked.
		if item.Line < 1 || item.Line > len(lines) || !singleLineScalar(item) || sharesLine(v, i) ||
			!blockItemPrefix.MatchString(lines[item.Line-1]) {
			return refuse(item.Line)
		}
		gone = append(gone, item.Line)
	}
	return joinLines(deleteLines(lines, gone)), true, nil
}

// RemoveKey returns body with the key at path and its value deleted, the
// values it held when that value was a list, and whether anything changed.
// A key that is not there changes nothing. It removes whole lines only --
// the key's own and, for a block list, its entries' -- and refuses a value
// it cannot bound that way, rather than guessing where it ends.
func RemoveKey(body string, path []string) (string, []string, bool, error) {
	if len(path) == 0 {
		return "", nil, false, errors.New("no key path to remove")
	}
	lines := splitLines(body)
	root, err := parseRoot(body)
	if err != nil {
		return "", nil, false, err
	}
	if root == nil {
		return body, nil, false, nil
	}
	refuse := func(line int) (string, []string, bool, error) {
		_, _, err := notEditable(line, "remove", nil, "", path)
		return "", nil, false, err
	}
	k, v, line, ok := walkTo(root, path)
	if !ok {
		return refuse(line)
	}
	if k == nil {
		return body, nil, false, nil
	}
	end := k.Line
	switch {
	case isNull(v):
	case v.Kind == yaml.ScalarNode && singleLineScalar(v) && v.Line == k.Line:
	case v.Kind == yaml.SequenceNode && v.Style&yaml.FlowStyle != 0:
		if flowSequenceOn(lines, k.Line, path[len(path)-1]) == nil {
			return refuse(k.Line)
		}
	case v.Kind == yaml.SequenceNode:
		for i, item := range v.Content {
			if !singleLineScalar(item) || sharesLine(v, i) {
				return refuse(item.Line)
			}
			end = max(end, item.Line)
		}
	default:
		return refuse(k.Line)
	}
	if k.Line < 1 || end > len(lines) {
		return refuse(k.Line)
	}
	gone := make([]int, 0, end-k.Line+1)
	for l := k.Line; l <= end; l++ {
		gone = append(gone, l)
	}
	return joinLines(deleteLines(lines, gone)), sequenceValues(v), true, nil
}

// blockItemPrefix captures the exact leading whitespace, dash and spacing
// of an existing sequence entry.
var blockItemPrefix = regexp.MustCompile(`^(\s*-\s+)\S`)

// flowSequenceOn matches a single-line flow sequence under key on the given
// 1-based line, and nothing else: prefix through `[`, the entries, and `]`
// with whatever follows it. A flow list spread over several lines, or one
// carrying a bracket inside a quoted value, is nil -- and the caller
// refuses, because getting either wrong writes a policy.yaml the worker can
// no longer parse, which takes the machine out of the fleet entirely.
func flowSequenceOn(lines []string, line int, key string) []string {
	if line < 1 || line > len(lines) {
		return nil
	}
	re := regexp.MustCompile(`^(\s*` + regexp.QuoteMeta(key) + `:\s*\[)([^\[\]]*)(\].*)$`)
	return re.FindStringSubmatch(lines[line-1])
}

// splitFlow splits a flow sequence's inside into its entries.
func splitFlow(inside string) []string {
	if strings.TrimSpace(inside) == "" {
		return nil
	}
	return strings.Split(inside, ",")
}

// notEditable is the refusal: the line, and the change to make by hand.
func notEditable(line int, verb string, ids []string, prep string, path []string) (string, bool, error) {
	where := strings.Join(path, ".")
	if len(ids) == 0 {
		return "", false, fmt.Errorf("%w (around line %d): %s %s by hand", ErrPolicyNotEditable, line, verb, where)
	}
	return "", false, fmt.Errorf("%w (around line %d): %s %s %s %s by hand",
		ErrPolicyNotEditable, line, verb, strings.Join(ids, ", "), prep, where)
}

// -----------------------------------------------------------------------------
// The blocks this file writes
// -----------------------------------------------------------------------------

// freshFileHeader goes only on a policy.yaml this command created. It is
// not added to a file somebody else wrote: a command run to add one model
// has no business leaving its own commentary in an operator's file.
var freshFileHeader = []string{
	"# MemQL Cockpit worker policy. Every allow list here is DEFAULT-DENY: only",
	"# what is listed is offered (docs/local-models.md, docs/local-apps.md).",
}

// appendBlock adds the whole path, as a fresh top-level block, at the end
// of the file.
func appendBlock(lines, path, ids []string, fresh bool) []string {
	out := append([]string(nil), lines...)
	if fresh {
		out = append(out, freshFileHeader...)
	} else if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
		out = append(out, "")
	}
	return append(out, nestedBlock(0, path, ids)...)
}

// insertUnder adds the rest of a path under an existing mapping: at the
// end of the file for the root, otherwise directly beneath parentKey's line
// at the indentation parent's own children already use -- taken from a
// child rather than assumed, so a file indented with four spaces stays
// indented with four spaces.
func insertUnder(lines []string, parent, parentKey *yaml.Node, rest, ids []string) []string {
	if parentKey == nil {
		return appendBlock(lines, rest, ids, false)
	}
	indent := parentKey.Column - 1 + 2
	if len(parent.Content) > 0 {
		indent = parent.Content[0].Column - 1
	}
	return insertLines(lines, parentKey.Line, nestedBlock(indent, rest, ids))
}

// nestedBlock renders keys, each two deeper than the last, ending in the
// list: nestedBlock(0, [models allow], ids) is `models:`, `  allow:` and
// its entries.
func nestedBlock(indent int, keys, ids []string) []string {
	out := make([]string, 0, len(keys)+len(ids))
	for i, key := range keys {
		out = append(out, strings.Repeat(" ", indent+2*i)+yamlKey(key)+":")
	}
	return append(out, items(indent+2*len(keys), ids)...)
}

func items(indent int, ids []string) []string {
	pad := strings.Repeat(" ", indent)
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, pad+"- "+yamlScalar(id))
	}
	return out
}

// plainKey is the set of keys that need no quoting. Narrower than
// plainScalar: a colon inside a KEY is one character away from ending it,
// so a key with one is quoted rather than left to the reader's parser.
var plainKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@/+-]*$`)

func yamlKey(s string) string {
	if plainKey.MatchString(s) {
		return s
	}
	return strconv.Quote(s)
}

// plainScalar is the set of ids that need no quoting. A model id is
// normally in it -- `llama3.1:8b`, `hf.co/owner/repo:Q4_K_M` -- because a
// colon only ends a key when a space follows it. Anything else is quoted
// rather than assumed safe: an id that YAML re-read as a mapping would
// make the whole file unparseable, and the worker would then serve
// nothing at all.
var plainScalar = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/+@-]*$`)

func yamlScalar(s string) string {
	if plainScalar.MatchString(s) {
		return s
	}
	return strconv.Quote(s)
}

// -----------------------------------------------------------------------------
// Node and line helpers
// -----------------------------------------------------------------------------

// documentRoot returns the single document's root mapping, or nil.
//
// A file with more than one document is refused rather than guessed at:
// which of two `models:` keys is the live one is a question this command
// has no standing to answer.
func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc == nil || doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil
	}
	root := doc.Content[0]
	if isNull(root) {
		return nil
	}
	return root
}

// parseRoot parses body and returns its root, or nil for a file with
// nothing in it yet.
func parseRoot(body string) (*yaml.Node, error) {
	var doc yaml.Node
	if strings.TrimSpace(body) != "" {
		if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
			// The worker cannot read this file either, so it is already
			// broken. Overwriting it would delete whatever the operator is
			// halfway through fixing.
			return nil, fmt.Errorf("it does not parse as YAML, so nothing was changed: %w", err)
		}
	}
	return documentRoot(&doc), nil
}

// walkTo finds the key at path under root. A key missing anywhere on the
// path is (nil, nil, _, true) -- there is nothing there -- while a node on
// the path that is not a block mapping is (_, _, its line, false): there
// may be something there, in a shape this file does not edit.
func walkTo(root *yaml.Node, path []string) (key, value *yaml.Node, line int, ok bool) {
	node := root
	for i, name := range path {
		if node.Kind != yaml.MappingNode || node.Style&yaml.FlowStyle != 0 {
			return nil, nil, node.Line, false
		}
		k, v := findKey(node, name)
		if k == nil {
			return nil, nil, 0, true
		}
		if i == len(path)-1 {
			return k, v, k.Line, true
		}
		if isNull(v) {
			return nil, nil, 0, true
		}
		node = v
	}
	return nil, nil, 0, true
}

// singleLineScalar is an entry whose whole text sits on its own line: no
// block scalar (`|`, `>`) and no quoted value folded over several lines.
func singleLineScalar(n *yaml.Node) bool {
	if n == nil || n.Kind != yaml.ScalarNode || n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return false
	}
	return !strings.Contains(n.Value, "\n")
}

// sharesLine reports whether sequence entry i shares a line with another.
func sharesLine(seq *yaml.Node, i int) bool {
	for j, other := range seq.Content {
		if j != i && other.Line == seq.Content[i].Line {
			return true
		}
	}
	return false
}

// deleteLines removes the given 1-based line numbers.
func deleteLines(lines []string, gone []int) []string {
	drop := make(map[int]bool, len(gone))
	for _, l := range gone {
		drop[l] = true
	}
	out := make([]string, 0, len(lines))
	for i, l := range lines {
		if !drop[i+1] {
			out = append(out, l)
		}
	}
	return out
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(s)) {
			return true
		}
	}
	return false
}

func findKey(m *yaml.Node, key string) (*yaml.Node, *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

func isNull(n *yaml.Node) bool {
	return n == nil || (n.Kind == yaml.ScalarNode && n.Tag == "!!null")
}

func sequenceValues(n *yaml.Node) []string {
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	out := make([]string, 0, len(n.Content))
	for _, item := range n.Content {
		if item.Kind == yaml.ScalarNode {
			out = append(out, item.Value)
		}
	}
	return out
}

// insertLines puts add after the 1-based line number after.
func insertLines(lines []string, after int, add []string) []string {
	if after < 0 {
		after = 0
	}
	if after > len(lines) {
		after = len(lines)
	}
	out := make([]string, 0, len(lines)+len(add))
	out = append(out, lines[:after]...)
	out = append(out, add...)
	return append(out, lines[after:]...)
}

// splitLines and joinLines round-trip the file. The written file always
// ends in a newline, including one that arrived without: that changes no
// key, and a YAML file whose last line has no terminator is a nuisance
// every editor silently fixes anyway.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// cleanIDs trims, drops the empties and collapses duplicates, keeping the
// caller's order -- which is the order `--model` was typed in, and the
// order the person expects to read back.
func cleanIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func missing(wanted, have []string) []string {
	present := make(map[string]struct{}, len(have))
	for _, h := range have {
		present[h] = struct{}{}
	}
	out := make([]string, 0, len(wanted))
	for _, w := range wanted {
		if _, ok := present[w]; !ok {
			out = append(out, w)
		}
	}
	return out
}

// writePolicyAtomic writes through a temp file in the SAME directory and
// renames, so a crash or a full disk leaves the old policy.yaml whole. A
// half-written one is worse than an unchanged one: the worker reads it at
// the next SIGHUP and would find a truncated allow list, which is a
// machine that silently stops serving models it was serving a minute ago.
func writePolicyAtomic(path string, body []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, policyDirMode); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".policy-*")
	if err != nil {
		return fmt.Errorf("temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, policyFileMode); err != nil {
		return fmt.Errorf("setting the mode on %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming into %s: %w", path, err)
	}
	return nil
}
