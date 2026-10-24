package access

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	sdkclient "github.com/znasllc-io/memql/sdk/go/client"

	"github.com/znasllc-io/memql/cockpit/internal/auth"
	"github.com/znasllc-io/memql/cockpit/internal/config"
	"github.com/znasllc-io/memql/cockpit/internal/crash"
)

// callTimeout bounds the round trip once the human part is over.
//
// IT DELIBERATELY DOES NOT COVER THE SIGN-IN. EnsureValidToken is the
// interactive one here, so an expired token opens a browser -- and that is
// human-paced: find the window, pick an account, approve. Twenty seconds is a
// good ceiling for "the cluster did not answer" and a terrible one for "the
// human has not finished logging in", where it would kill the sign-in partway
// through with a deadline error naming nothing the person did wrong.
//
// It is not a whole-command ceiling either, and the comment should not claim
// to be one: the SDK opens its stream on a context that outlives this one (so
// the dial is bounded by gRPC's own connect timeout, not by this), and
// Dispatcher.SendAndWait writes before it selects on Done. What this reliably
// covers is the wait for an ANSWER, which is the stage that actually hangs.
// A var, not a const, so a test can shrink it: the ceiling can only be proven
// by reaching it, and a suite is not the place to wait twenty seconds.
var callTimeout = 20 * time.Second

// ensureToken is auth.EnsureValidToken, indirected so the deadline placement
// above is testable without a cluster or a browser.
var ensureToken = auth.EnsureValidToken

// HandleCommand runs `memql access`. installCredStore is the caller's
// credential-store resolver, run only once the arguments are known to be good
// -- resolving the OS keyring costs D-Bus round trips (and can exit non-zero
// when MEMQL_COCKPIT_CRED_STORE names one that is unavailable), which is not a
// price `--help` or a typo should pay. Every sibling command validates argv
// before installing the store; this keeps that order.
func HandleCommand(args []string, installCredStore func()) int {
	// crash.Catch for the reason internal/lint states: a panic downstream would
	// otherwise kill the process with an unsanitized goroutine trace on stderr.
	// It matters more here than anywhere else in the binary -- this is the one
	// command holding a live user bearer, and crash.SanitizeForCrashLog carries
	// patterns for exactly the mql_pat_ / JWT / Authorization values it passes
	// into the SDK.
	exitCode := 0
	if rep := crash.Catch("access:HandleCommand", func() {
		exitCode = run(args, os.Stdout, os.Stderr, installCredStore)
	}); rep != nil {
		fmt.Fprint(os.Stderr, crash.UserMessage(rep))
		return 2
	}
	return exitCode
}

// run is HandleCommand with its streams injected, so the exit codes and the
// sentences are testable without a subprocess.
func run(args []string, out, errOut io.Writer, installCredStore func()) int {
	// A TINY ARGV PARSER, not flag.FlagSet, and for a specific reason: Go's
	// flag package stops parsing at the first positional, so `memql access
	// prod --json` silently produced a TEXT report with a zero exit code --
	// straight into a caller's `| jq`, which is the exact invocation this
	// command's own usage string documents. internal/lint made the same choice
	// for the same reason.
	var (
		asJSON bool
		name   string
	)
	for _, a := range args {
		switch {
		case a == "--json", a == "-j":
			asJSON = true
		case a == "--help", a == "-h":
			// Help that was asked for is a SUCCESS, and it goes to STDOUT --
			// `memql access --help > usage.txt` must not write an empty file
			// when every sibling verb fills one.
			printUsage(out)
			return 0
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(errOut, "ERROR: unknown flag %q\n", a)
			printUsage(errOut)
			return 2
		default:
			if name != "" {
				fmt.Fprintf(errOut, "ERROR: too many positional arguments (got %q after %q)\n", a, name)
				printUsage(errOut)
				return 2
			}
			name = a
		}
	}

	if installCredStore != nil {
		installCredStore()
	}

	clusters, err := config.LoadClusters()
	if err != nil {
		fmt.Fprintf(errOut, "ERROR: %v\n", err)
		return 1
	}
	cluster, err := resolveCluster(clusters, name)
	if err != nil {
		fmt.Fprintf(errOut, "ERROR: %v\n", err)
		return 1
	}

	// Background, not a deadlined context: Fetch bounds its own round trip and
	// leaves the sign-in unbounded on purpose.
	summary, err := Fetch(context.Background(), cluster)
	if err != nil {
		fmt.Fprintf(errOut, "ERROR: %v\n", err)
		return 1
	}

	if asJSON {
		// The write error is DELIBERATELY not an exit code, so the two
		// renderers agree. The only realistic failure here is a closed pipe --
		// `memql access --json | head -1` -- which is something head does
		// routinely and not a failure of this command; Render has no way to
		// report it at all, and one of them exiting 1 where the other exits 0
		// for the same pipeline is worse than neither doing.
		_ = RenderJSON(out, summary, cluster.Name)
		return 0
	}
	Render(out, summary, cluster.Name)
	return 0
}

// Fetch signs in if needed, asks the cluster for the caller's access record,
// and decodes it.
//
// IT ASKS THE CLUSTER RATHER THAN DECODING THE BEARER. The cockpit does not
// parse its own tokens -- the standing client rule the wire's `session_id` and
// `display_name` fields exist to serve -- because a claim is what was true when
// the token was minted, and a role changed since is exactly what somebody
// running this command is trying to find out.
func Fetch(ctx context.Context, cluster config.ClusterConfig) (Summary, error) {
	cluster = config.WithLocalDefault(cluster)
	if cluster.Endpoint == "" {
		return Summary{}, fmt.Errorf("cluster %q has no endpoint. Re-run `memql cluster add <domain>` to register it", cluster.Name)
	}

	// INTERACTIVE on purpose, unlike the worker's paths: a person typed this,
	// so a browser sign-in is the right answer to an expired token rather than
	// a window nobody will open.
	token, err := ensureToken(ctx, cluster)
	if err != nil {
		// Named, because the cluster is often IMPLICIT here -- resolved from
		// selected_cluster rather than typed -- and "login: context deadline
		// exceeded" on its own does not say which one is unreachable.
		return Summary{}, fmt.Errorf("sign in to %q: %w", cluster.Name, err)
	}

	// The deadline starts HERE, once the human part is done.
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	// ===================================================================
	// THE CEILING IS ENFORCED HERE, NOT BY THE CONTEXT ALONE
	// ===================================================================
	// Passing a deadlined context to the SDK is NOT enough, and believing it
	// was is how this hung. sdkclient.Connect opens the stream on
	// context.Background() by design -- "the stream context must outlive the
	// connect timeout" -- so `client.Stream(...)` blocks on a channel in
	// CONNECTING until gRPC's own connect timeout, with nothing of ours
	// attached to it. Against a peer that completes a TCP handshake and then
	// says nothing -- a firewall dropping packets, a wedged load balancer,
	// exactly the "cluster is not answering" case this ceiling exists for --
	// the ctx deadline passes unnoticed and the command waits forever at a
	// prompt somebody is sitting in front of.
	//
	// So the whole round trip runs beside a select. The goroutine still owns
	// closing the connection, so a late arrival cleans up after itself rather
	// than leaking a socket; it outlives this call by at most gRPC's own
	// timeout, on a process that is about to exit.
	type answer struct {
		summary Summary
		err     error
	}
	done := make(chan answer, 1)
	go func() {
		conn, err := sdkclient.Connect(ctx, sdkclient.ConnectConfig{
			Endpoint: cluster.Endpoint,
			Token:    token,
		})
		if err != nil {
			done <- answer{err: fmt.Errorf("connect to %s: %w", cluster.Endpoint, err)}
			return
		}
		defer conn.Close()

		resp, err := conn.Dispatcher().SendAndWait(ctx, &memqlv1.MemqlClientMessage{
			Payload: &memqlv1.MemqlClientMessage_MyAccess{MyAccess: &memqlv1.MyAccessMsg{}},
		})
		if err != nil {
			done <- answer{err: fmt.Errorf("ask %s who I am: %w", cluster.Name, err)}
			return
		}
		s, err := summaryFromResponse(cluster.Name, resp)
		done <- answer{summary: s, err: err}
	}()

	select {
	case a := <-done:
		return a.summary, a.err
	case <-ctx.Done():
		return Summary{}, fmt.Errorf("%s did not answer within %s (%s is reachable or not, but it is not talking)",
			cluster.Name, callTimeout, cluster.Endpoint)
	}
}

// summaryFromResponse interprets one server message. Split out from Fetch
// because everything it decides is reachable without a cluster, and the
// refusal path below is the one this command most needs to get right.
func summaryFromResponse(cluster string, resp *memqlv1.MemqlServerMessage) (Summary, error) {
	if resp == nil {
		return Summary{}, fmt.Errorf("%s answered with nothing at all", cluster)
	}
	// A REFUSAL IS NOT A TRANSPORT ERROR. It arrives as a QueryError inside a
	// perfectly good response, so a caller that only checked `err` would render
	// an empty record as "you hold nothing" -- which is the same shape as the
	// answer it would give a person who really does.
	//
	// The CODE travels with the message. The engine refuses this call with
	// Unauthenticated, and a person told only "access context not available"
	// has no idea their session was revoked -- which is the single most likely
	// reason to be running this command in the first place.
	if qErr := resp.GetQueryError(); qErr != nil {
		return Summary{}, refusalError(cluster, qErr)
	}
	result := resp.GetMyAccessResult()
	if result == nil {
		return Summary{}, fmt.Errorf("%s answered without an access record", cluster)
	}
	return Decode(result), nil
}

func refusalError(cluster string, qErr *memqlv1.QueryErrorMsg) error {
	e := qErr.GetError()
	msg := strings.TrimSpace(e.GetMessage())
	if msg == "" {
		msg = "no reason given"
	}
	if code := strings.TrimSpace(e.GetCode()); code != "" {
		return fmt.Errorf("%s refused: %s (%s)", cluster, msg, code)
	}
	return fmt.Errorf("%s refused: %s", cluster, msg)
}

// resolveCluster picks the cluster to report on.
//
// It REFUSES to guess between several. Reporting somebody's access on a
// cluster they did not name is worse than asking, because the answer looks
// exactly like the one they wanted -- and the whole point of this command is
// to be believed about which permissions are real.
func resolveCluster(file *config.ClustersFile, name string) (config.ClusterConfig, error) {
	registered := clusterNames(file)

	if name != "" {
		c, ok := lookupCluster(file, name)
		if !ok {
			if len(registered) == 0 {
				return config.ClusterConfig{}, fmt.Errorf("cluster %q is not registered, and neither is any other. Register one with `memql cluster add <domain>`", name)
			}
			return config.ClusterConfig{}, fmt.Errorf("cluster %q is not registered. Registered clusters: %s", name, strings.Join(registered, ", "))
		}
		return c, nil
	}

	if sel := strings.TrimSpace(file.SelectedCluster); sel != "" {
		if c, ok := lookupCluster(file, sel); ok {
			return c, nil
		}
		// A STALE SELECTION IS ONLY A PROBLEM WHEN THERE IS A CHOICE. It is
		// reachable through ordinary use -- `memql cluster remove` does not
		// clear selected_cluster -- so remove-then-add leaves one cluster and
		// a dangling name. Refusing there would dead-end the command over an
		// ambiguity that does not exist, and contradict the usage text's
		// "the selected cluster ... or the only registered one".
		if len(registered) > 1 {
			return config.ClusterConfig{}, fmt.Errorf("the selected cluster %q is no longer registered. Name one explicitly: %s", sel, strings.Join(registered, ", "))
		}
	}

	if len(registered) == 1 {
		c, _ := lookupCluster(file, registered[0])
		return c, nil
	}
	if len(registered) == 0 {
		return config.ClusterConfig{}, fmt.Errorf("no clusters are registered. Register one with `memql cluster add <domain>`, or use the built-in `local`")
	}

	return config.ClusterConfig{}, fmt.Errorf("several clusters are registered and none is selected. Name one: %s", strings.Join(registered, ", "))
}

// lookupCluster resolves a name, including the reserved `local` slot that
// clusters.yaml need not contain.
//
// `local` IS ALWAYS ADDRESSABLE, because every other command treats it that
// way: `cluster list` synthesizes the row, `cluster add` refuses the name as
// reserved, and `cluster remove` refuses to delete it as a permanent default.
// Without this, `memql access local` on a fresh dev box answered "not
// registered. Register one with `memql cluster add`" -- advice `cluster add`
// then refuses.
//
// It is deliberately NOT added to the auto-pick candidate set: synthesizing a
// second cluster would turn the ordinary one-real-cluster case into an
// ambiguity that does not exist for the person.
func lookupCluster(file *config.ClustersFile, name string) (config.ClusterConfig, bool) {
	if c, ok := file.Get(name); ok {
		return c, true
	}
	if name == localClusterName {
		return config.WithLocalDefault(config.ClusterConfig{Name: localClusterName}), true
	}
	return config.ClusterConfig{}, false
}

// localClusterName is the reserved slot cmd/memql treats as permanently
// present.
const localClusterName = "local"

func clusterNames(file *config.ClustersFile) []string {
	if file == nil {
		return nil
	}
	names := make([]string, 0, len(file.Clusters))
	for _, c := range file.Clusters {
		names = append(names, c.Name)
	}
	sort.Strings(names)
	return names
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: memql access [<cluster>] [--json]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Print what this cluster says about the signed-in credential on this")
	fmt.Fprintln(w, "machine: the user it resolves to, the role held (slug, name and rank),")
	fmt.Fprintln(w, "group memberships, and account scope.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "With no <cluster>, the selected cluster in ~/.memql/clusters.yaml is used,")
	fmt.Fprintln(w, "or the only registered one. Several with none selected is an error rather")
	fmt.Fprintln(w, "than a guess. The built-in `local` cluster is always addressable by name.")
}
