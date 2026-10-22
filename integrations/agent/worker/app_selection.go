package worker

import (
	"context"
	"fmt"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"sort"
)

func (s *EngineStore) CheckMachineAppRouting(ctx context.Context, registration, app string) error {
	return s.Engine.CheckMachineAppRouting(ctx, registration, app)
}

func filterAppDoors(ctx context.Context, store any, doors []memqlengine.AppDoor) ([]memqlengine.AppDoor, error) {
	if s, ok := store.(*EngineStore); ok && s != nil && s.Engine != nil {
		return s.Engine.FilterAppDoors(ctx, doors)
	}
	return doors, nil
}

func checkMachineAppRouting(ctx context.Context, store any, registration, app string) error {
	if s, ok := store.(interface {
		CheckMachineAppRouting(context.Context, string, string) error
	}); ok {
		return s.CheckMachineAppRouting(ctx, registration, app)
	}
	return nil
}

func (s *EngineStore) SelectAppForCall(ctx context.Context, req memqlengine.AppCallRequest, localOnly bool, excluded []string, doors []memqlengine.AppDoor) (memqlengine.AppSelection, error) {
	if s == nil || s.Engine == nil {
		return memqlengine.AppSelection{}, fmt.Errorf("app selection is unavailable")
	}
	return s.Engine.SelectAppForCall(ctx, req, localOnly, excluded, doors)
}

func (a *RemoteAppInference) SelectApp(ctx context.Context, req memqlengine.AppCallRequest, localOnly bool, excluded []string) (memqlengine.AppSelection, error) {
	doors, err := a.Doors(ctx, req.ActingUserId)
	if err != nil {
		return memqlengine.AppSelection{}, err
	}
	order, _ := a.AppOrder(ctx, req.ActingUserId)
	return selectAppWithStore(ctx, a.store, req, localOnly, excluded, doors, order)
}

func selectAppWithStore(ctx context.Context, store any, req memqlengine.AppCallRequest, localOnly bool, excluded []string, doors []memqlengine.AppDoor, order []string) (memqlengine.AppSelection, error) {
	if selector, ok := store.(interface {
		SelectAppForCall(context.Context, memqlengine.AppCallRequest, bool, []string, []memqlengine.AppDoor) (memqlengine.AppSelection, error)
	}); ok {
		return selector.SelectAppForCall(ctx, req, localOnly, excluded, doors)
	}
	// Embedded/legacy stores retain their existing explicit ordering. Production
	// EngineStore always takes the persisted machine-policy path above.
	rank := func(app string) int {
		for i, id := range order {
			if id == app {
				return i
			}
		}
		return len(order)
	}
	sort.SliceStable(doors, func(i, j int) bool { return rank(doors[i].AppId) < rank(doors[j].AppId) })
	for _, door := range doors {
		if req.AppId != "*" && req.AppId != door.AppId {
			continue
		}
		skip := false
		for _, app := range excluded {
			if app == door.AppId {
				skip = true
			}
		}
		if skip {
			continue
		}
		for _, m := range door.Machines {
			if m.Online && (m.LocalStream || (!localOnly && m.Forwardable)) && (req.Schema == nil || m.StructuredResult) {
				return memqlengine.AppSelection{AppId: door.AppId, RegistrationId: m.RegistrationId}, nil
			}
		}
	}
	return memqlengine.AppSelection{}, &memqlengine.AppUnavailable{AppId: req.AppId}
}
