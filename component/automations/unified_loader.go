package automations

// unified_loader.go pulls automation declarations out of the new
// domain-first DSL tree (dsl/<domain>/automations.memql) and feeds
// each one through the existing compileMemQL pipeline.
//
// This is the ONLY automation loading path. A legacy walker over a
// `Loader.fsys` field once handled `automation.memql` single-file
// declarations; it was unreachable and was deleted in memql#2858, so
// LoadAll is now a thin wrapper over LoadFromUnifiedTree.
//
// The unified tree bundles multiple automations + their logic blocks
// per file (`<domain>/automations.memql`), so we extract each
// automation slice from the bundled source and compile each in
// isolation.

import (
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// automationStructHeader matches the opening of an `automation
// NAME {` block at column 0 (with optional leading whitespace).
var automationStructHeader = regexp.MustCompile(`(?m)^[ \t]*automation[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

// automationLooseHeader matches an `automation NAME` declaration WITHOUT
// requiring the opening brace on the same line. Every header the strict
// regex above misses is an automation that would silently vanish, so the
// loose form is used only to DETECT and report those -- never to extract.
//
// Neither regex understands `/* */`, so both are run over a comment-blanked
// copy of the source (languageParser.BlankComments, memql#2861) rather than
// the raw text. Scanning raw, a commented-out automation matched here and loaded
// LIVE, and -- once a dropped automation became a refused boot (#2830) -- a
// commented-out automation whose `{` sat on the next line was reported
// unextractable and took the node down instead.
//
// Both regexes anchor at `^[ \t]*automation`, so a `//` line-commented
// automation can never match them; line comments need no masking.
var automationLooseHeader = regexp.MustCompile(`(?m)^[ \t]*automation[ \t]+([A-Za-z_][A-Za-z0-9_]*)`)

// LoadFromUnifiedTree walks dsl.Tree() looking for
// `<domain>/automations.memql` files, extracts every
// `automation NAME { ... }` block, and compiles each via the
// existing compileMemQL pipeline.
//
// Slice extraction mirrors the pattern used in
// component/memql/function_slices.go: regex-match the header,
// walk balanced braces to the closer, walk backwards from the
// header to gather the @-attribute + comment preamble.
//
// Returns the list of compiled automations. EVERY load problem, across all
// four phases -- read, terse-lowering, extract, compile -- is collected and,
// unless the MEMQL_DSL_ALLOW_SKIPS break-glass is set, returned as an error,
// so the node refuses to boot half-loaded rather than running silently
// without the automation (memql#2830).
//
// Directories whose name starts with `_` or `.` are skipped as soft-disabled,
// matching every other DSL walker.
func (l *Loader) LoadFromUnifiedTree() ([]*Automation, error) {
	tree := memqldsl.Tree()
	var out []*Automation
	// Hard load problems collected across the whole walk (memql#2830). We
	// keep walking after the first one so the operator gets the COMPLETE
	// list in a single boot attempt rather than peeling them off one per
	// restart.
	var problems []automationLoadProblem

	err := fs.WalkDir(tree, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		// Soft-disable / hidden directories are skipped, matching the
		// convention every other DSL walker enforces (dslfs.WalkMemqlFiles,
		// component/actions/loader.go, capability_loader.go) and documented
		// for MEMQL_DSL_PATH in dsl/runtime_mount.go. This walker previously
		// had no such filter, which did not matter while a drop was a silent
		// WARN -- but now that a load problem REFUSES BOOT, a product bundle
		// parking a WIP automation in `<domain>/_disabled/automations.memql`
		// would brick the fleet on a file the rest of the DSL system treats
		// as disabled (memql#2830 review round 2).
		if d.IsDir() {
			base := d.Name()
			if base != "." && (strings.HasPrefix(base, "_") || strings.HasPrefix(base, ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "/automations.memql") {
			return nil
		}
		data, readErr := fs.ReadFile(tree, path)
		if readErr != nil {
			if l.logger != nil {
				l.logger.Warn("unified automation loader: read failed",
					"path", path, "error", readErr)
			}
			problems = append(problems, automationLoadProblem{
				Path: path, Phase: "read", Err: readErr.Error(),
			})
			return nil
		}
		source := string(data)
		// An unterminated `/*` comments out the rest of the file, so every
		// automation below it is ABSENT -- correct per the lexer, and now
		// per the comment-blanked extractor, but silently absent is what
		// #2830 outlawed. One typo'd opener would remove a workflow from the
		// fleet with no diagnostic at all. Lexer-legal input, so this is a
		// WARN naming the line rather than a load problem (memql#2861).
		if line := languageParser.UnterminatedBlockCommentLine(source); line > 0 && l.logger != nil {
			l.logger.Warn("unified automation loader: unterminated block comment",
				"component", ComponentName, "path", path, "line", line,
				"effect", "every automation below this line is commented out and will not load")
		}
		// Lower terse single-step automations (memql#2215) to their longhand
		// block form so the slice extractor below (which keys off `automation
		// NAME {`) discovers them. compileMemQL re-runs the rewriter, so this is
		// idempotent for already-longhand sources.
		//
		// A non-nil error is a hard authoring violation (G1 / memql#2363: a
		// file-top `args { }` block preceding a terse automation, which the
		// terse form forbids). Previously terse lowering could never error, so
		// the error path is new -- surface it loudly and skip the file rather
		// than silently loading the un-lowered (and thus un-discovered) source.
		lowered, lerr := languageParser.NormaliseTerseAutomationSource(source)
		if lerr != nil {
			if l.logger != nil {
				l.logger.Warn("unified automation loader: terse lowering rejected",
					"component", ComponentName, "path", path, "error", lerr)
			}
			problems = append(problems, automationLoadProblem{
				Path: path, Phase: "terseLowering", Err: lerr.Error(),
			})
			return nil
		}
		source = lowered
		slices, unextracted := extractAutomationSlicesReporting(source)
		for _, name := range unextracted {
			if l.logger != nil {
				l.logger.Warn("unified automation loader: automation header could not be extracted",
					"component", ComponentName, "path", path, "automation", name)
			}
			problems = append(problems, automationLoadProblem{
				Path: path, Name: name, Phase: "extract",
				Err: "could not extract an automation block for this header -- unbalanced braces, or the opening `{` is not on the header line",
			})
		}
		for _, slice := range slices {
			origin := "unified:" + path + ":" + slice.Name
			automation, compileErr := l.compileMemQL(slice.Source, origin)
			if compileErr != nil {
				// EVERY compile error is a hard problem -- there is no
				// by-design skip left to carve out. The old code exempted
				// errors containing "not found in registry" as expected
				// per-node-type concept-visibility filtering. That exemption
				// is now provably unreachable for its intended purpose AND
				// actively harmful:
				//
				//   - Both by-design producers of that text
				//     (component/memql/concept_resolver.go, and the twin in
				//     this package's loader.go) sit behind the LEGACY FORM A
				//     `use` syntax, which the parser now rejects outright --
				//     `use a.b` fails with "retired Form A shape" long before
				//     concept resolution runs. Form B (`use a.b.{ c }`)
				//     tolerates an unresolvable name by design and returns no
				//     error at all.
				//   - @visibility filtering no longer exists in the tree, and
				//     app/database.go loads the full concept set on every node
				//     type, so no automation is dropped for concept-absence.
				//     Measured on the shipped tree: 0 such skips.
				//   - The only REACHABLE producers of that text are
				//     component/memql/executor_filter.go, where it means a
				//     filter names a concept that does not exist -- a genuine
				//     authoring defect. So the exemption could only ever
				//     swallow real bugs, which is exactly memql#2830.
				if l.logger != nil {
					l.logger.Warn("unified automation loader: compile failed",
						"component", ComponentName,
						"path", origin,
						"error", compileErr)
				}
				problems = append(problems, automationLoadProblem{
					Path: path, Name: slice.Name, Phase: "compile", Err: compileErr.Error(),
				})
				continue
			}
			automation.Origin = origin
			// #2800: this is the ONLY place trust is granted. These bodies
			// come from the embedded/registered DSL tree, so they are
			// authored, reviewed and not caller-supplied.
			automation.Trusted = true
			out = append(out, automation)
		}
		return nil
	})
	if err != nil {
		return out, fmt.Errorf("walk unified DSL tree: %w", err)
	}

	// Strict automation boot (memql#2830). Before this gate every compile
	// error was swallowed by the `continue` above: a malformed automation
	// was dropped with a WARN and the node booted green, silently missing
	// the workflow. That is the pack-rot failure mode the DSL strict-boot
	// gate already refuses for every other construct kind
	// (component/memql/engine_bootstrap.go); automations were the one
	// exemption. MEMQL_DSL_ALLOW_SKIPS is the same operator break-glass.
	if len(problems) > 0 {
		if !memql.DSLAllowSkips() {
			// "load refused", not "boot refused": this error also surfaces at
			// runtime through LoadByName -> MCP run_automation, where the
			// process is already up and "boot" would be the wrong noun.
			return out, fmt.Errorf("strict automation load refused: %d automation load problem(s) in the unified DSL tree; set %s=1 to load anyway (operator break-glass -- a node with problems will boot with those automations missing).\n%s",
				len(problems), memql.AllowSkipsEnvVar, formatAutomationProblems(problems))
		}
		if l.logger != nil {
			l.logger.Error("MEMQL_DSL_ALLOW_SKIPS set: loading automations despite load problems (operator break-glass)",
				"component", ComponentName,
				"problems", len(problems),
				"detail", formatAutomationProblems(problems))
		}
	}

	if l.logger != nil {
		l.logger.Info("unified automation loader: loaded",
			"count", len(out))
	}
	return out, nil
}

// automationLoadProblem is one automation (or one whole file) the unified
// loader could not turn into a runnable automation. No reason is exempt.
type automationLoadProblem struct {
	Path  string // automations.memql file in the DSL tree
	Name  string // automation name (empty for whole-file problems)
	Phase string // "read" | "terseLowering" | "extract" | "compile"
	Err   string
}

// formatAutomationProblems renders the collected problems one per line for
// the refusal error + the break-glass log.
func formatAutomationProblems(problems []automationLoadProblem) string {
	var b strings.Builder
	for i, p := range problems {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("  - ")
		b.WriteString(p.Path)
		if p.Name != "" {
			b.WriteString(":")
			b.WriteString(p.Name)
		}
		b.WriteString(" [")
		b.WriteString(p.Phase)
		b.WriteString("] ")
		b.WriteString(p.Err)
	}
	return b.String()
}

// automationSlice is one extracted automation declaration ready to
// feed through compileMemQL.
type automationSlice struct {
	Source string // slice text (preamble + automation body)
	Name   string // declaration name from the header
}

// extractAutomationSlices finds every `automation NAME { ... }`
// block in `source` and returns each as a self-contained slice
// with its @-attribute preamble.
//
// Logic blocks in the same file are NOT extracted here -- they're
// declared as `logic NAME { ... }` and load through the unified
// FUNCTION loader (LoadUnifiedFunctions). The automation slice
// stands alone because compileMemQL parses the slice in isolation;
// any logic-block references resolve via the function registry at
// runtime.
func extractAutomationSlices(source string) []automationSlice {
	slices, _ := extractAutomationSlicesReporting(source)
	return slices
}

// extractAutomationSlicesReporting is extractAutomationSlices plus the names
// of headers it could NOT turn into a slice.
//
// Extraction was the one drop phase with no signal at all -- not even a WARN
// (memql#2830 review round 2). An automation with unbalanced braces, or whose
// opening `{` sits on the next line, simply vanished: `LoadAll -> 31
// automations, err=<nil>` with a workflow missing, which is verbatim the
// failure mode this issue is about. Unbalanced braces is also the single most
// common malformed-automation shape.
//
// Two classes are detected:
//   - a matched header whose `{` never closes (findMatchingBrace fails);
//   - a bare `automation NAME` header the strict struct regex does not match,
//     which is almost always the `{`-on-the-next-line spelling. Terse
//     `automation NAME @trigger(...) => logic X` declarations are lowered to
//     longhand BEFORE extraction, so they do not reach here as bare headers.
func extractAutomationSlicesReporting(source string) ([]automationSlice, []string) {
	var unextracted []string

	// Detect headers on a comment-blanked view so a commented-out automation
	// is ABSENT rather than live (memql#2861). BlankComments preserves byte
	// offsets, so every offset below indexes the original text identically:
	// headers and braces are found on `scan`, the slice is cut from `source`,
	// and the emitted slice keeps its authored comments verbatim.
	//
	// This mirrors ExtractFunctionSlices (component/memql/function_slices.go),
	// which has blanked comments before header detection since memql#1074 --
	// which is why queries, mutations and logic were never exposed to this
	// defect, and why the fix reuses that helper instead of adding a second
	// implementation. Other extractors still scan raw text and have the same
	// blindness (component/actions/loader.go, component/memql/keyword_slices.go
	// and capability_loader.go) -- tracked in memql#2868, not fixed here.
	scan := languageParser.BlankComments(source)
	// Spans, not just the blanked copy: see the preamble walk below for why a
	// blank line inside a block comment is indistinguishable without them.
	commentSpans := languageParser.CommentSpans(source)

	matches := automationStructHeader.FindAllStringSubmatchIndex(scan, -1)

	// A header the strict regex missed is a silent loss, so find those too.
	matchedAt := make(map[int]bool, len(matches))
	for _, m := range matches {
		matchedAt[m[0]] = true
	}
	for _, lm := range automationLooseHeader.FindAllStringSubmatchIndex(scan, -1) {
		if !matchedAt[lm[0]] {
			unextracted = append(unextracted, source[lm[2]:lm[3]])
		}
	}

	if len(matches) == 0 {
		return nil, unextracted
	}

	var out []automationSlice
	for _, m := range matches {
		headerStart := m[0]
		headerEnd := m[1]
		nameStart := m[2]
		nameEnd := m[3]
		name := source[nameStart:nameEnd]

		openIdx := headerEnd - 1 // position of `{`
		closeIdx := findMatchingBrace(scan, openIdx)
		if closeIdx < 0 {
			unextracted = append(unextracted, name)
			continue
		}

		// Walk backwards for the @-attribute + comment preamble.
		//
		// The walk runs on `scan` -- the COMMENT-BLANKED view -- and the slice
		// is cut from `source`, so authored comments (including semantic `///`
		// doc comments) survive verbatim in the emitted text.
		//
		// Blanking is what makes the walk correct (memql#2872). It previously
		// ran on the original and stopped at any line not starting with `@` or
		// `//`, which a `/* ... */` line is -- so EVERY annotation above a
		// block comment was cut out of the slice. Both directions were silent:
		// a dropped `@disabled` means the automation loads ENABLED and fires on
		// every replica; a dropped `@trigger` means it loads with a nil trigger,
		// registered and counted but never subscribed, and invisible to the
		// shipped-count guard because IsEventTriggered() is false for nil.
		//
		// On the blanked view a comment line is all spaces, so the rule is:
		//
		//   - blanked line starts with `@`            -> annotation, include
		//   - blanked line empty, and the line is a
		//     comment (original non-empty, OR the
		//     line sits INSIDE a comment span)        -> include, keep going
		//   - blanked line empty and genuinely blank  -> real blank line, STOP
		//   - anything else                           -> real code, STOP
		//
		// The span check is not redundant with the blanked comparison, and
		// leaving it out is how the first cut of this fix broke: a BLANK LINE
		// INSIDE a block comment is byte-identical in both views (newlines are
		// preserved and there is nothing else on the line to blank), so the
		// walk stopped mid-comment and the slice started inside it -- the
		// remaining comment text and its `*/` then parsed as code. That is
		// verbatim the documented "2+ consecutive blank lines refuses boot"
		// failure that sank the two earlier attempts, and the pre-existing
		// TestBlockCommentAboveAutomationDoesNotDisturbTheLoad caught it.
		//
		// The last two matter: without them the walk would swallow the previous
		// construct, so parking one automation would silently disable its
		// neighbour. TestSlicePreambleStopsAtRealCode pins that direction.
		//
		// Two earlier attempts at this were reverted because pulling the
		// comment body into the slice made compileMemQL's raw-text gates fire
		// on ordinary comments. Those gates now scan a blanked view too
		// (loader.go), which is the half that makes this safe.
		preambleStart := headerStart
		for k := headerStart - 1; k >= 0; k-- {
			lineStart := strings.LastIndexByte(source[:k], '\n') + 1
			blanked := strings.TrimSpace(strings.TrimRight(scan[lineStart:k+1], "\r\n"))
			original := strings.TrimSpace(strings.TrimRight(source[lineStart:k+1], "\r\n"))
			inComment := languageParser.OffsetInComment(commentSpans, lineStart)
			if strings.HasPrefix(blanked, "@") || (blanked == "" && (original != "" || inComment)) {
				preambleStart = lineStart
				k = lineStart - 1
				continue
			}
			break
		}

		// POST-CHECK: the walk is per-line, so it can stop with preambleStart
		// already INSIDE a comment span (memql#2872 review).
		//
		// The shape: a block comment whose OPENER shares a line with real code,
		// directly above the preamble --
		//
		//     automation firstOne { ... } /* parked, see #123
		//          second line */
		//     @enabled
		//     automation secondOne { ... }
		//
		// On the opener's line the code before `/*` survives blanking, so
		// `blanked` is non-empty and the loop breaks -- but preambleStart has
		// already advanced onto the comment's LAST line. The slice then begins
		// `second line */`, which parses as code. Because the strict gate is
		// all-or-nothing, that one ordinary comment refuses the WHOLE tree: 31
		// automations gone.
		//
		// That is the documented "slice starts mid-comment" failure that sank
		// both earlier attempts, reached by a different route. A per-line stop
		// test structurally cannot see it -- the offending line is not the one
		// being examined -- so the final position is validated here and snapped
		// forward past the span.
		// STRICTLY inside, not merely inside. preambleStart == span.Start means
		// the comment is included WHOLE (`/* parked */ @disabled` on one line),
		// which is fine -- the gates blank it. Only a start position with
		// dangling comment text before it is a problem.
		for {
			span, inside := languageParser.CommentSpanContaining(commentSpans, preambleStart)
			if !inside || span.Start >= preambleStart {
				break
			}
			next := strings.IndexByte(source[span.End:], '\n')
			if next < 0 {
				preambleStart = headerStart
				break
			}
			preambleStart = span.End + next + 1
			if preambleStart > headerStart {
				preambleStart = headerStart
				break
			}
		}

		out = append(out, automationSlice{
			Source: source[preambleStart : closeIdx+1],
			Name:   name,
		})
	}
	return out, unextracted
}

// findMatchingBrace returns the index of the `}` matching the `{`
// at openIdx. String + line-comment aware. Callers pass a
// BlankComments'd source so a brace inside a `/* */` comment does
// not close a block early (memql#2861). Returns -1 if unbalanced.
func findMatchingBrace(source string, openIdx int) int {
	if openIdx < 0 || openIdx >= len(source) || source[openIdx] != '{' {
		return -1
	}
	depth := 0
	inString := false
	for i := openIdx; i < len(source); i++ {
		c := source[i]
		if inString {
			if c == '\\' && i+1 < len(source) {
				i++
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '/':
			if i+1 < len(source) && source[i+1] == '/' {
				nl := strings.IndexByte(source[i:], '\n')
				if nl < 0 {
					return -1
				}
				i += nl
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// io.ReadAll is referenced in case future logic needs it.
var _ = io.ReadAll

// silenceUnused keeps the data_lifecycle import in scope if a
// future revision needs it.
var _ = fs.ValidPath
