package automations

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
)

// Authored field adjustments apply only to the author's own writes. Unlike
// an event automation's mutation call, a field write does not re-enter row
// authorization, so it must never acquire authority over another writer's row.
// Every writing node installs this synchronous hook; leader gating is for bus
// delivery and must not suppress the node actually persisting the row.
func (s *AuthoredScheduler) subscribeBeforeWrite(owner string, a *Automation) func() {
	source := "authored:" + authoredEntryKey(owner, a.Name)
	hooks := BuildBeforeWriteHooks(s.beforeWriteEngine, []*Automation{a}, s.beforeWriteRegistry, s.logger)
	for concept, list := range hooks {
		for i := range list {
			apply := list[i].Apply
			list[i].Apply = func(ctx context.Context, row map[string]any) error {
				ac, _ := auth.AccessFromContext(ctx)
				if ac == nil || ac.UserId != owner {
					return nil
				}
				s.mu.Lock()
				gate := s.globalGate
				s.mu.Unlock()
				if gate != nil && !gate() || s.breaker != nil && !s.breaker.Allow(owner, a.Name) {
					return nil
				}
				err := apply(auth.ContextWithClientOrigin(AuthorContext(ctx, owner)), row)
				if s.breaker != nil {
					if err == nil {
						s.breaker.RecordSuccess(owner, a.Name)
					} else {
						s.breaker.RecordFailure(owner, a.Name, err.Error())
					}
				}
				return err
			}
		}
		hooks[concept] = list
	}
	s.beforeWriteEngine.SetBeforeWriteHookSource(source, hooks)
	return func() { s.beforeWriteEngine.SetBeforeWriteHookSource(source, nil) }
}

// Authored hooks may adjust only caller-writable data, never server stamps,
// ownership, or relationships that can move a row across authorization scopes.
func (s *AuthoredScheduler) validateBeforeWriteFields(a *Automation) error {
	if a.BeforeWrite == nil {
		return nil
	}
	if s.loader.registry == nil {
		return fmt.Errorf("automation %q: authored before-write requires a concept registry [before_write_field]", a.Name)
	}
	c, err := s.loader.registry.Get(a.BeforeWrite.Concept)
	if err != nil {
		return fmt.Errorf("automation %q: unknown authored before-write concept [before_write_field]", a.Name)
	}
	allowed := map[string]bool{}
	for _, f := range c.PublicFields() {
		allowed[f] = true
	}
	if c.RowAuthz != nil {
		delete(allowed, c.RowAuthz.Owner)
		delete(allowed, c.RowAuthz.Account)
	}
	for _, r := range c.Relationships {
		delete(allowed, r.FieldSource)
	}
	for _, step := range a.Steps {
		if step.FieldWrite != nil && !allowed[step.FieldWrite.Field] {
			return fmt.Errorf("automation %q: authored field write %q is server-stamped, internal, or an ownership/relationship field [before_write_field]", a.Name, step.FieldWrite.Field)
		}
	}
	return nil
}
