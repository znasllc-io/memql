package worker

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/znasllc-io/memql/cockpit/internal/auth"
	"github.com/znasllc-io/memql/cockpit/internal/config"
)

// How the watched-folder sweeper gets an origin and a credential
// (memql#4841).
//
// ===========================================================================
// THE CREDENTIAL IS THE SIGNED-IN USER'S, AND IT HAS TO BE
// ===========================================================================
// The Library's HTTP routes gate on the ACTOR RESOLVING TO A USER: the
// upload path stamps `ownerUserId` from `actor.userId` and keys the blob path
// on it, so a credential with no user behind it has nowhere to put the bytes.
// The worker token this process authenticates its STREAM with is exactly that
// kind of credential -- `mql_wkr_` names a machine, is admitted on
// WorkerService and nowhere else, and no HTTP middleware anywhere reads it.
//
// THE RULE IS "RESOLVES TO A USER", NOT "IS class=user", and the distinction
// became load-bearing in memql#4863. An app session's back-channel is now
// minted as `class="app_session"` whose `sub` IS the owning user's id, and
// those routes admit it for precisely that reason -- which is what makes
// internal/worker/appsession's Library.Pull and .Push work at all. So do not
// read this paragraph as "only class=user reaches /artifacts" and conclude the
// app-session path cannot; the classes that stay off the surface are the ones
// that name a MACHINE (`service_account`, `voice_agent`, `worker_token`,
// `node`), because those have no user for the row to belong to.
//
// The sweeper still runs as the PERSON, and that is a separate decision rather
// than a consequence of the above. A folder backup is that person's own files
// moving, and it must not depend on a delegated run's short-lived credential
// happening to exist: no app session is open when a machine sweeps at three in
// the morning. Running as the person also makes row authorization here exactly
// the browser's -- the same reads, the same writes, the same refusals -- with
// no second story to keep in step.
//
// A PAT DOES NOT WORK EITHER, and this is worth knowing before debugging a
// 401: PATs verify only on the identity node (the per-node verifier is built
// with a nil PATVerifier), so a cluster configured with a PAT alone can dial
// gRPC and cannot reach `/artifacts`. Both clients name that case in their 401
// message rather than leaving somebody to find it.

// backupBaseURL derives the cluster's HTTP origin from the worker's own
// cluster_url.
//
// THERE IS NO SECOND HOST TO CONFIGURE. The Library routes and the query
// gateway are served by the same front door the worker already dials, and
// inventing a knob for them would be one an operator has to keep in sync with
// something they cannot see. Same derivation appsession.LibraryBaseURL makes,
// restated here rather than imported: coupling the app-session package and the
// backup package for one string would tie two subsystems that have nothing
// else to say to each other.
func backupBaseURL(clusterURL string) string {
	raw := strings.TrimSpace(clusterURL)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		return "https://" + strings.TrimSuffix(raw, ":443")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := "https"
	switch strings.ToLower(u.Scheme) {
	case "grpc", "http":
		scheme = "http"
	}
	return scheme + "://" + strings.TrimSuffix(u.Host, ":443")
}

// backupBearer resolves the signed-in user's access token for the cluster this
// worker is pointed at, or returns nil when there is no way to get one.
//
// A NIL RESULT DISABLES THE FEATURE, quietly. A machine that is paired but not
// signed in is the ordinary state of a freshly paired worker, and refusing to
// start the worker over it would make backups a prerequisite for computer use.
//
// NON-INTERACTIVE, always. This runs under a LaunchAgent or a systemd unit,
// where the browser step is not slow -- it is a process blocked forever on a
// window that will never open. The refresh path is unaffected and still rolls
// an expired token forward silently; only the last resort differs, and it says
// to run `memql login` here.
func backupBearer(clusterURL string, logger *slog.Logger) func(context.Context) (string, error) {
	cluster, ok := clusterFor(clusterURL)
	if !ok {
		// WARN, not Debug. This disables the whole feature, and the most
		// likely cause is a spelling difference nobody can see: a cluster_url
		// carrying a non-443 port matches no registry entry, so the sweeper
		// silently did nothing while every other part of the worker worked.
		// A line an operator will actually see is the difference between a
		// five-minute fix and an unexplainable backup.
		if logger != nil {
			logger.Warn("backup: no registered cluster matches this worker's cluster_url, so no folders are backed up",
				"cluster_url", clusterURL,
				"hint", "run `memql cluster list` and make sure one entry's endpoint names the same host")
		}
		return nil
	}
	return func(ctx context.Context) (string, error) {
		token, err := auth.EnsureValidTokenNonInteractive(ctx, cluster, logger)
		if err != nil {
			return "", fmt.Errorf("backup: no usable sign-in for cluster %q: %w", cluster.Name, err)
		}
		return token, nil
	}
}

// clusterFor finds the registered cluster this worker is pointed at.
//
// MATCHED ON HOST, not on the string. The worker's cluster_url and the
// registry's endpoint describe the same front door and are routinely written
// differently -- "api.example.com:443" against "https://api.example.com" --
// and a string comparison would silently find nothing and disable the feature
// with no error anybody could act on.
func clusterFor(clusterURL string) (config.ClusterConfig, bool) {
	want := hostOf(clusterURL)
	if want == "" {
		return config.ClusterConfig{}, false
	}
	file, err := config.LoadClusters()
	if err != nil || file == nil {
		return config.ClusterConfig{}, false
	}
	for _, entry := range file.Clusters {
		resolved := config.WithLocalDefault(entry)
		if hostOf(resolved.Endpoint) == want {
			return resolved, true
		}
	}
	return config.ClusterConfig{}, false
}

func hostOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(u.Host, ":443"))
}
