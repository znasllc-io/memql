package memql

// prompt_template_fields.go holds the load-time cross-check between what a
// prompt's `.tmpl` READS and what its body DECLARES.
//
// Why (memql#3616): a prompt's input schema compiles with
// additionalProperties:false, and aiRuntime.Invoke validates the caller's
// data BEFORE rendering. So a template that renders `{{if .phase}}` while
// the declaration omits `phase` is not "a field the prompt ignores" -- it
// is a field no caller can ever supply. Every call that supplies it fails
// schema validation, and the failure lands wherever the caller decided to
// put it. `cognitionPrediction` degraded into its pattern-based fallback
// for the whole life of the divergence, which is why nobody saw it. The
// same class had already been fixed once by hand -- see the `directive`
// field comment on cognitionReply -- which is what makes it worth a check
// rather than another comment.
//
// Direction: the check is one-way, template reads ⊆ declared fields. A
// declared field the template never reads is inert (the data is simply not
// rendered) and several prompts legitimately carry near-future fields, so
// that direction is not enforced.
//
// Scope: a prompt declaring NO fields compiles to a nil schema, so
// ValidateData accepts anything and there is no rejection to prevent.
// Those are skipped rather than being held to a rule that cannot bite.

import (
	"fmt"
	"sort"
	"strings"
	"text/template"
	"text/template/parse"
)

// promptTemplateRootFields returns the distinct ROOT-SCOPE field names a
// compiled prompt template reads, sorted.
//
// "Root scope" is the part that takes care: inside `{{range .transcript}}`
// or `{{with .space}}` the dot is rebound, so `{{.text}}` there reads an
// element field, not a prompt input. The walker tracks that rebinding; it
// also treats `$.x` as a root read wherever it appears, since `$` is always
// the top-level argument.
func promptTemplateRootFields(tmpl *template.Template) []string {
	if tmpl == nil || tmpl.Tree == nil || tmpl.Tree.Root == nil {
		return nil
	}
	w := &templateFieldWalker{
		tmpl:    tmpl,
		seen:    make(map[string]bool),
		visited: make(map[string]bool),
	}
	w.walkList(tmpl.Tree.Root, true)
	out := make([]string, 0, len(w.seen))
	for name := range w.seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

type templateFieldWalker struct {
	tmpl    *template.Template
	seen    map[string]bool
	visited map[string]bool // {{template}} bodies already walked, so a cycle terminates
}

func (w *templateFieldWalker) record(name string) {
	if name = strings.TrimSpace(name); name != "" {
		w.seen[name] = true
	}
}

func (w *templateFieldWalker) walkList(list *parse.ListNode, rootDot bool) {
	if list == nil {
		return
	}
	for _, n := range list.Nodes {
		w.walkNode(n, rootDot)
	}
}

func (w *templateFieldWalker) walkNode(n parse.Node, rootDot bool) {
	switch node := n.(type) {
	case *parse.ActionNode:
		w.walkPipe(node.Pipe, rootDot)
	case *parse.IfNode:
		// `if` never rebinds dot -- both branches stay in the current scope.
		w.walkPipe(node.Pipe, rootDot)
		w.walkList(node.List, rootDot)
		w.walkList(node.ElseList, rootDot)
	case *parse.WithNode:
		// The body runs with dot rebound to the pipeline's value; the else
		// branch runs with the ORIGINAL dot.
		w.walkPipe(node.Pipe, rootDot)
		w.walkList(node.List, false)
		w.walkList(node.ElseList, rootDot)
	case *parse.RangeNode:
		// Same rebinding rule as `with`: body is an element, else is outer.
		w.walkPipe(node.Pipe, rootDot)
		w.walkList(node.List, false)
		w.walkList(node.ElseList, rootDot)
	case *parse.ListNode:
		w.walkList(node, rootDot)
	case *parse.TemplateNode:
		w.walkTemplate(node, rootDot)
	}
}

func (w *templateFieldWalker) walkPipe(pipe *parse.PipeNode, rootDot bool) {
	if pipe == nil {
		return
	}
	for _, cmd := range pipe.Cmds {
		if cmd == nil {
			continue
		}
		for _, arg := range cmd.Args {
			w.walkArg(arg, rootDot)
		}
	}
}

func (w *templateFieldWalker) walkArg(n parse.Node, rootDot bool) {
	switch node := n.(type) {
	case *parse.FieldNode:
		// `.a.b.c` -- only the first segment is a prompt input, and only
		// when dot is still the root argument.
		if rootDot && len(node.Ident) > 0 {
			w.record(node.Ident[0])
		}
	case *parse.VariableNode:
		// `$.a` is a root read regardless of the enclosing scope; `$x.a`
		// reads off a declared variable and is not.
		if len(node.Ident) > 1 && node.Ident[0] == "$" {
			w.record(node.Ident[1])
		}
	case *parse.ChainNode:
		// `(pipeline).Field` -- the base decides whose field it is.
		if _, isDot := node.Node.(*parse.DotNode); isDot && rootDot && len(node.Field) > 0 {
			w.record(node.Field[0])
		}
		w.walkArg(node.Node, rootDot)
	case *parse.PipeNode:
		w.walkPipe(node, rootDot)
	}
}

func (w *templateFieldWalker) walkTemplate(node *parse.TemplateNode, rootDot bool) {
	if node == nil {
		return
	}
	// The argument pipeline itself is evaluated in the current scope.
	w.walkPipe(node.Pipe, rootDot)
	if node.Pipe == nil {
		// `{{template "x"}}` passes nil -- nothing inside can read a root
		// field, so the body is out of scope for this check.
		return
	}
	if !argIsRootDot(node.Pipe, rootDot) {
		// The partial gets some other value as its dot; its reads are that
		// value's fields, not the prompt's inputs.
		return
	}
	assoc := w.tmpl.Lookup(node.Name)
	if assoc == nil || assoc.Tree == nil {
		// Partial not in this template set (the loader's partial set is
		// optional in tests). Nothing to walk.
		return
	}
	if w.visited[node.Name] {
		return
	}
	w.visited[node.Name] = true
	w.walkList(assoc.Tree.Root, true)
}

// argIsRootDot reports whether a {{template}} argument pipeline passes the
// root argument through unchanged -- either a bare `.` while dot is still
// root, or an explicit `$`.
func argIsRootDot(pipe *parse.PipeNode, rootDot bool) bool {
	if pipe == nil || len(pipe.Cmds) != 1 {
		return false
	}
	cmd := pipe.Cmds[0]
	if cmd == nil || len(cmd.Args) != 1 {
		return false
	}
	switch arg := cmd.Args[0].(type) {
	case *parse.DotNode:
		return rootDot
	case *parse.VariableNode:
		return len(arg.Ident) == 1 && arg.Ident[0] == "$"
	}
	return false
}

// validatePromptTemplateFields checks that every root-scope field the
// template reads is declared in the prompt's body. Returns nil for a
// prompt with no declared fields (nil schema -> nothing is rejected, so
// there is nothing to guard).
func validatePromptTemplateFields(decl *promptDecl, tmpl *template.Template) error {
	if decl == nil || len(decl.fields) == 0 {
		return nil
	}
	declared := make(map[string]bool, len(decl.fields))
	for _, f := range decl.fields {
		declared[strings.TrimSpace(f.name)] = true
	}
	var missing []string
	for _, name := range promptTemplateRootFields(tmpl) {
		if !declared[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("prompt %q: template reads %d undeclared input(s): %s. "+
		"The input schema is additionalProperties:false and is validated BEFORE the template renders, "+
		"so a caller supplying one of these fails EVERY call -- the field is unreachable, not optional. "+
		"Declare each in the prompt body, or stop reading it in the template",
		decl.name, len(missing), strings.Join(missing, ", "))
}
