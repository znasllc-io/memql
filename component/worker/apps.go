package worker

import (
	"sort"
	"strconv"
	"strings"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/airoute"
)

// apps.go owns the local-app inventory a cockpit reports and the
// routing labels the engine derives from it (memql#4359).
//
// Two rules carry the whole design, and both are deliberate:
//
//   - The RUNNABLE set is closed in the engine. A cockpit may report
//     any app id it likes; ids outside AppIdClaudeCode / AppIdCodex
//     are stored on the registration and produce no label, so a newer
//     cockpit never makes the engine attempt something it has no
//     protocol for. Growing the set is a value change here, not a
//     wire change.
//
//   - A label is derived only from an app that is BOTH `allowed` (the
//     machine's own policy.yaml apps.allow) and `signedIn`. Selection
//     therefore cannot pick a machine that would refuse the run --
//     the alternative is a dispatch that fails on the far side after
//     the plan has already committed to it.
const (
	// AppIdClaudeCode is Claude Code's app id. The value is the shared
	// routing vocabulary's: the closed set is declared ONCE, in
	// core/airoute, so the DSL parser can refuse a policy entry naming an
	// app the engine does not drive without importing this package -- which
	// it cannot, because component/worker imports component/language.
	AppIdClaudeCode = airoute.AppClaudeCode
	// AppIdCodex is Codex's app id.
	AppIdCodex = airoute.AppCodex

	// AppLabelPrefix prefixes every derived routing label. A label is
	// "app:claude-code" => "2.1" -- the value is the app's major.minor
	// so a require-label can pin a floor without pinning a patch.
	AppLabelPrefix = "app:"

	// SubscriptionUnknown / None / Present are the closed set for what
	// an app reports about its own subscription. The engine never
	// infers this from anything else.
	SubscriptionUnknown = "unknown"
	SubscriptionNone    = "none"
	SubscriptionPresent = "present"

	// maxReportedApps bounds the inventory a single registration can
	// carry, so a malformed cockpit cannot grow the row without limit.
	maxReportedApps = 32
	// maxAppFieldLen bounds each reported string field.
	maxAppFieldLen = 200
)

// AppInfo is one local app on a cockpit machine.
type AppInfo struct {
	Id           string `json:"id"`
	Version      string `json:"version"`
	SignedIn     bool   `json:"signedIn"`
	Subscription string `json:"subscription"`
	Allowed      bool   `json:"allowed"`
}

// Runnable reports whether the engine can drive this app: the id is
// one the engine knows AND the machine will actually run it.
func (a AppInfo) Runnable() bool {
	return IsKnownAppId(a.Id) && a.Allowed && a.SignedIn
}

// IsKnownAppId reports whether id is in the engine's closed runnable
// set. Unknown ids are stored, never driven.
func IsKnownAppId(id string) bool { return airoute.IsRunnableApp(id) }

// KnownAppIds returns the closed runnable set, sorted.
func KnownAppIds() []string { return airoute.RunnableApps() }

// NormalizeSubscription clamps a reported subscription value to the
// closed set. Anything unrecognised -- including empty -- reads as
// "unknown", which is the honest answer rather than "none".
func NormalizeSubscription(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case SubscriptionNone:
		return SubscriptionNone
	case SubscriptionPresent:
		return SubscriptionPresent
	}
	return SubscriptionUnknown
}

// AppsFromProto converts the wire inventory to the internal shape.
// Entries with an empty id are dropped; the result is sorted by id so
// the persisted row and the derived labels are stable across beats
// (an unstable order would rewrite the registration on every
// heartbeat for no change).
func AppsFromProto(in []*memqlv1.AppInfo) []AppInfo {
	if len(in) == 0 {
		return nil
	}
	out := make([]AppInfo, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, a := range in {
		if a == nil {
			continue
		}
		id := truncate(strings.TrimSpace(a.GetId()), maxAppFieldLen)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, AppInfo{
			Id:           id,
			Version:      truncate(strings.TrimSpace(a.GetVersion()), maxAppFieldLen),
			SignedIn:     a.GetSignedIn(),
			Subscription: NormalizeSubscription(a.GetSubscription()),
			Allowed:      a.GetAllowed(),
		})
		if len(out) >= maxReportedApps {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}

// AppLabels derives the routing labels for an inventory. Only
// runnable apps produce one; the value is major.minor of the
// reported version, or the empty string when the app reported no
// parseable version (a label whose value is "" still matches a
// require of {"app:codex": ""}, which is the "any version" ask).
func AppLabels(apps []AppInfo) map[string]string {
	if len(apps) == 0 {
		return nil
	}
	out := make(map[string]string, len(apps))
	for _, a := range apps {
		if !a.Runnable() {
			continue
		}
		out[AppLabelPrefix+a.Id] = MajorMinor(a.Version)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// AppLabelKey is the routing label key for an app id.
func AppLabelKey(appId string) string { return AppLabelPrefix + appId }

// MajorMinor reduces a version string to "<major>.<minor>". It
// tolerates a leading "v" and any suffix ("2.1.4-beta" -> "2.1"),
// and returns "" when there is no leading number at all -- the
// caller treats that as "version unknown", never as "version 0".
func MajorMinor(version string) string {
	v := strings.TrimSpace(version)
	v = strings.TrimPrefix(v, "v")
	if v == "" {
		return ""
	}
	parts := strings.SplitN(v, ".", 3)
	major := leadingDigits(parts[0])
	if major == "" {
		return ""
	}
	if len(parts) == 1 {
		return major
	}
	minor := leadingDigits(parts[1])
	if minor == "" {
		return major
	}
	return major + "." + minor
}

func leadingDigits(s string) string {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return ""
	}
	// Strip a leading zero run so "01" and "1" compare equal.
	if n, err := strconv.Atoi(s[:end]); err == nil {
		return strconv.Itoa(n)
	}
	return s[:end]
}

// mergeAppLabels returns base with every app label replaced by the
// ones derived from apps. Labels the operator set by hand survive;
// stale app labels from a previous beat do not, which is what makes
// signing OUT of an app remove the machine from selection.
func mergeAppLabels(base map[string]string, apps []AppInfo) map[string]string {
	out := make(map[string]string, len(base)+len(apps))
	for k, v := range base {
		if strings.HasPrefix(k, AppLabelPrefix) {
			continue
		}
		out[k] = v
	}
	for k, v := range AppLabels(apps) {
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// appsEqual reports whether two inventories are identical. Used to
// skip a registration rewrite when a heartbeat re-reports what the
// row already says.
func appsEqual(a, b []AppInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// -----------------------------------------------------------------------------
// App descriptors (epic memql#5096, design D8)
// -----------------------------------------------------------------------------

// Harness words: HOW the cockpit drives an app.
//
// The set is closed here for the reason the app-id set is closed: it names a
// protocol the ENGINE has to understand, and an id the engine has no protocol
// for must not become a routing decision. What the closure does NOT do is
// refuse a registration -- an unrecognised word leaves the descriptor unused,
// exactly as an unrecognised app id leaves the entry unlabelled.
//
// The word is reported per MACHINE, never inferred from the app id: the
// cockpit picks codex-app-server only when the binary answers
// `codex app-server --help`, and falls back to codex-mcp otherwise. Two
// machines can therefore report different harnesses for the same app, which
// is a fact about those machines rather than a disagreement.
const (
	// HarnessClaudeHeadless is one `claude -p` process per turn, resumed by
	// session ref, streaming JSON events.
	HarnessClaudeHeadless = "claude-headless"
	// HarnessCodexAppServer is `codex app-server`: a JSON-RPC protocol with
	// thread continuation, structured events and usage.
	HarnessCodexAppServer = "codex-app-server"
	// HarnessCodexMCP is `codex mcp-server`'s two tools, the fallback where
	// the app-server is not available.
	HarnessCodexMCP = "codex-mcp"
)

// AppDescriptor is HOW one app on a machine is driven, as that machine
// reported it at registration.
type AppDescriptor struct {
	Id               string `json:"id"`
	Harness          string `json:"harness"`
	StructuredResult bool   `json:"structuredResult"`
	FollowUps        bool   `json:"followUps"`
}

// IsKnownHarness reports whether h is a protocol this engine understands.
func IsKnownHarness(h string) bool {
	switch strings.TrimSpace(h) {
	case HarnessClaudeHeadless, HarnessCodexAppServer, HarnessCodexMCP:
		return true
	}
	return false
}

// KnownHarnesses returns the closed harness set, sorted.
func KnownHarnesses() []string {
	return []string{HarnessClaudeHeadless, HarnessCodexAppServer, HarnessCodexMCP}
}

// Valid reports whether this descriptor is one the engine can act on: a known
// app AND a known harness. Both halves matter -- a known app driven by an
// unknown protocol is exactly as unusable as an unknown app.
func (d AppDescriptor) Valid() bool {
	return IsKnownAppId(d.Id) && IsKnownHarness(d.Harness)
}

// AppDescriptorsFromProto converts the wire descriptors, DROPPING every entry
// the engine cannot act on and de-duplicating by id. Sorted by id so the
// persisted row is stable across reconnects.
func AppDescriptorsFromProto(in []*memqlv1.AppDescriptor) []AppDescriptor {
	if len(in) == 0 {
		return nil
	}
	out := make([]AppDescriptor, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, d := range in {
		if d == nil {
			continue
		}
		desc := AppDescriptor{
			Id:               truncate(strings.TrimSpace(d.GetId()), maxAppFieldLen),
			Harness:          truncate(strings.TrimSpace(d.GetHarness()), maxAppFieldLen),
			StructuredResult: d.GetStructuredResult(),
			FollowUps:        d.GetFollowUps(),
		}
		if !desc.Valid() {
			continue
		}
		if _, dup := seen[desc.Id]; dup {
			continue
		}
		seen[desc.Id] = struct{}{}
		out = append(out, desc)
		if len(out) >= maxReportedApps {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}

// DescriptorFor finds the descriptor for an app id.
//
// The `ok` is load-bearing: an app that reported no descriptor is not the
// same as one whose harness does neither. A zero descriptor read as an answer
// would tell the engine "no structured result, no follow-ups" about a machine
// that simply predates the field, and the engine would then refuse a turn the
// machine could have served.
func DescriptorFor(descriptors []AppDescriptor, appId string) (AppDescriptor, bool) {
	appId = strings.TrimSpace(appId)
	for _, d := range descriptors {
		if d.Id == appId {
			return d, true
		}
	}
	return AppDescriptor{}, false
}

// AppProviderReferences returns the `app:<id>` provider names for the closed
// runnable set (design D10).
//
// `app:` names one vocabulary in two places on purpose: a policy naming
// `app:claude-code` and a machine advertising the routing label
// `app:claude-code` are talking about one thing. The two never MEET in code --
// a provider reference resolves through the registry, a label lives on a
// registration -- so this function is what keeps the id set behind both a
// single list rather than two that drift.
func AppProviderReferences() []string {
	ids := KnownAppIds()
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, AppLabelPrefix+id)
	}
	return out
}
