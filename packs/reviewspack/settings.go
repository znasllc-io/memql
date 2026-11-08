package reviewspack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// settings.go -- ONE STORE'S REVIEWS SETTINGS, MERGED ON EVERY WRITE.
//
// The settings are one row per store (SettingsRowID), and every write is a
// new version of that row. A version is a whole payload, so a write that
// carried only the field it changed would clear every other field. Every
// writer here reads the current row and changes only what it was given.
//
// # Defaults live here, not on the row
//
// A field the row does not carry has its default (EffectiveSettings): links
// last 14 days, a request goes out 14 days after the order, requests are off
// and the reviews are not shown. Absent is never "zero days".

// DefaultLinkLifetimeDays is how long a review link stays open when the store
// has not said.
const DefaultLinkLifetimeDays = 14

// DefaultRequestDelayDays is how long after an order its review request goes
// out when the store has not said.
const DefaultRequestDelayDays = 14

// Settings is one store's effective settings.
type Settings struct {
	PublicDisplay     bool
	LinkLifetimeDays  int
	RequestDelayDays  int
	RequestsEnabled   bool
	RequestTemplateID string
	// RequestSenderIdentityID is the sending identity the request email leaves
	// as: the store organization's own, or the installation's standing in for
	// an organization with no mailbox of its own. An operator's choice.
	RequestSenderIdentityID string
	NotifyEmail             string
	RemovalApproverUserID   string
}

// EffectiveSettings reads a settings row with every default applied.
func EffectiveSettings(row map[string]any) Settings {
	s := Settings{LinkLifetimeDays: DefaultLinkLifetimeDays, RequestDelayDays: DefaultRequestDelayDays}
	if row == nil {
		return s
	}
	s.PublicDisplay, _ = row["publicDisplay"].(bool)
	s.RequestsEnabled, _ = row["requestsEnabled"].(bool)
	if n := asInt(row["linkLifetimeDays"]); n > 0 {
		s.LinkLifetimeDays = n
	}
	// SET, NOT MERELY PRESENT: a query projects a field the row never wrote
	// as an empty value, and empty is not a zero-day delay.
	if v, set := row["requestDelayDays"]; set && v != nil {
		if n := asInt(v); n >= 0 {
			s.RequestDelayDays = n
		}
	}
	s.RequestTemplateID = strings.TrimSpace(asString(row["requestTemplateId"]))
	s.RequestSenderIdentityID = strings.TrimSpace(asString(row["requestSenderIdentityId"]))
	s.NotifyEmail = strings.TrimSpace(asString(row["notifyEmail"]))
	s.RemovalApproverUserID = strings.TrimSpace(asString(row["removalApproverUserId"]))
	return s
}

var emailShape = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// settingFields normalizes and validates each field a caller may change.
var settingFields = map[string]func(v any) (any, error){
	"publicDisplay":   boolSetting,
	"requestsEnabled": boolSetting,
	"linkLifetimeDays": func(v any) (any, error) {
		return intSetting(v, 1, 365, "a link's lifetime is 1 to 365 days")
	},
	"requestDelayDays": func(v any) (any, error) {
		return intSetting(v, 0, 365, "the request goes out 0 to 365 days after the order")
	},
	"requestTemplateId":       stringSetting(200),
	"requestSenderIdentityId": stringSetting(200),
	"removalApproverUserId":   stringSetting(200),
	"criteriaText":            stringSetting(4000),
	"notifyEmail": func(v any) (any, error) {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("must be text")
		}
		s = strings.TrimSpace(s)
		if s != "" && (len(s) > 320 || !emailShape.MatchString(s)) {
			return nil, fmt.Errorf("%q is not an email address", s)
		}
		return s, nil
	},
}

func boolSetting(v any) (any, error) {
	b, ok := v.(bool)
	if !ok {
		return nil, fmt.Errorf("must be true or false")
	}
	return b, nil
}

func intSetting(v any, lo, hi int, rule string) (any, error) {
	switch v.(type) {
	case int, int64, float64, json.Number:
	default:
		return nil, fmt.Errorf("must be a whole number: %s", rule)
	}
	n := asInt(v)
	if f, ok := v.(float64); ok && f != float64(n) {
		return nil, fmt.Errorf("must be a whole number: %s", rule)
	}
	if n < lo || n > hi {
		return nil, fmt.Errorf("%d is out of range: %s", n, rule)
	}
	return n, nil
}

func stringSetting(max int) func(v any) (any, error) {
	return func(v any) (any, error) {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("must be text")
		}
		s = strings.TrimSpace(s)
		if len(s) > max {
			return nil, fmt.Errorf("is longer than %d characters", max)
		}
		return s, nil
	}
}

// updateSettings changes the named settings of one store and keeps the rest.
func (p *Provider) updateSettings(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID := strings.TrimSpace(asString(args["storeId"]))
	if storeID == "" {
		return nil, fmt.Errorf("reviews: storeId is required; settings are per store")
	}
	site, err := p.siteForStore(ctx, asString(args["siteId"]), storeID)
	if err != nil {
		return nil, err
	}
	changes := map[string]any{}
	names := make([]string, 0, len(args))
	for k := range args {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if k == "storeId" || k == "siteId" {
			continue
		}
		check, known := settingFields[k]
		if !known {
			return nil, fmt.Errorf("reviews: %q is not a reviews setting", k)
		}
		v, err := check(args[k])
		if err != nil {
			return nil, fmt.Errorf("reviews: %s %v", k, err)
		}
		changes[k] = v
	}
	if len(changes) == 0 {
		return nil, fmt.Errorf("reviews: name at least one setting to change")
	}
	stampOrganization(changes, site)
	return p.writeSettings(ctx, storeID, changes)
}

// stampOrganization records the store's organization -- its site's -- on the
// settings, so later acts are checked against it (siteForStore). The site was
// already checked to belong to any organization the settings carried.
func stampOrganization(changes map[string]any, site map[string]any) {
	if acct := strings.TrimSpace(asString(site["accountId"])); acct != "" {
		changes["accountId"] = acct
	}
}

// errApproverCannotModerate refuses a removal approver who is not one of the
// people who can moderate the store (reviewsPeople).
var errApproverCannotModerate = errors.New("reviews: Choose someone in this store's organization who can moderate reviews.")

// writeSettings merges changes into the store's current settings row and
// answers the new version.
//
// Three rules decide who may write. A store with no settings row yet is set up
// by the cluster's operators; after that, whoever can read the row may change
// it. The owner who approves removals is named only by the current owner or by
// an operator -- a moderator who could name themselves would remove reviews
// nobody approved. And the mailbox the store's requests leave from is named
// only by an operator: it may be the installation's own, standing in for an
// organization with none, and only the installation's people may lend it.
//
// And one rule decides whom: the approver named is one of the people who can
// moderate the store, in its organization as reviewsPeople reads it
// (storeOrganization, moderatorsIn) -- never somebody outside it, never one of
// the cluster's operators, never a role test's stand-in. Whoever names them,
// operator or current owner.
func (p *Provider) writeSettings(ctx context.Context, storeID string, changes map[string]any) ([]memorynodes.MemoryNode, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || strings.TrimSpace(ac.UserId) == "" {
		return nil, fmt.Errorf("reviews: changing settings needs a signed-in person")
	}
	operator := auth.IsClusterOperator(ac.Role)
	current, err := p.settingsFor(ctx, storeID)
	if err != nil {
		return nil, err
	}
	if len(current) == 0 && !operator {
		return nil, fmt.Errorf("reviews: this store has no reviews settings yet; the cluster's operators set them up")
	}
	// THE ORGANIZATION IS THE OPERATORS' TO SET. Whose store this is must not
	// be decided by the first site that happens to touch it, so a change of
	// organization from anyone else is dropped and the row keeps its own. The
	// site's, which the caller stamped (stampOrganization), is taken first: the
	// approver check below reads it for settings that carry none.
	siteAccount := asString(changes["accountId"])
	if acct, set := changes["accountId"]; set && !operator &&
		bareAccount(asString(acct)) != bareAccount(asString(current["accountId"])) {
		delete(changes, "accountId")
	}
	if next, naming := changes["requestSenderIdentityId"]; naming && !operator &&
		strings.TrimSpace(asString(next)) != strings.TrimSpace(asString(current["requestSenderIdentityId"])) {
		return nil, fmt.Errorf("reviews: only the cluster's operators name the mailbox a store's review requests leave from")
	}
	if next, naming := changes["removalApproverUserId"]; naming && next != asString(current["removalApproverUserId"]) {
		now := strings.TrimSpace(asString(current["removalApproverUserId"]))
		if !operator && !sameUser(now, ac.UserId) {
			return nil, fmt.Errorf("reviews: only the store's current owner, or the cluster's operators, can name who approves removals")
		}
		if named := strings.TrimSpace(asString(next)); named != "" {
			// The settings' own organization, else the site's: the rule the
			// picker reads by. siteForStore has already matched the site to any
			// organization the settings carry, so the site's is safe to check
			// against; whether the write records it is the rule above.
			people, err := p.moderatorsIn(ctx, storeOrganization(current, siteAccount))
			if err != nil {
				return nil, err
			}
			if !slices.ContainsFunc(people, func(m Moderator) bool { return sameUser(m.UserID, named) }) {
				return nil, errApproverCannotModerate
			}
		}
	}

	// THE WHOLE ROW, merged: a version is a whole payload.
	merged := map[string]any{"settingsId": SettingsRowID(storeID), "storeId": storeID}
	for _, k := range settingsRowFields {
		if v, set := current[k]; set && v != nil {
			merged[k] = normalizeSetting(k, v)
		}
	}
	for k, v := range changes {
		merged[k] = v
	}
	if strings.TrimSpace(asString(merged["ownerUserId"])) == "" {
		merged["ownerUserId"] = strings.TrimSpace(ac.UserId)
	}
	if _, set := merged["publicDisplay"]; !set {
		merged["publicDisplay"] = false
	}
	if err := p.writer.Write(ctx, "putReviewSettings", merged); err != nil {
		return nil, err
	}
	return receipt("v1:reviews:settingsSaved", SettingsRowID(storeID), map[string]any{
		"storeId": storeID, "changed": sortedKeys(changes),
	})
}

// settingsRowFields are the settings row's own fields, in the order the write
// mutation declares them. Row intrinsics (id, createdAt) are not among them.
var settingsRowFields = []string{
	"ownerUserId", "accountId", "publicDisplay", "criteriaText", "linkLifetimeDays", "requestDelayDays",
	"requestsEnabled", "requestTemplateId", "notifyEmail", "removalApproverUserId", "requestSenderIdentityId",
}

// normalizeSetting turns a value read off a row into the type the write
// mutation declares: a whole number may come back as a float.
func normalizeSetting(field string, v any) any {
	switch field {
	case "linkLifetimeDays", "requestDelayDays":
		return asInt(v)
	case "publicDisplay", "requestsEnabled":
		b, _ := v.(bool)
		return b
	}
	return strings.TrimSpace(asString(v))
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// requireSiteForStore checks the site an act names is the store's own; see
// siteForStore.
func (p *Provider) requireSiteForStore(ctx context.Context, siteID, storeID string) error {
	_, err := p.siteForStore(ctx, siteID, storeID)
	return err
}

// siteForStore answers the site an act names, once it has checked the site is
// the STORE'S OWN. The organization gate judged the act against that site's
// organization, so the site must serve the store, and -- once the store's
// settings carry an organization -- belong to that organization: a site of
// another organization bound to the same store would let a person granted the
// part there act on this store.
func (p *Provider) siteForStore(ctx context.Context, siteID, storeID string) (map[string]any, error) {
	siteID = strings.TrimSpace(siteID)
	if siteID == "" {
		return nil, fmt.Errorf("reviews: siteId is required: the act is judged against the organization of the store's site")
	}
	site := p.siteRow(ctx, siteID)
	if site == nil {
		return nil, fmt.Errorf("reviews: site %q is not readable by this caller", siteID)
	}
	if !siteServesStore(site, storeID) {
		return nil, fmt.Errorf("reviews: site %q is not bound to store %q", siteID, storeID)
	}
	settings, err := p.settingsFor(ctx, storeID)
	if err != nil {
		return nil, err
	}
	if want := bareAccount(asString(settings["accountId"])); want != "" && bareAccount(asString(site["accountId"])) != want {
		return nil, fmt.Errorf("reviews: site %q belongs to another organization than store %q's reviews", siteID, storeID)
	}
	return site, nil
}

// bareAccount is an organization id without its concept prefix: one may be
// written either way.
func bareAccount(id string) string {
	id = strings.TrimSpace(id)
	if i := strings.LastIndex(id, ":"); i >= 0 {
		return id[i+1:]
	}
	return id
}
