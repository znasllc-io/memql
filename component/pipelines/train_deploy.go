package pipelines

import (
	"fmt"
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

var (
	// A migration file registers itself. A helper in the same directory does not.
	migrationRegisters = regexp.MustCompile(`\bMustRegister\b|\bRegister\(`)
	// Any of these changes what a row holds or what a table is. Case-insensitive,
	// because a statement is written either way.
	migrationChanges = regexp.MustCompile(`(?i)\b(create\s+(unlogged\s+|temp(orary)?\s+)?table|alter\s+table|drop\s+table|rename\s+table|truncate|insert\s+into|update\s+[a-z_"]|delete\s+from|create\s+(extension|type|function|trigger|view|materialized|schema|sequence|rule|policy)|alter\s+(type|extension|sequence|schema|index)|drop\s+(type|extension|function|trigger|view|materialized|schema|sequence|rule|policy|column|constraint)|add\s+column|set\s+data\s+type|grant\s|revoke\s|create_hypertable|add_retention_policy|add_compression_policy|add_continuous_aggregate_policy)\b`)
	// Adding an index is what these do, written by hand or through the helpers.
	migrationIndexes = regexp.MustCompile(`(?i)\bcreate\s+(unique\s+)?index\b|\bensurePayloadLookupIndex\(|\bensureLatestRowIndex\(|\bensure[A-Za-z]*Index\(`)
	migrationComment = regexp.MustCompile(`(?m)^\s*//.*$`)
	sqlComment       = regexp.MustCompile(`(?m)--.*$`)
)

// ClassifyMigration reads one migration's Go source. It errs towards asking: a
// migration whose text shows neither an index nor a known change is data, because
// it does something this reader cannot see (a helper with another name, SQL built at
// run time), and the cost of being wrong that way is one press of a button.
func ClassifyMigration(source string) MigrationKind {
	if !migrationRegisters.MatchString(source) {
		return MigrationNone
	}
	code := migrationComment.ReplaceAllString(source, "")
	if migrationChanges.MatchString(code) {
		return MigrationData
	}
	if migrationIndexes.MatchString(code) {
		return MigrationIndexes
	}
	return MigrationData
}

// ClassifySQLMigration reads one migration written as SQL. It registers nothing
// because being in the directory is what makes it a migration, and otherwise the same
// rule holds: what shows no index and no known change is treated as a change.
func ClassifySQLMigration(source string) MigrationKind {
	code := sqlComment.ReplaceAllString(source, "")
	if strings.TrimSpace(code) == "" {
		return MigrationNone
	}
	if migrationChanges.MatchString(code) {
		return MigrationData
	}
	if migrationIndexes.MatchString(code) {
		return MigrationIndexes
	}
	return MigrationData
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

// WorkerContractPath is the file that declares what a machine's actions accept.
const WorkerContractPath = "cockpit/internal/worker/tools/capabilities.go"

var workerContractRe = regexp.MustCompile(`"(worker[A-Za-z]*\.[a-z_]+)"\s*(?::|\]\s*=)\s*([0-9]+)`)

// WorkerContracts reads the action contracts a machine declares from its source.
func WorkerContracts(source string) map[string]string {
	out := map[string]string{}
	for _, m := range workerContractRe.FindAllStringSubmatch(source, -1) {
		out[m[1]] = m[2]
	}
	return out
}

// ChangedWorkerContracts names the contracts that differ between two sources.
func ChangedWorkerContracts(before, after string) []string {
	was, is := WorkerContracts(before), WorkerContracts(after)
	var changed []string
	for name, version := range is {
		if was[name] != version {
			changed = append(changed, fmt.Sprintf("%s %s to %s", name, orNone(was[name]), version))
		}
	}
	for name, version := range was {
		if _, ok := is[name]; !ok {
			changed = append(changed, fmt.Sprintf("%s %s removed", name, version))
		}
	}
	slices.Sort(changed)
	return changed
}

func orNone(version string) string {
	if version == "" {
		return "none"
	}
	return version
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
