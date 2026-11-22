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
// be presented once more. Apply reaches every node through the
// providers.reload broadcast, which is what carries that to the peers.
//
// Regression tests: ai_vendor_signin_boot_test.go, including the cross-node
// hop (TestAReloadBroadcastAllowsOneMoreTryOnThePeer). Never run against a
// vendor.

// vendorSignInAtStartTimeout bounds one vendor's sign-in and model list.
const vendorSignInAtStartTimeout = 45 * time.Second

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

// SignInToConfiguredVendors signs in to every vendor whose cluster account is
// set up on this node, in the background, and records the result where
// `memql provider-auth check` and Settings read it. It returns at once.
func (e *MemQLEngine) SignInToConfiguredVendors(ctx context.Context) {
	if e == nil {
		return
	}
	e.vendorSignInWG.Add(1)
	go func() {
		defer e.vendorSignInWG.Done()
		e.signInToConfiguredVendors(ctx)
	}()
}

// signInToConfiguredVendors is SignInToConfiguredVendors, waited for.
func (e *MemQLEngine) signInToConfiguredVendors(ctx context.Context) {
	logger := e.safeLogger()
	for _, vendor := range []string{metrics.FederationVendorAnthropic, metrics.FederationVendorOpenAI} {
		signIn, entry, ok := e.clusterVendorAccount(vendor)
		if !ok {
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
			// This line says which node is now without the vendor.
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
		signIn.NoteModelsListed(listed)
		if logger != nil {
			logger.Info("vendor sign-in: signed in and the vendor listed its models",
				"component", ComponentName, "vendor", vendor, "models", listed)
		}
	}
}
