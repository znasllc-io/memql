#!/usr/bin/env bash
#
# scripts/dev/fuzz-seeds.sh
# =========================
#
# Writes the committed fuzz seed corpus (memql#5387).
#
# Go reads seeds from testdata/fuzz/<Target>/ as files in the `go test fuzz v1`
# format: a version line, then one typed literal per f.Fuzz parameter. Every
# target here takes a single string, so each file is two lines:
#
#     go test fuzz v1
#     string("query q { filter row => row.a == \"x\" }")
#
# WHY A SCRIPT AND NOT FIFTY HAND-WRITTEN FILES. A malformed seed file is
# IGNORED BY GO WITH NO WARNING -- no error, no skip line, nothing. A hand
# edit that loses a backslash therefore removes a seed silently, and the only
# evidence is a subtest count nobody is watching. Writing them from one
# printf, from literals held in one place, removes that whole class. The
# format is verified by COUNTING subtests before and after, not by reading the
# files.
#
# WHAT IS SEEDED, AND WHAT DELIBERATELY IS NOT. The conformance corpus is 529
# .memql files, and FuzzParse (test/conformance/fuzz_test.go) already feeds
# every one of them to the parser at run time through f.Add. Committing them
# again, per target, would be ~2000 near-duplicate files -- the "corpus that
# becomes a burden" this epic's design record warns about. So what is
# committed here is small and CHOSEN: the shapes a lexer is known to get
# wrong, every retired expression spelling paired with the form that replaces
# it, and the absence table the lowering and the evaluator must agree about.
#
# THIS SCRIPT ONLY ADDS. It never cleans a target directory: those directories
# also hold the inputs a fuzz run FOUND, written there by Go itself and
# committed beside the fix, and deleting one would throw away the only record
# that a bug ever existed. Re-running is idempotent for the seeds it owns.
#
# Per repo convention (CLAUDE.md): function-based structure, main() at the
# bottom.

set -euo pipefail

REPO_ROOT=""

readonly LEXER_DIR="component/language/parser/testdata/fuzz/FuzzLexer"
readonly EXPR_DIR="component/language/parser/testdata/fuzz/FuzzParseV1Expression"
readonly LOWER_DIR="component/memql/testdata/fuzz/FuzzLower"
readonly EVAL_DIR="component/memql/testdata/fuzz/FuzzEvalExpr"

# -----------------------------------------------------------------
# Writing one seed
# -----------------------------------------------------------------

# write_seed <dir> <slug> <go-quoted-literal>
#
# The third argument is the literal EXACTLY as it appears inside string(...),
# surrounding quotes included, so it is passed through single quotes at the
# call site and bash never touches a backslash. Go parses it with
# strconv.Unquote, which means every escape must be one Go accepts: \x for a
# raw byte, \u for a code point, and NO lone surrogate (\ud800 is not a valid
# Go escape -- write the CESU-8 bytes with \x instead).
function write_seed() {
	local dir="$1" slug="$2" literal="$3"
	mkdir -p "$dir"
	printf 'go test fuzz v1\nstring(%s)\n' "$literal" >"$dir/seed-$slug"
}

# repeat <unit> <count> -- prints unit, count times.
function repeat() {
	local unit="$1" count="$2" out="" i
	for ((i = 0; i < count; i++)); do
		out+="$unit"
	done
	printf '%s' "$out"
}

# -----------------------------------------------------------------
# FuzzLexer -- the shapes a lexer gets wrong
# -----------------------------------------------------------------

function seed_lexer() {
	local d="$LEXER_DIR"

	write_seed "$d" empty '""'
	# A NUL in source. grep SKIPS a file containing one, so a defect reachable
	# only through this byte hides from every text-based gate in the repo.
	write_seed "$d" nul '"query q {\x00}"'
	# The lexer must not run off the end of the input.
	write_seed "$d" unterminated-string '"query q { filter row => row.a == \"x }"'
	write_seed "$d" unterminated-comment '"/* query q {"'
	write_seed "$d" nested-comment '"/* /* */ */"'
	# Line endings, which position reporting counts.
	write_seed "$d" crlf '"query q {\r\n}"'
	# A byte-order mark before the first token.
	write_seed "$d" bom '"\xef\xbb\xbfquery q {}"'
	# A lone surrogate inside a string literal: bytes that are not a
	# character. Written with \x because \ud800 is not a legal Go escape.
	write_seed "$d" lone-surrogate '"\"\xed\xa0\x80\""'
	# Invalid UTF-8 as the whole input, with no string literal around it.
	write_seed "$d" invalid-utf8 '"\xff\xfe"'
	# Stack depth.
	local opens closes
	opens="$(repeat '(' 50)"
	closes="$(repeat ')' 50)"
	write_seed "$d" deep-nesting "\"${opens}1${closes}\""
	# Buffer growth.
	write_seed "$d" long-identifier "\"$(repeat a 10000)\""
	# A grapheme that is two runes (e + COMBINING ACUTE ACCENT).
	write_seed "$d" combining-marks '"row.é"'
	# A bidi override inside an identifier.
	write_seed "$d" rtl '"row.‮abc"'
	write_seed "$d" tab-indent '"query\tq\t{\t}"'
	# A dotted path the lexer FUSES into ONE identifier token: 4096 segments
	# in a single token, which is the shape the chain bound counts
	# (component/language/parser/chain_bound.go).
	write_seed "$d" long-dotted-path "\"args$(repeat '.a' 4096)\""
	# Operators with no operands, including the maximal-munch traps.
	write_seed "$d" only-operators '"&&||!==>=<=??"'
	# The `=>` glyph's tokenisation, three ways.
	write_seed "$d" lambda-arrow-spaced '"row => row.a"'
	write_seed "$d" lambda-arrow-tight '"row=>row.a"'
	write_seed "$d" lambda-arrow-split '"row = > row.a"'
}

# -----------------------------------------------------------------
# FuzzParseV1Expression -- every retirement, beside its replacement
# -----------------------------------------------------------------
#
# Each pair is one refusal the edition owes an author a sentence about (D24).
# Seeding BOTH halves is the point: the retired form starts the fuzzer next to
# the refusal path, and the replacement starts it next to the accepted form
# the refusal names -- which is also the half whose parse/print fixed point
# the target checks.

function seed_parser_expressions() {
	local d="$EXPR_DIR"

	write_seed "$d" retired-has '"row => row.tags has \"x\""'
	write_seed "$d" replacement-in '"row => \"x\" in row.tags"'

	write_seed "$d" retired-not-in '"row => row.status not in [\"a\"]"'
	write_seed "$d" replacement-negated-in '"row => !(row.status in [\"a\"])"'

	write_seed "$d" retired-cond '"row => cond(row.urgent, 1, 2)"'
	write_seed "$d" replacement-ternary '"row => row.urgent ? 1 : 2"'

	write_seed "$d" retired-coalesce '"row => coalesce(row.title, \"b\")"'
	write_seed "$d" replacement-nullish '"row => row.title ?? \"b\""'

	write_seed "$d" retired-semicolon '"row => row.a == 1 ; row.b == 2"'
	write_seed "$d" retired-comma '"row => row.a == 1 , row.b == 2"'
	write_seed "$d" replacement-and '"row => row.a == 1 && row.b == 2"'

	write_seed "$d" retired-null '"row => row.a == null"'
	write_seed "$d" replacement-nil '"row => row.a == nil"'

	write_seed "$d" retired-optional-chain '"row => ?.status == args.status"'
	write_seed "$d" replacement-safe-path '"row => row.?lineage.planId == args.id"'

	write_seed "$d" retired-object-literal-call '"row => f({k: 1})"'
	write_seed "$d" replacement-named-args '"row => f(k: 1)"'

	write_seed "$d" retired-bare-concept-id '"row => row.concept == v1:crm:lead"'
	write_seed "$d" replacement-quoted-concept-id '"row => row.concept == \"v1:crm:lead\""'

	write_seed "$d" retired-when-guard '"row => when(args.x) { row.f == args.x }"'
	write_seed "$d" replacement-optional-guard '"row => (args.x == nil || row.f == args.x)"'

	write_seed "$d" retired-contains '"row => contains(row.title, \"sub\")"'
	write_seed "$d" replacement-includes '"row => row.title.includes(\"sub\")"'

	write_seed "$d" retired-exists-len-count '"row => exists(row.a) && len(row.tags) > count(row.nums)"'
	write_seed "$d" replacement-nil-check-count '"row => row.a != nil && row.tags.count() > 0"'

	write_seed "$d" retired-dollar-args '"row => $args.x == null"'
	write_seed "$d" replacement-args '"row => args.x == nil"'

	write_seed "$d" retired-concat '"row => concat(row.a, row.b) == \"ab\""'
	write_seed "$d" replacement-plus '"row => row.a + row.b == \"ab\""'

	# A filter written with no lambda header at all: the retirement an author
	# meets first, because it is what every pre-edition query looked like.
	write_seed "$d" retired-headerless-filter '"row.status == args.status"'
	write_seed "$d" replacement-lambda-filter '"row => row.status == args.status"'

	# Both sides of the chain bound (MaxExpressionChain, 4096, in
	# component/language/parser/chain_bound.go). A chain is built in a LOOP,
	# so the parser never recurses while reading one and the nesting bound
	# does not see it -- but the tree is as tall as the chain and every walk
	# over it recurses. The seed AT the bound is the one that matters most:
	# this target parses, prints and re-parses, so it exercises the recursive
	# printer and the recursive dump over a tree 4096 nodes tall. The seed one
	# link PAST it must be refused, which is the fuzzer starting beside the
	# refusal path. The numbers are the bound; if the constant moves, move
	# them with it.
	local chain_at chain_past
	chain_at="$(repeat '.a' 4096)"
	chain_past="$(repeat '.a' 4097)"
	write_seed "$d" chain-at-the-bound "\"args${chain_at}\""
	write_seed "$d" chain-past-the-bound "\"args${chain_past}\""
}

# -----------------------------------------------------------------
# FuzzLower / FuzzEvalExpr -- the absence table
# -----------------------------------------------------------------
#
# A missing field, JSON null, nil and "" are ONE unset value in == and !=, and
# != is null-safe. That rule has two implementations -- the SQL lowering and
# the in-process evaluator -- and the differential lane exists because they
# can disagree. These seeds put both fuzzers on the rows where they would.
#
# The two targets take DIFFERENT input shapes, and this is the trap the plan's
# snippet walked into: FuzzLower unwraps a one-parameter lambda and lowers its
# body, so `row => ...` is the right seed there; FuzzEvalExpr evaluates what
# it parses, so a `row => ...` seed evaluates to a lambda VALUE and exercises
# none of the absence rules. Its seeds are bare expressions over the fixed
# scope in expr_eval_fuzz_test.go.

function seed_lowering() {
	local d="$LOWER_DIR"

	write_seed "$d" absent-eq-nil '"row => row.title == nil"'
	write_seed "$d" absent-ne-empty '"row => row.title != \"\""'
	write_seed "$d" absent-in-empty-list '"row => row.status in []"'
	write_seed "$d" absent-starts-with-empty '"row => row.title startsWith \"\""'
	write_seed "$d" absent-safe-path-nil '"row => row.?lineage.planId == nil"'
	write_seed "$d" absent-count-zero '"row => row.tags.count() == 0"'
	write_seed "$d" absent-negated-bool '"row => !row.urgent"'
	write_seed "$d" absent-open-block-key '"row => row.extras.known == nil"'
	write_seed "$d" absent-untyped-field '"row => row.blob == nil"'
	write_seed "$d" absent-map-key '"row => row.labels.anything != \"x\""'
	write_seed "$d" absent-required-nested '"row => row.settings.mode == nil"'
	write_seed "$d" optional-arg-guard '"row => (args.status == nil || row.status == args.status)"'
	write_seed "$d" starts-with-list '"row => row.email startsWith [\"a@\", \"\"]"'
	write_seed "$d" membership-literal-in-field '"row => \"a\" in row.tags"'
	write_seed "$d" numeric-bounds '"row => row.priority >= args.n && row.score <= 1.5"'
	write_seed "$d" float-equality '"row => row.overage != row.reported"'
	write_seed "$d" datetime-compare '"row => row.dueAt < now"'
	write_seed "$d" nested-connectives '"row => (row.status == \"open\" || row.urgent) && !(row.title startsWith \"INC-\")"'
}

function seed_evaluation() {
	local d="$EVAL_DIR"

	write_seed "$d" absent-eq-nil '"row.value == nil"'
	write_seed "$d" absent-ne-empty '"row.value != \"\""'
	write_seed "$d" absent-in-empty-list '"row.value in []"'
	write_seed "$d" absent-starts-with-empty '"row.value startsWith \"\""'
	write_seed "$d" absent-safe-path-nil '"row.?lineage.planId == nil"'
	write_seed "$d" absent-count-zero '"row.tags.count() == 0"'
	write_seed "$d" absent-negated-bool '"!row.flag"'
	write_seed "$d" absent-field-eq-absent-field '"row.flag == row.missing"'
	write_seed "$d" absent-arg-nullish '"args.null ?? args.missing ?? \"fallback\""'
	write_seed "$d" absent-blank-coalesce '"args.title ?? \"untitled\""'
	write_seed "$d" absent-in-list-of-absences '"nil in [\"\", nil]"'
	write_seed "$d" absent-compare-orders '"row.value < \"a\" && row.value >= nil"'
	write_seed "$d" optional-arg-guard '"args.status == nil || row.status == args.status"'
	write_seed "$d" membership-literal-in-field '"\"a\" in row.tags"'
	write_seed "$d" empty-collection-methods '"args.none.count() + args.none.where(i => true).count()"'
	write_seed "$d" nested-connectives '"(row.status == \"open\" || row.value != nil) && !(row.title startsWith \"INC-\")"'
}

# -----------------------------------------------------------------

function main() {
	REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
	cd "$REPO_ROOT"
	seed_lexer
	seed_parser_expressions
	seed_lowering
	seed_evaluation
	echo "SUCCESS: fuzz seeds written to ${LEXER_DIR}, ${EXPR_DIR}, ${LOWER_DIR} and ${EVAL_DIR}"
	echo "INFO: verify Go is reading them by counting subtests, not by reading the files:"
	echo "INFO:   go test ./component/language/parser/ -run 'FuzzLexer|FuzzParseV1Expression' -v | grep -c '^=== RUN   Fuzz'"
}

main "$@"
