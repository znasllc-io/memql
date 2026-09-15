package memql

import (
	"context"
	"fmt"
	"sort"
)

// BeforeWriteHook changes the incoming payload in-process, before validation.
// Hooks must neither persist nor publish. An error aborts the original write.
type BeforeWriteHook struct {
	Name  string
	On    string
	Apply func(context.Context, map[string]any) error
}

// SetBeforeWriteHooks atomically replaces the registry on each writing node.
func (e *MemQLEngine) SetBeforeWriteHooks(hooks map[string][]BeforeWriteHook) {
	e.SetBeforeWriteHookSource("shipped", hooks)
}

// SetBeforeWriteHookSource replaces one owner's hook set without removing
// shipped hooks or another author's hooks. A nil set removes that source.
func (e *MemQLEngine) SetBeforeWriteHookSource(source string, hooks map[string][]BeforeWriteHook) {
	e.beforeWriteMu.Lock()
	defer e.beforeWriteMu.Unlock()
	if e.beforeWriteSources == nil {
		e.beforeWriteSources = map[string]map[string][]BeforeWriteHook{}
	}
	copied := map[string][]BeforeWriteHook{}
	for concept, list := range hooks {
		copied[concept] = append([]BeforeWriteHook(nil), list...)
	}
	if hooks == nil {
		delete(e.beforeWriteSources, source)
	} else {
		e.beforeWriteSources[source] = copied
	}
	merged := map[string][]BeforeWriteHook{}
	var sources []string
	for name := range e.beforeWriteSources {
		sources = append(sources, name)
	}
	sort.Strings(sources)
	for _, name := range sources {
		for concept, list := range e.beforeWriteSources[name] {
			merged[concept] = append(merged[concept], list...)
		}
	}
	for _, list := range merged {
		sort.SliceStable(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	}
	e.beforeWriteHooks = merged
}

func (e *MemQLEngine) applyBeforeWrite(ctx context.Context, concept, id string, prior bool, payload map[string]any) error {
	e.beforeWriteMu.RLock()
	hooks := e.beforeWriteHooks[concept]
	e.beforeWriteMu.RUnlock()
	for _, hook := range hooks {
		if hook.On == "create" && prior || hook.On == "update" && !prior {
			continue
		}
		if err := hook.Apply(context.WithValue(ctx, beforeWriteIDKey{}, id), payload); err != nil {
			return fmt.Errorf("before-write automation %q on %s/%s: %w", hook.Name, concept, id, err)
		}
	}
	return nil
}

type beforeWriteIDKey struct{}

// BeforeWriteRowID is the intrinsic id of the original write while a hook runs.
func BeforeWriteRowID(ctx context.Context) string {
	id, _ := ctx.Value(beforeWriteIDKey{}).(string)
	return id
}

type beforeWriteRunKey struct{}

// ContextWithBeforeWrite marks a synchronous hook, whose reads must not journal writes.
func ContextWithBeforeWrite(ctx context.Context) context.Context {
	return context.WithValue(ctx, beforeWriteRunKey{}, true)
}

// InBeforeWrite reports whether the original write is executing its synchronous hook.
func InBeforeWrite(ctx context.Context) bool {
	active, _ := ctx.Value(beforeWriteRunKey{}).(bool)
	return active
}
