package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// PolicyDocument is an append-only configuration revision. Defaults are never
// persisted here: reset removes an override and reveals the shipped definition.
// One revision for the whole graph makes cycle validation and concurrent edits
// atomic, including edits to different policies which reference one another.
type PolicyDocument struct {
	Rules     map[string]RuleConfig   `json:"rules"`
	Revision  int64                   `json:"revision"`
	Overrides map[string]PolicyConfig `json:"overrides"`
	UpdatedBy string                  `json:"updatedBy"`
}

type PolicyStore interface {
	Read(context.Context) (PolicyDocument, error)
	CompareAndSwap(context.Context, int64, PolicyDocument) error
}

type PolicyRecord struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Primary      string   `json:"primary"`
	Fallbacks    []string `json:"fallbacks"`
	Chain        []string `json:"chain"`
	DefaultChain []string `json:"defaultChain"`
	Shipped      bool     `json:"shipped"`
	Customized   bool     `json:"customized"`
	Protected    bool     `json:"protected"`
	Revision     int64    `json:"revision"`
}

func clonePolicy(p *PolicyConfig) *PolicyConfig {
	if p == nil {
		return nil
	}
	c := *p
	c.Fallbacks = append([]string(nil), p.Fallbacks...)
	return &c
}

// AttachStore captures the canonical definitions once at bootstrap. The
// authored runtime remains core-first; customization never rewrites that DSL.
func (r *PolicyRegistry) AttachStore(store PolicyStore) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defaults = make(map[string]*PolicyConfig, len(r.byName))
	for name, p := range r.byName {
		r.defaults[name] = clonePolicy(p)
	}
	r.store = store
}

func (r *PolicyRegistry) configuration(ctx context.Context) (PolicyDocument, map[string]*PolicyConfig, error) {
	if r == nil {
		return PolicyDocument{}, nil, fmt.Errorf("policy registry is unavailable")
	}
	r.mu.RLock()
	store := r.store
	base := r.defaults
	if base == nil {
		base = r.byName
	}
	defaults := make(map[string]*PolicyConfig, len(base))
	for n, p := range base {
		defaults[n] = clonePolicy(p)
	}
	r.mu.RUnlock()
	doc := PolicyDocument{Overrides: map[string]PolicyConfig{}}
	if store != nil {
		var err error
		doc, err = store.Read(ctx)
		if err != nil {
			return doc, nil, fmt.Errorf("read routing policy configuration: %w", err)
		}
	}
	if doc.Overrides == nil {
		doc.Overrides = map[string]PolicyConfig{}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return doc, nil, err
	}
	var detached PolicyDocument
	if err := json.Unmarshal(raw, &detached); err != nil {
		return doc, nil, err
	}
	return detached, defaults, nil
}

func policySnapshot(defaults map[string]*PolicyConfig, doc PolicyDocument) (*PolicyRegistry, error) {
	snapshot := newPolicyRegistry()
	for n, p := range defaults {
		snapshot.byName[n] = clonePolicy(p)
	}
	for n, p := range doc.Overrides {
		c := p
		snapshot.byName[n] = clonePolicy(&c)
	}
	if err := ExpandPolicyChains(snapshot); err != nil {
		return nil, err
	}
	// The binding remains an invariant even if a reference tries to wrap it.
	if p, ok := snapshot.Lookup("embeddingsBinding"); ok && (p.Primary != "embedder:active" || len(p.Fallbacks) != 0) {
		return nil, fmt.Errorf("embeddingsBinding must use only embedder:active to preserve the index vector space")
	}
	return snapshot, nil
}

// Snapshot reads committed configuration from shared storage for one routing
// decision. No TTL or best-effort event can cause a peer to use a stale policy.
// A database failure refuses resolution; it never silently falls back to defaults.
func (r *PolicyRegistry) Snapshot(ctx context.Context) (*PolicyRegistry, error) {
	if r == nil {
		return nil, fmt.Errorf("policy registry unavailable")
	}
	r.mu.RLock()
	attached := r.store != nil
	r.mu.RUnlock()
	if !attached {
		return r, nil
	}
	doc, defaults, err := r.configuration(ctx)
	if err != nil {
		return nil, err
	}
	return policySnapshot(defaults, doc)
}

func (r *PolicyRegistry) Catalog(ctx context.Context) ([]PolicyRecord, error) {
	doc, defaults, err := r.configuration(ctx)
	if err != nil {
		return nil, err
	}
	snapshot, err := policySnapshot(defaults, doc)
	if err != nil {
		return nil, err
	}
	out := make([]PolicyRecord, 0, snapshot.Count())
	for _, p := range snapshot.All() {
		raw := p
		override, custom := doc.Overrides[p.Name]
		if custom {
			raw = &override
		}
		base, shipped := defaults[p.Name]
		var original []string
		if shipped {
			original = base.ProviderChain()
		}
		out = append(out, PolicyRecord{Name: p.Name, Description: raw.Description, Primary: raw.Primary, Fallbacks: append([]string{}, raw.Fallbacks...), Chain: p.ProviderChain(), DefaultChain: original, Shipped: shipped, Customized: shipped && custom, Protected: p.Name == "embeddingsBinding", Revision: doc.Revision})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

var policyNamePattern = regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`)

// Save and Reset are shared by manual and Ask operations. The caller supplies
// the revision they inspected, so stale edits cannot silently overwrite anyone.
func (r *PolicyRegistry) Save(ctx context.Context, expected int64, policy PolicyConfig) error {
	return r.change(ctx, expected, policy.Name, &policy)
}
func (r *PolicyRegistry) Reset(ctx context.Context, expected int64, name string) error {
	return r.change(ctx, expected, name, nil)
}
func (r *PolicyRegistry) change(ctx context.Context, expected int64, name string, policy *PolicyConfig) error {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || strings.TrimSpace(ac.UserId) == "" || !auth.CanAuthor(auth.UserContext{Role: ac.Role}) {
		return fmt.Errorf("policy changes require an authenticated owner or developer")
	}
	if !policyNamePattern.MatchString(name) {
		return fmt.Errorf("policy name must start with a lowercase letter and contain only letters and digits")
	}
	doc, defaults, err := r.configuration(ctx)
	if err != nil {
		return err
	}
	if expected < 0 || doc.Revision != expected {
		return fmt.Errorf("routing policies changed since revision %d; refresh and review before saving", expected)
	}
	if policy == nil {
		if defaults[name] == nil {
			return fmt.Errorf("%s is a custom policy and has no shipped default", name)
		}
		delete(doc.Overrides, name)
	} else {
		if len(policy.Description) > 4000 || len(policy.Fallbacks) > 16 {
			return fmt.Errorf("policy exceeds the description or 16-fallback limit")
		}
		policy = clonePolicy(policy)
		policy.Primary = strings.TrimSpace(policy.Primary)
		if err := languageParser.ValidatePolicyEntry(policy.Primary); err != nil {
			return err
		}
		seen := map[string]bool{policy.Primary: true}
		for j, entry := range policy.Fallbacks {
			entry = strings.TrimSpace(entry)
			if err := languageParser.ValidatePolicyEntry(entry); err != nil {
				return err
			}
			if seen[entry] {
				return fmt.Errorf("source %q is repeated", entry)
			}
			seen[entry] = true
			policy.Fallbacks[j] = entry
		}
		if name == "embeddingsBinding" && (policy.Primary != "embedder:active" || len(policy.Fallbacks) != 0) {
			return fmt.Errorf("embeddingsBinding must use only embedder:active; changing the bound model requires the embedding migration workflow")
		}
		doc.Overrides[name] = *policy
	}
	if _, err := policySnapshot(defaults, doc); err != nil {
		return err
	}
	r.mu.RLock()
	store := r.store
	r.mu.RUnlock()
	if store == nil {
		return fmt.Errorf("persistent policy storage is not configured")
	}
	doc.Revision = expected + 1
	doc.UpdatedBy = ac.UserId
	return store.CompareAndSwap(ctx, expected, doc)
}
