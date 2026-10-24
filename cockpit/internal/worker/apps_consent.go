package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/znasllc-io/memql/cockpit/internal/worker/apps"
	"github.com/znasllc-io/memql/cockpit/internal/worker/inference"
	"github.com/znasllc-io/memql/cockpit/internal/worker/tools"
)

// `memql worker apps --allow <app> [--deny <app>] [--home <cluster>]`.
//
// App consent is the machine owner's word about ONE cluster
// (policy.yaml apps.homes.<home>.allow, tools.AppsPolicy), and this is the
// way to give or withdraw it without knowing the file, the key and the
// signal. It is `memql worker models --allow` generalised: the same
// comment-preserving textual edit (inference.MergeList / RemoveFromList),
// then the same SIGHUP to the running worker (inference.Readvertise).
//
// A SIGHUP IS ENOUGH HERE, which it is not for a model. The app inventory
// rides every Heartbeat and reads the policy on every beat, so a cluster
// sees an app allowed or withdrawn on the worker's next beat -- no
// reconnect -- and the session gate reads it at the next session start.
//
// THE CLUSTER IS NAMED, OR THERE IS ONLY ONE. A machine enrolled with two
// clusters is refused with the command for each, the way the uninstaller
// refuses an ambiguous scope: guessing would hand one cluster a consent
// the owner meant for another, which is the crossing per-cluster consent
// exists to rule out.

// appConsentPath is where one cluster's app consent lives in policy.yaml.
func appConsentPath(homeKey string) []string {
	return []string{"apps", "homes", homeKey, "allow"}
}

// retiredAppsAllowPath is the machine-wide list apps.homes replaced.
var retiredAppsAllowPath = []string{"apps", "allow"}

// appConsentChange is what an edit did beyond the list it was asked about.
type appConsentChange struct {
	// retired is what the removed machine-wide apps.allow listed.
	retired []string
	// retiredErr is why a retired list that is still there could not be
	// removed. The consent edit stands either way: the list allows
	// nothing, so leaving it costs a warning, not a permission.
	retiredErr error
}

// editAppConsent returns body with allow added to, and deny removed from,
// one cluster's apps.homes entry -- and the retired machine-wide apps.allow
// removed in the same edit, which is how a file written before consent was
// per cluster is migrated. A pure function of the bytes, like the editor
// it composes, so the whole decision is asserted on strings.
func editAppConsent(body, home string, allow, deny []string) (string, bool, appConsentChange, error) {
	var change appConsentChange
	path := appConsentPath(homeKeyIn(body, home))

	out, added, err := inference.MergeList(body, path, allow)
	if err != nil {
		return "", false, change, err
	}
	out, removed, err := inference.RemoveFromList(out, path, deny)
	if err != nil {
		return "", false, change, err
	}
	withoutRetired, retired, dropped, err := inference.RemoveKey(out, retiredAppsAllowPath)
	switch {
	case err != nil:
		change.retiredErr = err
	case dropped:
		out = withoutRetired
		change.retired = retired
	}
	return out, added || removed || dropped, change, nil
}

// homeKeyIn is the spelling the file already uses for home, when it has an
// entry for it: apps.homes is read without regard to case, so writing a
// second spelling beside the first would split one cluster's consent over
// two entries.
func homeKeyIn(body, home string) string {
	var doc yaml.Node
	if yaml.Unmarshal([]byte(body), &doc) != nil || len(doc.Content) != 1 {
		return home
	}
	homes := yamlChild(yamlChild(doc.Content[0], "apps"), "homes")
	if homes == nil || homes.Kind != yaml.MappingNode {
		return home
	}
	for i := 0; i+1 < len(homes.Content); i += 2 {
		if key := homes.Content[i].Value; strings.EqualFold(strings.TrimSpace(key), strings.TrimSpace(home)) {
			return key
		}
	}
	return home
}

func yamlChild(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// resolveConsentHome decides which cluster --allow/--deny apply to: the one
// --home names (by its id, or its cluster URL), or the only one there is.
func resolveConsentHome(homes []Home, flag string, allow, deny []string) (string, error) {
	flag = strings.TrimSpace(flag)
	if len(homes) == 0 {
		return "", errors.New("this machine is not enrolled with any cluster, so there is no cluster to allow an app for; " +
			"pair it first (memql worker pair), then run this again")
	}
	if flag == "" {
		if len(homes) == 1 {
			return homes[0].ID, nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "this machine is enrolled with %d clusters, and consent is given to one at a time; say which with --home:", len(homes))
		for _, h := range homes {
			b.WriteString("\n  " + appsCommand(allow, deny, h.ID))
		}
		return "", errors.New(b.String())
	}
	for _, h := range homes {
		if strings.EqualFold(strings.TrimSpace(h.ID), flag) {
			return h.ID, nil
		}
	}
	for _, h := range homes {
		if sameClusterURL(h.ClusterURL, flag) {
			return h.ID, nil
		}
	}
	ids := make([]string, 0, len(homes))
	for _, h := range homes {
		ids = append(ids, h.ID)
	}
	return "", fmt.Errorf("this machine is not enrolled with %q; its clusters are %s", flag, strings.Join(ids, ", "))
}

// appsCommand is the command line that makes this change for one cluster.
func appsCommand(allow, deny []string, home string) string {
	parts := []string{"memql worker apps"}
	for _, id := range allow {
		parts = append(parts, "--allow "+id)
	}
	for _, id := range deny {
		parts = append(parts, "--deny "+id)
	}
	return strings.Join(append(parts, "--home "+home), " ")
}

// appConsentRequest is `worker apps --allow/--deny`, parsed.
type appConsentRequest struct {
	policyPath  string
	homes       []Home
	home        string
	allow, deny []string
}

// runAppConsent makes the edit, says what the cluster's consent now is,
// and signals the running worker -- only when something changed. The
// signal is a seam (inference.Readvertise in the command) so the whole
// flow is testable without a worker to signal.
func runAppConsent(ctx context.Context, w io.Writer, req appConsentRequest, readvertise func(context.Context) error) error {
	allow, deny := cleanAppIDs(req.allow), cleanAppIDs(req.deny)
	for _, id := range append(append([]string(nil), allow...), deny...) {
		if !apps.IsKnownID(id) {
			return &SetupError{Code: SetupExitUsage, Msg: fmt.Sprintf(
				"this cockpit drives no app called %q (it drives %s)", id, knownApps())}
		}
	}
	for _, id := range allow {
		for _, other := range deny {
			if id == other {
				return &SetupError{Code: SetupExitUsage, Msg: fmt.Sprintf(
					"%s is both --allow and --deny; say which you mean", id)}
			}
		}
	}
	home, err := resolveConsentHome(req.homes, req.home, allow, deny)
	if err != nil {
		return &SetupError{Code: SetupExitUsage, Msg: err.Error()}
	}

	var changed bool
	var change appConsentChange
	err = inference.EditPolicy(req.policyPath, func(body string) (string, bool, error) {
		out, c, ch, err := editAppConsent(body, home, allow, deny)
		changed, change = c, ch
		return out, c, err
	})
	if err != nil {
		return setupFailed("%v", err)
	}

	// What the worker will read, from the file as written -- not the
	// command line restated, so an edit that did not land cannot be
	// reported as one that did.
	var now []string
	if p, err := tools.LoadPolicy(req.policyPath); err == nil {
		now = p.AppsAllowFor(home)
	}
	state := "no app"
	if len(now) > 0 {
		state = strings.Join(now, ", ")
	}
	if !changed {
		fmt.Fprintf(w, "Nothing to change: %s already allows %s in %s.\n\n", home, state, req.policyPath)
		return nil
	}
	fmt.Fprintf(w, "%s now allows %s (%s).\n", home, state, req.policyPath)
	if len(change.retired) > 0 {
		fmt.Fprintf(w, "Removed the machine-wide apps.allow (%s): consent is per cluster now, and that list allowed nothing anywhere. "+
			"Allow an app for another cluster with --home.\n", strings.Join(change.retired, ", "))
	}
	if change.retiredErr != nil {
		fmt.Fprintf(w, "The machine-wide apps.allow is still in the file and allows nothing anywhere: %v\n", change.retiredErr)
	}
	if err := readvertise(ctx); err != nil {
		fmt.Fprintln(w, capitalise(err.Error())+".")
	} else {
		fmt.Fprintln(w, "The running worker was signalled; the cluster sees this on the worker's next heartbeat.")
	}
	fmt.Fprintln(w)
	return nil
}

// cleanAppIDs trims, lowercases and de-duplicates, keeping the order typed.
func cleanAppIDs(ids []string) []string {
	var out []string
	for _, id := range ids {
		id = strings.ToLower(strings.TrimSpace(id))
		if id == "" || slices.Contains(out, id) {
			continue
		}
		out = append(out, id)
	}
	return out
}

// knownApps names the apps this cockpit drives, for a sentence.
func knownApps() string {
	var ids []string
	for _, s := range apps.Specs() {
		ids = append(ids, s.ID)
	}
	return strings.Join(ids, ", ")
}
