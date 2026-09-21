package githubconnect

import (
	"context"
	"strings"
	"sync"
	"time"
)

// resolve.go -- WHERE the cluster's GitHub App comes from (design record
// 2026-09-20-github-app-setup, D1 and D2).
//
// The six values used to have exactly one home: the node's environment, read
// once at boot. A cluster owner can now register the app from the product,
// and that registration cannot be written back into an environment -- a
// Kubernetes Secret is the deployment's, a laptop's `.env` is nobody's, and
// either way every pod would have to restart to see it. So the values have a
// second home: six rows, under the SAME six names, in the two stores every
// node already reads instance-wide configuration from
// (v1:platform:globalVariable and v1:platform:globalSecret). Same names on
// purpose -- it is what lets the `githubApp` readiness module, which walks
// environment and then rows BY NAME, report the truth with no new code.
//
// ===========================================================================
// ONE TIER OR THE OTHER, NEVER A MIX
// ===========================================================================
// An app is six values that belong together: a client secret from one
// registration and a private key from another is not "most of an app", it is a
// Connect button that mints grants nothing can fetch under. So the six resolve
// WHOLE from ONE tier, which is the lane rule integrations/email's
// ConfigResolver states for the same reason.
//
// THE ENVIRONMENT WINS, AND ANY OF IT IS ALL OF IT. An operator who set even
// one MEMQL_GITHUB_APP_* value has made a statement about this cluster's app,
// and rows written later from a browser must not quietly take over which app
// a running deployment talks to -- the inbound seam's "ENV WINS, deliberately"
// (component/inbound/handler.go). A PARTIAL environment therefore stays a
// partial environment: the identity node refuses boot on it exactly as before
// (Validate), rather than being papered over by whatever the rows hold.

// Source says which tier the cluster's GitHub App resolved from.
type Source string

const (
	// SourceNone: no app. Connect is absent and the Source stop offers the
	// pasted-token path.
	SourceNone Source = ""
	// SourceEnvironment: the MEMQL_GITHUB_APP_* values on the node. The
	// deployment owns the app; the product shows it and changes nothing.
	SourceEnvironment Source = "environment"
	// SourceCluster: six rows a cluster owner's registration wrote. The
	// product owns the app and may replace or remove it.
	SourceCluster Source = "cluster"
)

// RowReader reads one named, instance-wide value from the cluster's stores.
//
// TWO FUNCTIONS, because they are two stores with two readers. A secret is
// sealed under MEMQL_MASTER_KEY and only the Secret reader unseals; a plaintext
// row written under a secret's name would satisfy a falling-through lookup
// while the decrypting reader still found nothing. readiness_eval.go's
// resolveSlot refuses that fallthrough for the same reason, and this agrees
// with it slot for slot -- it has to, or the Set up card and the Connect
// button would disagree about whether this cluster has an app.
//
// Functions rather than an engine, so this package stays what its doc says it
// is: no state, no row, no engine. Production hands it
// memql.PluginContext.ResolveSystemVariable / ResolveSystemSecret; a test
// hands it a map.
type RowReader struct {
	Variable func(ctx context.Context, name string) (string, error)
	Secret   func(ctx context.Context, name string) (string, error)
}

// secretEnvNames are the three of the six that are credentials. The split is
// the registry's (`secret: true` in scripts/secrets/manifest.yaml), restated
// here because this package cannot import the registry and a wrong guess reads
// the wrong store. TestTheSecretSplitMatchesTheRegistry pins the two together.
var secretEnvNames = map[string]bool{
	EnvClientSecret:  true,
	EnvPrivateKeyB64: true,
	EnvWebhookSecret: true,
}

// IsSecretName reports whether one of the six names is a credential, and so
// lives sealed in v1:platform:globalSecret rather than in globalVariable.
func IsSecretName(envName string) bool { return secretEnvNames[envName] }

// EnvNames lists the six, in the order the refusals name them.
func EnvNames() []string {
	var out []string
	for _, f := range (Config{}).fields() {
		out = append(out, f.envName)
	}
	return out
}

// Resolve answers the cluster's GitHub App and the tier it came from.
//
// A row that cannot be read is an ABSENT row. "Not found" is the ordinary
// answer on every cluster that never registered an app, and the stores do not
// distinguish it from a transient failure in a way this package could act on:
// either way there is no app to offer right now, the caller answers
// github_app_not_configured, and the next call asks again.
func Resolve(ctx context.Context, rows RowReader) (Config, Source) {
	if env := LoadFromEnv(); len(env.Present()) > 0 {
		return env, SourceEnvironment
	}
	read := func(name string) string {
		fn := rows.Variable
		if IsSecretName(name) {
			fn = rows.Secret
		}
		if fn == nil {
			return ""
		}
		v, err := fn(ctx, name)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(v)
	}
	stored := Config{
		AppID:         read(EnvAppID),
		AppSlug:       read(EnvAppSlug),
		ClientID:      read(EnvClientID),
		ClientSecret:  read(EnvClientSecret),
		PrivateKeyB64: read(EnvPrivateKeyB64),
		WebhookSecret: read(EnvWebhookSecret),
	}
	if !stored.Configured() {
		// ALL SIX OR NONE, here too. A removal clears every row and a
		// registration writes every row, so a partial set is a write that was
		// interrupted -- and half an app is no app.
		return Config{}, SourceNone
	}
	return stored, SourceCluster
}

// resolverTTL is how long one answer is reused.
//
// TEN SECONDS, the figure component/identity/http's granted-origin cache
// settled on and for its reason: the identity node is filtered out of the mesh
// event bridge, so no invalidation broadcast can reach the one node type that
// serves the callback. What makes a plain TTL correct is that every node shares
// one database -- a registration is ROWS, and whichever node wrote them, every
// other node reads them on its next miss. Ten seconds is far shorter than the
// trip to GitHub and back that follows a registration, and long enough that a
// poll sweeping thirty packages does one read of six rows, not thirty.
const resolverTTL = 10 * time.Second

// Resolver is Resolve with a short memory. Safe for concurrent use.
type Resolver struct {
	Rows RowReader
	// Now is the clock, for a test. Nil means time.Now.
	Now func() time.Time

	mu     sync.Mutex
	valid  bool
	at     time.Time
	cfg    Config
	source Source
}

// Current answers the cluster's GitHub App, from memory when that is fresh.
func (r *Resolver) Current(ctx context.Context) (Config, Source) {
	if r == nil {
		return Resolve(ctx, RowReader{})
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	r.mu.Lock()
	if r.valid && now().Sub(r.at) < resolverTTL {
		cfg, source := r.cfg, r.source
		r.mu.Unlock()
		return cfg, source
	}
	r.mu.Unlock()

	// Read OUTSIDE the lock: six row reads are six round trips, and a second
	// caller waiting on them gains nothing a second read would not give it.
	cfg, source := Resolve(ctx, r.Rows)

	r.mu.Lock()
	r.cfg, r.source, r.at, r.valid = cfg, source, now(), true
	r.mu.Unlock()
	return cfg, source
}

// Invalidate forgets the remembered answer, so the next call reads again. The
// node that just registered or removed an app calls it; every other node
// catches up inside resolverTTL.
func (r *Resolver) Invalidate() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.valid = false
	r.mu.Unlock()
}
