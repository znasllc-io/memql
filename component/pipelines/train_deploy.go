package pipelines

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// train_deploy.go -- the decisions a deploy makes before it touches anything
// (design record docs/superpowers/specs/2026-11-02-nightly-release-from-main-design.md,
// section 7): when an automatic deploy may start, and which releases must wait for
// the owner's button. Pure functions over values; the coordinator in
// component/pipelinerun reads the files and calls these.

// InDeployWindow answers whether at falls inside the deployment's window in the
// train's zone. The window may cross midnight, and it holds its start but not its
// end. A deployment with no window has none to be in: only the button starts it.
func (s TrainSettings) InDeployWindow(at time.Time) bool {
	if s.Deployment == nil || s.Deployment.WindowStart == "" {
		return false
	}
	loc, err := time.LoadLocation(s.TimeZone)
	if err != nil {
		return false
	}
	now := at.In(loc).Format("15:04")
	start, end := s.Deployment.WindowStart, s.Deployment.WindowEnd
	if start < end {
		return now >= start && now < end
	}
	return now >= start || now < end
}

// The reasons a release waits for the owner's Deploy button.
const (
	DeployGuardData     = "tables_or_data"
	DeployGuardContract = "worker_contract"
	DeployGuardStep     = "pipeline_step_removed"
	DeployGuardUnknown  = "change_unknown"
)

// DeployGuard is one reason a release waits for the owner, as the owner reads it.
type DeployGuard struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// MigrationKind is what a migration does to the database, as far as its text shows.
type MigrationKind string

const (
	// MigrationNone is a file that holds no migration: a helper beside them.
	MigrationNone MigrationKind = "none"
	// MigrationIndexes adds lookup indexes and nothing else.
	MigrationIndexes MigrationKind = "indexes"
	// MigrationData changes tables or rows, or does something the text cannot show.
	MigrationData MigrationKind = "tables_or_data"
)

// schemaVerbs change what a table is. They are looked for across the engine's code,
// because not all of its tables are made by a migration.
const schemaVerbs = `create\s+(or\s+replace\s+)?(unlogged\s+|temp(orary)?\s+)?table|alter\s+table|drop\s+table|rename\s+table|` +
	`truncate\s|create\s+(or\s+replace\s+)?(extension|type|function|trigger|view|materialized|schema|sequence|rule|policy|procedure|aggregate)|` +
	`alter\s+(type|extension|sequence|schema|index)|drop\s+(type|extension|function|trigger|view|materialized|schema|sequence|rule|policy|column|constraint)|` +
	`add\s+column|set\s+data\s+type|create_hypertable|add_retention_policy|add_compression_policy|add_continuous_aggregate_policy`

var (
	schemaChange = regexp.MustCompile(`(?i)\b(` + schemaVerbs + `)`)
	// A migration's own text also may not touch rows.
	migrationChange = regexp.MustCompile(`(?i)\b(` + schemaVerbs + `|insert\s+into|update\s+\S+\s+set\b|delete\s+from|grant\s|revoke\s)`)
	sqlLineComment  = regexp.MustCompile(`(?m)--.*$`)
	sqlIndexOnly    = regexp.MustCompile(`(?is)^\s*(create\s+(unique\s+)?index|drop\s+index|set\s|reset\s|begin\b|commit\b|start\s+transaction)`)
	sqlCreatesIndex = regexp.MustCompile(`(?i)\bcreate\s+(unique\s+)?index\b`)
	workerContract  = regexp.MustCompile(`\b(Contract|Version|ReadContract|ReadVersion|PipelineStepContract)\s*=\s*[0-9]|"worker[A-Za-z]*\.[a-z_]+"\s*(:|\]\s*=)\s*[0-9]`)
)

// migrationHarmless are the calls an index migration makes that can change nothing:
// its bookkeeping, its logging, and reading the clock. Anything else, in this file or
// not, is something this reader cannot see into, and the release waits for the owner.
// Being wrong that way costs one press of a button, and the other way costs production.
var migrationHarmless = map[string]bool{
	"MustRegister": true, "goMigrationRegistered": true, "quoteIdentifier": true, "Sorted": true, "SplitN": true, "Equal": true,
	"Errorf": true, "Is": true, "As": true, "New": true, "Join": true, "Sprintf": true, "Info": true, "Warn": true, "Debug": true,
	"Error": true, "Default": true, "Err": true, "Done": true, "After": true, "Now": true, "Since": true, "Background": true,
	"WithTimeout": true, "WithCancel": true, "Deadline": true, "Milliseconds": true, "Format": true, "Contains": true,
	"HasPrefix": true, "HasSuffix": true, "TrimSpace": true, "string": true, "len": true, "append": true, "make": true,
}

var migrationStatements = map[string]bool{"ExecContext": true, "Exec": true, "QueryContext": true, "QueryRowContext": true, "Query": true, "QueryRow": true}

// ClassifyMigration reads one Go migration. It says indexes only when the file adds an
// index and everything else it does is visibly harmless: each statement it runs is
// written out and names an index, and each helper it calls is one that only adds an
// index. What a helper elsewhere does cannot be read here, so a file calling one is
// data, even beside an index.
func ClassifyMigration(source string) MigrationKind {
	file, err := parser.ParseFile(token.NewFileSet(), "migration.go", source, parser.SkipObjectResolution)
	if err != nil {
		return MigrationData
	}
	declared := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncDecl:
			declared[node.Name.Name] = true
		case *ast.AssignStmt:
			for at, rhs := range node.Rhs {
				if _, isFunc := rhs.(*ast.FuncLit); isFunc && at < len(node.Lhs) {
					if name, ok := node.Lhs[at].(*ast.Ident); ok {
						declared[name.Name] = true
					}
				}
			}
		}
		return true
	})
	registers, addsIndex, unseen := false, false, false
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.BasicLit:
			if node.Kind == token.STRING && migrationChange.MatchString(node.Value) {
				unseen = true
			}
		case *ast.CallExpr:
			name := calledName(node.Fun)
			switch {
			case name == "MustRegister":
				registers = true
			case migrationStatements[name]:
				named, creates := statementAboutAnIndex(node)
				unseen = unseen || !named
				addsIndex = addsIndex || creates
			case strings.Contains(name, "Index"):
				addsIndex = true
			case !declared[name] && !migrationHarmless[name]:
				unseen = true
			}
		}
		return true
	})
	if !registers {
		return MigrationNone
	}
	if unseen || !addsIndex {
		return MigrationData
	}
	return MigrationIndexes
}

func calledName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// statementAboutAnIndex reports a statement written out where it runs and about an index,
// and whether it makes one.
func statementAboutAnIndex(call *ast.CallExpr) (named, creates bool) {
	for _, arg := range call.Args {
		ast.Inspect(arg, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if strings.Contains(strings.ToLower(lit.Value), "index") {
					named = true
				}
				if sqlCreatesIndex.MatchString(lit.Value) {
					creates = true
				}
			}
			return true
		})
	}
	return named, creates
}

// ClassifySQLMigration reads one migration written as SQL: indexes only when every
// statement in it creates or drops an index, or only sets up the session.
func ClassifySQLMigration(source string) MigrationKind {
	code := sqlLineComment.ReplaceAllString(source, "")
	kind := MigrationNone
	for _, statement := range strings.Split(code, ";") {
		if strings.TrimSpace(statement) == "" {
			continue
		}
		if !sqlIndexOnly.MatchString(statement) {
			return MigrationData
		}
		kind = MigrationIndexes
	}
	return kind
}

// MigrationPath reports a file that may hold a database migration: a Go file directly
// in component/database, or a SQL file in any migrations directory.
func MigrationPath(path string) bool {
	path = strings.TrimPrefix(path, "./")
	if strings.HasSuffix(path, ".sql") {
		// A down file undoes its own up file, which is the one that says what changes.
		return strings.Contains(path, "/migrations/") && !strings.HasSuffix(path, ".down.sql")
	}
	name, found := strings.CutPrefix(path, "component/database/")
	return found && !strings.Contains(name, "/") && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
}

// ClassifyMigrationFile reads a migration by its path's kind.
func ClassifyMigrationFile(path, source string) MigrationKind {
	if strings.HasSuffix(path, ".sql") {
		return ClassifySQLMigration(source)
	}
	return ClassifyMigration(source)
}

// EngineCodePath reports a file that runs inside the engine, where a table can be made
// without any migration. A machine's own database and a client's are not the engine's.
func EngineCodePath(path string) bool {
	if !strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, ".sql") {
		return false
	}
	if strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, ".down.sql") || strings.Contains(path, "/testdata/") {
		return false
	}
	for _, outside := range []string{"docs/", "clients/", "cockpit/", "test/", "sdk/", "scripts/", "cmd/"} {
		if strings.HasPrefix(path, outside) {
			return false
		}
	}
	return true
}

// WorkerContractPath reports a file that says what a machine's actions accept: the
// engine asks for an exact version, and a machine that lacks it stops getting work.
func WorkerContractPath(path string) bool {
	if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
		return false
	}
	for _, prefix := range []string{"integrations/agent/worker/", "core/computerpermission/", "cockpit/internal/worker/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// PatchLines is what a unified diff adds or removes, comments left out: prose about a
// table changes no table.
func PatchLines(patch string) []string {
	var out []string
	for _, line := range strings.Split(patch, "\n") {
		if line == "" || (line[0] != '+' && line[0] != '-') || strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
			continue
		}
		if code := strings.TrimSpace(line[1:]); code != "" && !strings.HasPrefix(code, "//") && !strings.HasPrefix(code, "--") {
			out = append(out, code)
		}
	}
	return out
}

// SchemaChanges are the lines that change a table.
func SchemaChanges(lines []string) []string { return matching(lines, schemaChange) }

// ContractChanges are the lines that state a worker action contract.
func ContractChanges(lines []string) []string { return matching(lines, workerContract) }

func matching(lines []string, pattern *regexp.Regexp) []string {
	var out []string
	for _, line := range lines {
		if pattern.MatchString(line) {
			out = append(out, line)
		}
	}
	return out
}

// DifferingLines is what PatchLines would say of a file GitHub gave no patch for: the
// lines one side has and the other does not, read from both sides whole.
func DifferingLines(before, after string) []string {
	count := map[string]int{}
	for _, line := range PatchLines(prefixed("-", before)) {
		count[strings.TrimPrefix(line, "-")]--
	}
	for _, line := range PatchLines(prefixed("+", after)) {
		count[strings.TrimPrefix(line, "+")]++
	}
	var out []string
	for line, n := range count {
		if n != 0 {
			out = append(out, line)
		}
	}
	sort.Strings(out)
	return out
}

func prefixed(mark, source string) string {
	lines := strings.Split(source, "\n")
	for n := range lines {
		lines[n] = mark + lines[n]
	}
	return strings.Join(lines, "\n")
}

// RemovedPipelineSteps names the steps before declares and after does not. The
// pipeline must not be able to switch off its own tests (spec D10), and a step moved
// to another stage reads as removed and added: asking the owner there is cheap.
func RemovedPipelineSteps(before, after []string) []string {
	var removed []string
	for _, step := range before {
		if !slices.Contains(after, step) {
			removed = append(removed, step)
		}
	}
	sort.Strings(removed)
	return removed
}

// HasDataGuard reports a release that changes tables or data, or may: it never rolls
// back by itself, because the code before it is not proven against the database
// after it.
func HasDataGuard(guards []DeployGuard) bool {
	return slices.ContainsFunc(guards, func(g DeployGuard) bool {
		return g.Code == DeployGuardData || g.Code == DeployGuardUnknown
	})
}

// EngineRefVersion reads an installation's pinned engine tag from its product.env.
func EngineRefVersion(productEnv string) string {
	for _, line := range strings.Split(productEnv, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "ENGINE_REF="); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// VersionAfter reports a dotted version later than another. A version that does not
// parse is later than nothing.
func VersionAfter(version, other string) bool {
	a := trainVersionRe.FindStringSubmatch(strings.TrimPrefix(version, "v"))
	b := trainVersionRe.FindStringSubmatch(strings.TrimPrefix(other, "v"))
	if a == nil || b == nil {
		return false
	}
	for i := 1; i < len(a); i++ {
		x, errX := strconv.Atoi(a[i])
		y, errY := strconv.Atoi(b[i])
		if errX != nil || errY != nil {
			return false
		}
		if x != y {
			return x > y
		}
	}
	return false
}
