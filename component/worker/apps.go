package worker

import (
	"sort"
	"strconv"
	"strings"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
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
	// AppIdClaudeCode is Claude Code's app id.
	AppIdClaudeCode = "claude-code"
	// AppIdCodex is Codex's app id.
	AppIdCodex = "codex"

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
func IsKnownAppId(id string) bool {
	switch strings.TrimSpace(id) {
	case AppIdClaudeCode, AppIdCodex:
		return true
	}
	return false
}

// KnownAppIds returns the closed runnable set, sorted.
func KnownAppIds() []string {
	return []string{AppIdClaudeCode, AppIdCodex}
}

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
