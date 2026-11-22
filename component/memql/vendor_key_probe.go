package memql

// Checking a person's API key WITHOUT SPENDING A TOKEN (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions K3, K11 and K12, safety rule S6).
//
// A key is checked before it is saved, once a day while it is idle, and when
// its owner asks. Every one of those is the same request: list the vendor's
// models with the key. Listing models sends no prompt and bills nothing, and a
// vendor that accepts the key for it accepts the key.
//
// TWO THINGS HAPPEN BEFORE ANY REQUEST.
//
// The SHAPE is checked (VendorKeyShapeRefusal): a value that cannot be a key
// for the chosen vendor is refused where it was typed, in words that never
// repeat it. An organization's ADMIN key is refused by shape too. It manages an
// organization and cannot call a model, and it is exactly the credential a
// person must not paste into a field that says "API key".
//
// The ADDRESS is checked (VendorKeysRefusedHere, K3). A key is sent to the
// vendor's own public address and nowhere else. A cluster that sends the
// vendor's calls to another address would never use the key, so adding one
// there is refused with the reason, and the key never leaves the node.
//
// THE REQUEST ITSELF goes to the vendor's own address whatever the cluster or
// the process environment says, through the same guarded HTTP client every
// vendor call uses, once, with no retry and a 20 second deadline. The answer is
// read by the one table every account failure is read by
// (classifyVendorAnswer), so a check and a real call agree about what a status
// means.
//
// WHAT A VERDICT IS, AND IS NOT. `good`, `refused` and `credit` are the
// vendor's word about the key. `use_only` is OpenAI saying the key is valid and
// may not list models (a restricted key): it is saved and checked by the calls
// it pays for (K12). `unreachable` is NOT a verdict on the key: a rate limit,
// an outage, a network failure or a cluster that cannot use keys. A caller
// never marks a key refused on it.
//
// OpenAI's restricted-key answer is not documented; see
// vendor_account_errors.go. It was not confirmed with a real restricted key.
//
// Regression tests: vendor_key_probe_test.go.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go/v3"
	openaioption "github.com/openai/openai-go/v3/option"
)

// The verdicts of a check.
const (
	// VendorKeyProbeGood: the vendor listed its models for the key.
	VendorKeyProbeGood = "good"
	// VendorKeyProbeRefused: the vendor does not accept the key.
	VendorKeyProbeRefused = "refused"
	// VendorKeyProbeCredit: the key's account is out of credit or at a limit.
	VendorKeyProbeCredit = "credit"
	// VendorKeyProbeUseOnly: the key is valid and may not list models, so it
	// can only be checked by the calls it pays for.
	VendorKeyProbeUseOnly = "use_only"
	// VendorKeyProbeUnreachable: nothing was learned about the key.
	VendorKeyProbeUnreachable = "unreachable"
)

// vendorKeyProbeTimeout bounds one check.
const vendorKeyProbeTimeout = 20 * time.Second

// maxVendorKeyLength is longer than any key either vendor issues.
const maxVendorKeyLength = 512

// VendorKeyProbe is what one check learned.
type VendorKeyProbe struct {
	// Verdict is one of the VendorKeyProbe constants.
	Verdict string
	// Detail is the vendor's own words behind a refusal, scrubbed of anything
	// shaped like a key, or this engine's words for `unreachable`. Empty when
	// good.
	Detail string
	// Models is how many models the vendor listed.
	Models int
	// At is when the check's request started: the time of its evidence.
	At time.Time
}

// VendorKeyShapeRefusal is why a value cannot be an API key for a vendor, in a
// sentence for the person who typed it, or "" when its shape is acceptable.
// The sentence never repeats the value.
func VendorKeyShapeRefusal(vendor, key string) string {
	if vendor != VendorAnthropic && vendor != VendorOpenAI {
		return "Choose Anthropic or OpenAI."
	}
	key = strings.TrimSpace(key)
	switch {
	case key == "":
		return "Paste the API key."
	case strings.ContainsFunc(key, func(r rune) bool { return unicode.IsSpace(r) || r < 0x21 || r > 0x7e }):
		return "An API key has no spaces or line breaks. Paste the key by itself."
	case len(key) > maxVendorKeyLength:
		return "That is too long to be an API key."
	}
	switch vendor {
	case VendorAnthropic:
		switch {
		case strings.HasPrefix(key, "sk-ant-admin"):
			return "That is an Anthropic Admin API key. It manages an organization and cannot call a model, so it is not accepted here. Add a standard API key."
		case !strings.HasPrefix(key, "sk-ant-"):
			return "That is not an Anthropic API key. An Anthropic key starts with sk-ant-."
		}
	case VendorOpenAI:
		switch {
		case strings.HasPrefix(key, "sk-admin-"):
			return "That is an OpenAI admin key. It manages an organization and cannot call a model, so it is not accepted here. Add a project or user API key."
		case strings.HasPrefix(key, "sk-ant-"):
			return "That is an Anthropic key. Choose Anthropic, or paste an OpenAI key."
		case !strings.HasPrefix(key, "sk-"):
			return "That is not an OpenAI API key. An OpenAI key starts with sk-."
		}
	}
	return ""
}

// VendorKeysRefusedHere is why this cluster cannot use an API key for a
// vendor, or "" when it can (K3).
//
// It asks the cluster's own text records of that vendor where they send their
// calls. When every one of them dials an address that is not the vendor's own,
// no call could ever be paid by a key, so a key is refused before it is checked
// or stored. One record that dials the vendor's own address is enough: a key
// can pay through that one, and each call still checks its own address
// (vendorAccountBinding.roundTrip).
func (r *ProviderRegistry) VendorKeysRefusedHere(vendor string) string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	entries := make([]*ProviderConfigEntry, 0, len(r.byName))
	for _, e := range r.byName {
		entries = append(entries, e)
	}
	r.mu.RUnlock()

	records := 0
	for _, e := range entries {
		if VendorOfEntry(e) != vendor {
			continue
		}
		records++
		if keysRefusedAt(e, vendor) == "" {
			return ""
		}
	}
	if records == 0 {
		// No record of the vendor at all: nothing dials anywhere, and a key
		// would simply have nothing to pay for. That is not this refusal.
		return ""
	}
	return fmt.Sprintf("this cluster sends %s calls to another address, so API keys cannot be used here", vendorDisplayName(vendor))
}

// ProbeVendorKey checks one key by listing the vendor's models with it.
func (r *ProviderRegistry) ProbeVendorKey(ctx context.Context, vendor, key string) VendorKeyProbe {
	at := time.Now().UTC()
	if refusal := VendorKeyShapeRefusal(vendor, key); refusal != "" {
		return VendorKeyProbe{Verdict: VendorKeyProbeUnreachable, Detail: refusal, At: at}
	}
	if refusal := r.VendorKeysRefusedHere(vendor); refusal != "" {
		return VendorKeyProbe{Verdict: VendorKeyProbeUnreachable, Detail: refusal, At: at}
	}
	return probeVendorKey(ctx, vendor, strings.TrimSpace(key), at)
}

// probeVendorKey makes the one request.
func probeVendorKey(ctx context.Context, vendor, key string, at time.Time) VendorKeyProbe {
	name := vendorDisplayName(vendor)
	origin := vendorKeyOrigins[vendor]
	ctx, cancel := context.WithTimeout(ctx, vendorKeyProbeTimeout)
	defer cancel()

	// What the vendor answered, read where the request is made: the status and
	// the scrubbed body, before either SDK turns them into an error of its own.
	var answered bool
	var status int
	var kind, reason string
	credential := func(req *http.Request, next accountNext) (*http.Response, error) {
		if !strings.EqualFold(requestOrigin(req), origin) {
			// Unreachable by construction: the client below is built for this
			// origin. It is asked anyway, because the cost of being wrong is a
			// person's key sent to an address nobody chose.
			return nil, fmt.Errorf("vendor key check: the request is not for %s's own address", name)
		}
		switch vendor {
		case VendorAnthropic:
			req.Header.Del("Authorization")
			req.Header.Set("X-Api-Key", key)
		case VendorOpenAI:
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Del("OpenAI-Project")
			req.Header.Del("OpenAI-Organization")
		}
		// The check carries a key somebody just typed: it follows no redirect
		// (keyBoundRedirectPolicy).
		resp, err := next(req.WithContext(withKeyBoundRequest(req.Context())))
		if err != nil || resp == nil {
			return resp, err
		}
		answered, status = true, resp.StatusCode
		if status < 200 || status >= 300 {
			kind, reason = classifyVendorAnswer(vendor, status, scrubAnswerBody(resp))
		}
		return resp, nil
	}

	var models int
	switch vendor {
	case VendorAnthropic:
		client := anthropic.NewClient(
			anthropicoption.WithHTTPClient(guardedHTTPClient(nil)),
			anthropicoption.WithBaseURL(origin),
			anthropicoption.WithMiddleware(func(req *http.Request, next anthropicoption.MiddlewareNext) (*http.Response, error) {
				return credential(req, accountNext(next))
			}),
			// No ambient credential of the SDK's own rides beside the key.
			anthropicoption.WithHeaderDel("X-Api-Key"),
			anthropicoption.WithHeaderDel("Authorization"),
			anthropicoption.WithMaxRetries(0),
		)
		if page, err := client.Models.List(ctx, anthropic.ModelListParams{}); err == nil && page != nil {
			models = len(page.Data)
		}
	case VendorOpenAI:
		client := openai.NewClient(
			openaioption.WithHTTPClient(guardedHTTPClient(nil)),
			openaioption.WithBaseURL(strings.TrimRight(origin, "/")+"/v1/"),
			openaioption.WithMiddleware(func(req *http.Request, next openaioption.MiddlewareNext) (*http.Response, error) {
				return credential(req, accountNext(next))
			}),
			openaioption.WithHeaderDel("Authorization"),
			openaioption.WithMaxRetries(0),
		)
		if page, err := client.Models.List(ctx); err == nil && page != nil {
			models = len(page.Data)
		}
	}

	probe := VendorKeyProbe{At: at}
	switch {
	case answered && status >= 200 && status < 300:
		probe.Verdict, probe.Models = VendorKeyProbeGood, models
	case kind == accountFailureRefused:
		probe.Verdict, probe.Detail = VendorKeyProbeRefused, reason
	case kind == accountFailureCredit:
		probe.Verdict, probe.Detail = VendorKeyProbeCredit, reason
	case kind == accountFailurePermission && vendor == VendorOpenAI && namesMissingPermission(reason):
		// K12: valid, and not allowed to list models.
		probe.Verdict, probe.Detail = VendorKeyProbeUseOnly, reason
	case kind == accountFailurePermission && vendor == VendorOpenAI:
		// K12, the other half: "any other refusal means the key is not saved".
		// A 403 that names no missing permission (a region OpenAI does not
		// serve, a project that was archived) verifies nothing about the key,
		// so it is neither stored as checked by use nor recorded against a key
		// that is already stored.
		probe.Verdict = VendorKeyProbeUnreachable
		probe.Detail = fmt.Sprintf("%s answered %d and did not say the key is missing a permission, so nothing about the key was learned: %s", name, status, reason)
	case kind == accountFailurePermission:
		probe.Verdict, probe.Detail = VendorKeyProbeRefused, reason
	case answered:
		probe.Verdict = VendorKeyProbeUnreachable
		probe.Detail = fmt.Sprintf("%s answered %d, which says nothing about the key. Try again in a moment.", name, status)
	default:
		probe.Verdict = VendorKeyProbeUnreachable
		probe.Detail = fmt.Sprintf("%s could not be reached to check the key. Try again in a moment.", name)
	}
	return probe
}
