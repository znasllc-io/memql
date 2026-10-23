package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// refusal.go -- what the worker does when a cluster REFUSES it
// (memql-cockpit#427).
//
// A cluster that cannot be reached and a cluster that answered "no" are
// different situations, and the runner used to treat them the same way:
// warn, back off to fifteen seconds, ask again, forever. For a refusal
// that is the wrong answer twice over. Asking every fifteen seconds asks
// the same question of the same answer -- a revoked token is still
// revoked -- and the warning it prints every time is the kind of line a
// person learns to skip, which is how a machine stays quietly dead.
//
// So a refusal gets a HOLD: one error line that says what happened and
// what fixes it, a retry every minute doubling to every fifteen, a gauge
// on the metrics endpoint, and a record under the home's state dir that
// `memql worker config` prints. The first accepted handshake ends all of
// it.
//
// NOT DISABLED, deliberately. The engine answers Unauthenticated for a
// revoked or expired token AND for a token lookup that merely failed (its
// interceptor maps every resolver error to "invalid worker token"), so
// from here a database blip during a deploy is indistinguishable from a
// revocation. Writing `enabled: false` into the operator's workers.yaml
// on that evidence would turn one bad minute into a machine that never
// comes back. The hold is the answer that is right in both cases: cheap
// for a machine that was really revoked, and self-healing for one that
// was not. For the same reason one refusal is not enough to start it: the
// cluster has to have said no refusalStreak times in a row AND for at
// least refusalGrace, so a resolver blip shorter than that costs nothing
// but a few fast retries.

// refusalStreak is the fewest refusals in a row that can start a hold.
const refusalStreak = 3

// refusalGrace is how long the cluster must have kept refusing before the
// hold starts. At the ordinary backoff (1s doubling to 15s) that is about
// six attempts -- trivial for a token that really was revoked, and long
// enough that a deploy's token-lookup blip never parks the whole fleet
// for a minute.
const refusalGrace = 30 * time.Second

// The hold starts at a minute and doubles to fifteen: a machine that was
// really revoked asks four times an hour, and one refused by a blip is
// back inside a minute of the blip ending.
const (
	defaultRefusalHoldMin = time.Minute
	defaultRefusalHoldMax = 15 * time.Minute
)

// refusalKind says WHAT the cluster refused, because the fix differs.
type refusalKind string

const (
	// refusedCredential: the worker token -- unknown, revoked, expired.
	refusedCredential refusalKind = "credential"
	// refusedRemoved: the token was accepted and the machine's
	// registration was not -- the owner removed it from the fleet.
	refusedRemoved refusalKind = "removed"
)

// refusal is one "no" from the cluster.
type refusal struct {
	kind refusalKind
	// clusterSaid is the engine's own words, passed through. The
	// sentences below say what it MEANS; this says what it SAID, which is
	// what a person searching the cluster's logs will find.
	clusterSaid string
}

// refusalOf classifies a failed handshake: a refusal, or not. Everything
// that is not recognisably the cluster refusing THIS MACHINE is treated
// as the cluster being unreachable, which keeps the ordinary fast
// backoff -- a misread in that direction costs some log lines, and a
// misread in the other costs a machine that waits a quarter of an hour
// for a network that came back in a second.
func refusalOf(err error) (refusal, bool) {
	var refused *RegisterRefusedError
	if errors.As(err, &refused) {
		// A register_failed for any reason but these -- a descriptor this
		// engine will not accept, a registration write that failed -- is
		// not about who this machine is.
		if kind, ok := engineRefusal(refused.Message); ok {
			return refusal{kind: kind, clusterSaid: refused.Message}, true
		}
		return refusal{}, false
	}
	// errors.As rather than status.FromError: the latter answers a
	// wrapped status with the WHOLE error text as its message, and the
	// cluster's own words are the part worth keeping.
	var grpcErr interface{ GRPCStatus() *status.Status }
	if !errors.As(err, &grpcErr) {
		return refusal{}, false
	}
	st := grpcErr.GRPCStatus()
	if st == nil {
		return refusal{}, false
	}
	switch st.Code() {
	case codes.Unauthenticated:
		// The interceptor's answer for every token it would not admit.
		return refusal{kind: refusedCredential, clusterSaid: st.Message()}, true
	case codes.PermissionDenied:
		// The status the engine ends a refused registration with; the
		// RegisterError before it is normally read first. Only its known
		// refusals count -- the same status carries every other
		// register_failed too.
		if kind, ok := engineRefusal(st.Message()); ok {
			return refusal{kind: kind, clusterSaid: st.Message()}, true
		}
	}
	return refusal{}, false
}

// engineRefusal recognises the engine's own refusal sentences
// (component/worker admitRegistration and upsertRegistration), EXACTLY:
// the same register_failed carries a wrapped store error for a write that
// failed, and a substring match could read one of those as a revocation.
func engineRefusal(message string) (refusalKind, bool) {
	switch strings.ToLower(strings.TrimSpace(message)) {
	case "worker is revoked":
		return refusedRemoved, true
	case "worker token is inactive", "worker token expired":
		return refusedCredential, true
	}
	return "", false
}

// refusalHold is the runner's standing with a cluster that refused it.
type refusalHold struct {
	// streak counts refusals in a row -- an unreachable attempt between
	// them starts it over -- and first is when the first of them came.
	streak int
	first  time.Time
	// held is set once streak reaches refusalStreak, and cleared only by
	// an accepted handshake.
	held  bool
	since time.Time
	wait  time.Duration
	last  refusal
}

// afterConnectFailure decides how long to wait after a failed connect,
// and says so -- once, for a refusal.
func (r *Runner) afterConnectFailure(err error, backoff *time.Duration) time.Duration {
	ref, refused := refusalOf(err)
	switch {
	case refused:
		if r.refused.streak == 0 {
			r.refused.first = r.clock()
		}
		r.refused.streak++
		r.refused.last = ref
	case !r.refused.held:
		// A streak is refusals IN A ROW. An attempt that never reached the
		// cluster says nothing about whether it still refuses, so the
		// grace starts over with the next refusal -- otherwise the first
		// refusals after a long outage would count the outage as grace.
		r.refused.streak = 0
	}
	if r.refused.held {
		// Every attempt while held waits the hold, whatever it failed
		// with: a network error in between does not mean the cluster
		// has changed its mind.
		r.refused.wait = nextBackoff(r.refused.wait, r.holdMaxOrDefault())
		r.logger.Debug("the cluster still refuses this machine; asking again later",
			"error", err,
			"retry_in", r.refused.wait.String(),
		)
		return r.refused.wait
	}
	if refused && r.refused.streak >= refusalStreak && r.clock().Sub(r.refused.first) >= refusalGrace {
		r.refused.held = true
		r.refused.since = r.clock()
		r.refused.wait = r.holdMinOrDefault()
		r.metrics.SetRefused(r.home, true)
		rec := refusalRecord{
			Since:       r.refused.since,
			Kind:        ref.kind,
			ClusterSaid: ref.clusterSaid,
			Token:       tokenFingerprint(r.cfg.Token),
		}
		if werr := saveRefusal(r.cfg.StateDir, rec); werr != nil {
			r.logger.Warn("could not record the refusal for `memql worker config`", "error", werr)
		}
		r.logger.Error(refusalLogSentence(ref.kind),
			"cluster_url", r.cfg.ClusterURL,
			"cluster_said", ref.clusterSaid,
			"retry_in", r.refused.wait.String(),
			"fix", refusalFix(ref.kind, r.home),
		)
		return r.refused.wait
	}
	r.logger.Warn("worker connect failed; will retry",
		"error", err,
		"backoff_seconds", backoff.Seconds(),
	)
	wait := *backoff
	*backoff = nextBackoff(*backoff, r.maxBackoff())
	return wait
}

// afterConnect ends any hold: the cluster accepted this machine.
func (r *Runner) afterConnect() {
	if r.refused.held {
		r.logger.Info("the cluster accepted this machine again",
			"refused_since", r.refused.since.Format(time.RFC3339))
		r.metrics.SetRefused(r.home, false)
	}
	r.refused = refusalHold{}
	// Unconditionally, because the record may be a PREVIOUS process's:
	// the machine was re-paired and the worker restarted, and a record
	// left behind would have `worker config` report a refusal that is
	// over.
	if err := clearRefusal(r.cfg.StateDir); err != nil {
		r.logger.Warn("could not clear the recorded refusal", "error", err)
	}
}

func (r *Runner) holdMinOrDefault() time.Duration {
	if r.holdMin > 0 {
		return r.holdMin
	}
	return defaultRefusalHoldMin
}

func (r *Runner) holdMaxOrDefault() time.Duration {
	if r.holdMax > 0 {
		return r.holdMax
	}
	return defaultRefusalHoldMax
}

// refusalLogSentence is the one error line a refusal earns.
func refusalLogSentence(kind refusalKind) string {
	if kind == refusedRemoved {
		return "the cluster says this machine was removed from its fleet; this worker stops asking every few seconds and asks again every few minutes"
	}
	return "the cluster refused this machine's worker token; this worker stops asking every few seconds and asks again every few minutes"
}

// refusalFix is what a person does about it, in the words they would
// type.
func refusalFix(kind refusalKind, home string) string {
	if kind == refusedRemoved {
		return "to serve that cluster again, pair this machine with a new code from the portal (memql worker pair <code>); to stop asking, remove it here (memql worker unpair --cluster " + home + ")"
	}
	return "pair this machine again with a new code from the portal: memql worker pair <code>"
}

// refusalRecord is what a hold leaves on disk for `memql worker config`,
// which runs in another process and cannot ask the worker. Nothing in it
// is secret: the token appears only as a fingerprint, which is enough to
// tell whether the refused token is still the configured one.
type refusalRecord struct {
	Since       time.Time   `json:"since"`
	Kind        refusalKind `json:"kind"`
	ClusterSaid string      `json:"clusterSaid"`
	Token       string      `json:"token"`
}

const refusalFile = "refused.json"

// tokenFingerprint names a token without carrying it: the first twelve
// hex digits of its SHA-256.
func tokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])[:12]
}

// saveRefusal records a hold. A record already there for the same token
// and kind keeps its original time: a worker that restarted into the same
// refusal has been refused since the first time, not since the restart.
func saveRefusal(stateDir string, rec refusalRecord) error {
	if strings.TrimSpace(stateDir) == "" {
		return nil
	}
	if prev, ok := loadRefusal(stateDir); ok && prev.Token == rec.Token && prev.Kind == rec.Kind && !prev.Since.IsZero() {
		rec.Since = prev.Since
	}
	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("refusal: mkdir: %w", err)
	}
	path := filepath.Join(stateDir, refusalFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("refusal: write: %w", err)
	}
	return os.Rename(tmp, path)
}

// loadRefusal reads the record, if there is one.
func loadRefusal(stateDir string) (refusalRecord, bool) {
	if strings.TrimSpace(stateDir) == "" {
		return refusalRecord{}, false
	}
	body, err := os.ReadFile(filepath.Join(stateDir, refusalFile))
	if err != nil {
		return refusalRecord{}, false
	}
	var rec refusalRecord
	if json.Unmarshal(body, &rec) != nil {
		return refusalRecord{}, false
	}
	return rec, true
}

// clearRefusal removes the record; absent is not an error.
func clearRefusal(stateDir string) error {
	if strings.TrimSpace(stateDir) == "" {
		return nil
	}
	if err := os.Remove(filepath.Join(stateDir, refusalFile)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// describeRefusal is the block `memql worker config` prints under a home
// the cluster is refusing, or nil when it is not. A record for a token the
// home no longer holds says nothing: that refusal was of a token somebody
// already replaced, and the worker settles it on its next connect.
func describeRefusal(rec refusalRecord, ok bool, home Home) []string {
	if !ok || rec.Token != tokenFingerprint(home.Token) {
		return nil
	}
	what := "The cluster did not accept this machine's token"
	fix := "Pair this machine again with a new code from the portal:"
	cmd := "memql worker pair <code>"
	if rec.Kind == refusedRemoved {
		what = "The cluster says this machine was removed from its fleet"
		fix = "Pair it again to serve that cluster, or remove it here to stop asking:"
		cmd = "memql worker pair <code>   |   memql worker unpair --cluster " + home.ID
	}
	lines := []string{
		fmt.Sprintf("REFUSED since %s", rec.Since.Local().Format("2006-01-02 15:04 MST")),
		fmt.Sprintf("%s (it said %q).", what, rec.ClusterSaid),
		"The worker stopped asking every few seconds; it asks again every few minutes.",
		fix,
		"  " + cmd,
	}
	return lines
}
