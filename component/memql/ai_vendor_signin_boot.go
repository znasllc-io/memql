package memql

import (
	"context"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/metrics"
)

// The sign-in's life around a running engine (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions A2 and A5).
//
// THE ENGINE IS THE ONLY PROCESS THAT SIGNS IN. A vendor that accepts an
// identity token once cannot be signed in to from two processes in one pod:
// whichever goes second is refused. `memql provider-auth check` used to be
// that second process. It now reports what the engine recorded, so the engine
// has to have something to report before anybody asks:
//
//   - AT START, when a vendor's cluster account is set up, each node signs in
//     once and lists that vendor's models with the new bearer. Listing models
//     sends no prompt and costs no tokens; it is the proof that the bearer is
//     accepted for API calls and not only minted.
//   - AFTER A PROVIDER RELOAD (an operator's Apply, here or on a peer), the
//     same again. With unchanged ids this is no new sign-in: the bearer in
//     hand is reused and only the model list is asked for.
//
// A PERSON MAY ASK FOR ONE MORE TRY. A refused sign-in is never retried on a
// timer (ai_vendor_signin.go). Verify and Apply are a person asking, usually
// right after fixing the vendor's side, so each lets a refused identity token
// be presented once more: Verify on the node that answers it, Apply on every
// node, through the providers.reload broadcast.
//
// WHAT THE ENGINE TRIES AGAIN BY ITSELF: THE NEXT IDENTITY TOKEN. Two outcomes
// leave a node without the vendor only until the pod's identity token is
// replaced: the engine is WAITING (the token on disk was already presented),
// or the vendor refused a token that MAY ONLY HAVE BEEN USED ALREADY (a
// container that restarted inside its pod; equally a new pod's first token,
// which cannot be told apart). Neither can be settled with the token on disk
// and both are settled by the next one. So the engine presents that token
// when it arrives, with nobody asking: a node with no traffic would otherwise
// stay "not verified" for as long as it stayed idle, and a misconfigured rule
// on a new pod would never be reported as refused.
//
//   - It waits until the token on disk says its replacement is due (kubelet
//     replaces a projected token at 80 percent of its life), and a little
//     more.
//   - kubelet keeps its own schedule and can be a minute or two late, so the
//     engine looks at the file before it does anything: with the old token
//     still there it presents nothing and looks again shortly, a bounded
//     number of times (vendorSignInFollowUpTries).
//   - What it presents is a NEW token. This is never a retry of a refused one,
//     and never a second presentation of an accepted one.
//   - A reload starts a pass of its own and supersedes the wait an earlier pass
//     left behind (vendorSignInGeneration), so two waits never both sign in.
//
// EVERY VENDOR GETS A RECORD, usable or not. Only the engine can say what a
// vendor's ids resolve to: they may be in the environment or in the cluster's
// storage, and the check command sees only the first. So the engine records a
// vendor nobody set up (notConfigured), a vendor whose ids it refuses itself
// (misconfigured: a partial set, an identity token it cannot use), and, on a
// node type that calls no models, that it signs in to nothing (notUsed). The
// check can then tell each of those from "the engine has not said yet".
//
// Regression tests: ai_vendor_signin_boot_test.go, including the cross-node
// hop (TestAReloadBroadcastAllowsOneMoreTryOnThePeer). Never run against a
// vendor.

// vendorSignInAtStartTimeout bounds one vendor's sign-in and model list.
const vendorSignInAtStartTimeout = 45 * time.Second

// vendorSignInFollowUpGrace is how long after the next identity token is due
// the engine waits before presenting it: kubelet replaces the file on its own
// schedule, at or a little after 80 percent of the token's life.
const vendorSignInFollowUpGrace = 30 * time.Second

// vendorSignInFollowUpLimit bounds the wait for the next token. A token that
// says it is due later than this is not waited for.
const vendorSignInFollowUpLimit = 70 * time.Minute

// vendorSignInFollowUpTries bounds how many times the engine looks for the
// next token: once when it is due, then again every vendorSignInFollowUpGrace
// while kubelet has not replaced the file. About five minutes in all.
const vendorSignInFollowUpTries = 10

// vendorSignInFollowUpWait is how the engine waits for the next identity
// token. A variable so a test can make the wait instant; it returns false when
// ctx ended first.
var vendorSignInFollowUpWait = func(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// allowVendorRetry lets every sign-in this process holds present a REFUSED
// identity token once more. It never allows an accepted token to be presented
// again.
func allowVendorRetry() {
	vendorSignInsMu.Lock()
	held := make([]*vendorSignIn, 0, len(vendorSignIns))
	for _, signIn := range vendorSignIns {
		held = append(held, signIn)
	}
	vendorSignInsMu.Unlock()
	for _, signIn := range held {
		signIn.AllowRetry()
	}
}

// clusterVendorAccount returns the sign-in for the cluster's account at a
// vendor, with one registered provider entry that uses it and can list
// models. ok is false when the vendor is not set up on this node.
//
// It reads the REGISTRY, not the process's sign-ins: after a reload that
// removed a vendor's ids the old sign-in is still held by the process, and
// signing in with it would be signing in to an account nobody configured.
func (e *MemQLEngine) clusterVendorAccount(vendor string) (*vendorSignIn, *ProviderConfigEntry, bool) {
	if e == nil || e.providers == nil {
		return nil, nil, false
	}
	for _, name := range e.providers.Names() {
		entry, ok := e.providers.Entry(name)
		if !ok || entry == nil || !entry.Available || entry.Client == nil {
			continue
		}
		switch vendor {
		case metrics.FederationVendorAnthropic:
			if !anthropicProviderTypes[strings.ToLower(entry.Config.Type)] {
				continue
			}
			if _, err := anthropicClientOf(entry.Client); err != nil {
				continue
			}
			signIn, err := anthropicSignInFor(entry.Config)
			if err != nil || signIn == nil {
				continue
			}
			return signIn, entry, true
		case metrics.FederationVendorOpenAI:
			if !isOpenAIProviderType(entry.Config.Type) {
				continue
			}
			if _, ok := openAIClientOf(entry.Client); !ok {
				continue
			}
			fed := openaiFederationFrom(entry.Config.Auth)
			if len(fed.present()) == 0 || len(fed.missing()) != 0 {
				continue
			}
			return processVendorSignIn(openaiSignInConfig(fed, nil)), entry, true
		}
	}
	return nil, nil, false
}

// DisableVendorSignInAtStart makes this node sign in to a vendor only when a
// model call needs it, never ahead of one.
//
// FOR A NODE TYPE THAT CALLS NO MODELS, which today is the identity node. It
// runs under a service account of its own, and a vendor's federation rule
// admits only the engine's (the runbooks map exactly those subjects). Signing
// in from it at start would be refused on every boot: a line in the vendor's
// console, a tick of the denial counter operators are told to alert on, and a
// "refused" in the check command, all for a bearer the node had no use for.
// The opt-out is the node type's to declare (app/build_identity.go), because
// the engine does not know which node it is running in.
func (e *MemQLEngine) DisableVendorSignInAtStart() {
	if e != nil {
		e.vendorSignInAtStartOff.Store(true)
	}
}

// SignInToConfiguredVendors signs in to every vendor whose cluster account is
// set up on this node, in the background, and records the result where
// `memql provider-auth check` reads it. It returns at once. A node that called
// DisableVendorSignInAtStart signs in to nothing and records that.
func (e *MemQLEngine) SignInToConfiguredVendors(ctx context.Context) {
	if e == nil {
		return
	}
	if e.vendorSignInAtStartOff.Load() {
		for _, vendor := range signInVendors {
			recordVendorWithoutSignIn(vendor, signInOutcomeNotUsed,
				"this node type calls no models and does not sign in to a vendor")
		}
		return
	}
	// A reload supersedes the follow-up an earlier pass left waiting.
	generation := e.vendorSignInGeneration.Add(1)
	e.vendorSignInWG.Add(1)
	go func() {
		defer e.vendorSignInWG.Done()
		e.signInToConfiguredVendors(ctx)
		e.followUpVendorSignIns(ctx, generation)
	}()
}

// signInVendors are the vendors this engine can hold a cluster account at.
var signInVendors = []string{metrics.FederationVendorAnthropic, metrics.FederationVendorOpenAI}

// followUpVendorSignIns presents the next identity token when a vendor's record
// says that token will settle it (see the file note). It ends when no vendor is
// waiting for a token, when a later pass has superseded this one, or after
// vendorSignInFollowUpTries looks.
func (e *MemQLEngine) followUpVendorSignIns(ctx context.Context, generation uint64) {
	for try := 0; try < vendorSignInFollowUpTries; try++ {
		var (
			pending []*vendorSignIn
			wait    time.Duration
		)
		for _, vendor := range signInVendors {
			signIn, _, ok := e.clusterVendorAccount(vendor)
			if !ok {
				continue
			}
			rec, recorded := signIn.Record()
			if !recorded || !nextTokenSettles(rec) || rec.NextTokenDue.IsZero() {
				continue
			}
			due := time.Until(rec.NextTokenDue) + vendorSignInFollowUpGrace
			if due < vendorSignInFollowUpGrace {
				due = vendorSignInFollowUpGrace
			}
			if due > vendorSignInFollowUpLimit {
				continue
			}
			pending = append(pending, signIn)
			if wait == 0 || due < wait {
				wait = due
			}
		}
		if len(pending) == 0 {
			return
		}
		if !vendorSignInFollowUpWait(ctx, wait) || e.vendorSignInGeneration.Load() != generation {
			return
		}
		arrived := false
		for _, signIn := range pending {
			if signIn.newTokenOnDisk() {
				arrived = true
			}
		}
		if !arrived {
			// kubelet has not replaced the file yet. There is nothing to
			// present, so no pass is spent on it; look again shortly.
			continue
		}
		e.signInToConfiguredVendors(ctx)
	}
}

// nextTokenSettles reports whether a record leaves the vendor unusable only
// until the pod's identity token is replaced.
func nextTokenSettles(rec vendorSignInRecord) bool {
	return rec.Outcome == signInOutcomeWaiting || (rec.Outcome == signInOutcomeDenied && rec.MayBeReplay)
}

// signInToConfiguredVendors is one pass over the vendors, waited for.
func (e *MemQLEngine) signInToConfiguredVendors(ctx context.Context) {
	logger := e.safeLogger()
	for _, vendor := range signInVendors {
		signIn, entry, ok := e.clusterVendorAccount(vendor)
		if !ok {
			outcome, detail := e.vendorWithoutAccount(vendor)
			recordVendorWithoutSignIn(vendor, outcome, detail)
			continue
		}
		// ONE CALL DOES BOTH. Listing models goes through the provider's own
		// client, whose middleware signs in when no bearer is in hand. The
		// sign-in is never made directly: the client is what knows the address
		// its vendor is reached at (ai_anthropic_federation.go).
		callCtx, cancel := context.WithTimeout(ctx, vendorSignInAtStartTimeout)
		listed, err := listModelsForCheck(callCtx, entry, vendor)
		cancel()
		if err != nil {
			// A refused sign-in is logged where it happened, with the vendor's
			// own reason (ai_federation_observer.go), and is in the record.
			// An accepted sign-in whose bearer the API then refused is recorded
			// here: without it the record would still say "accepted".
			// This line says which node is now without the vendor.
			signIn.NoteModelList(0, err)
			if logger != nil {
				outcome := "not attempted"
				if rec, ok := signIn.Record(); ok {
					outcome = rec.Outcome
				}
				logger.Warn("vendor sign-in: this node could not list the vendor's models",
					"component", ComponentName, "vendor", vendor, "provider", entry.Config.Name,
					"signIn", outcome, "error", err)
			}
			continue
		}
		signIn.NoteModelList(listed, nil)
		if logger != nil {
			logger.Info("vendor sign-in: signed in and the vendor listed its models",
				"component", ComponentName, "vendor", vendor, "models", listed)
		}
	}
}

// vendorWithoutAccount says why this node holds no usable cluster account at a
// vendor: nobody set it up, or ids are set and the engine itself refuses them.
//
// The difference is read from the RESOLVED auth of the vendor's own provider
// records, which is the only place both the environment and the cluster's
// storage have been consulted. The reason for a refusal is the constructor's
// own (which values are set, which are missing, or what is wrong with the
// identity token); it names values by their environment names and carries no
// credential.
func (e *MemQLEngine) vendorWithoutAccount(vendor string) (outcome, detail string) {
	for _, name := range e.providers.Names() {
		entry, ok := e.providers.Entry(name)
		if !ok || entry == nil || entry.Err() == nil {
			continue
		}
		idSet := false
		switch vendor {
		case metrics.FederationVendorAnthropic:
			idSet = anthropicProviderTypes[strings.ToLower(entry.Config.Type)] &&
				anthropicFederationFrom(entry.Config.Auth).anyIdSet()
		case metrics.FederationVendorOpenAI:
			idSet = isOpenAIProviderType(entry.Config.Type) &&
				openaiFederationFrom(entry.Config.Auth).anyIdSet()
		}
		if idSet {
			return signInOutcomeMisconfigured, truncateForLog(entry.Err().Error(), maxMisconfiguredNote)
		}
	}
	return signInOutcomeNotConfigured, "no federation ids are set for this vendor on this node"
}

// maxMisconfiguredNote bounds the engine's own reason on a record. Longer than
// a vendor's error body is allowed: the half-configured refusal names every
// value by its environment name, and cutting it would cut the missing ones.
const maxMisconfiguredNote = 1024
