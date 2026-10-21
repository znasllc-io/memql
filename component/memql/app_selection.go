package memql

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/id"
)

// AppSelection is one app on one owned machine. Selection never changes an
// active session; every new turn receives its complete input contract.
type AppSelection struct{ AppId, RegistrationId string }

type AppSelector interface {
	SelectApp(context.Context, AppCallRequest, bool, []string) (AppSelection, error)
}

type AppRoutingPolicy struct {
	Mode                                        string
	UsageWeight, SpeedWeight, ReliabilityWeight int
	Revision                                    string
}

func AppRoutingPolicyFrom(value any) AppRoutingPolicy {
	p := AppRoutingPolicy{Mode: "auto", UsageWeight: 50, SpeedWeight: 50, ReliabilityWeight: 50}
	row, ok := value.(map[string]any)
	if !ok {
		return p
	}
	if mode := mapString(row, "mode"); mode != "" {
		p.Mode = mode
	}
	p.Revision = mapString(row, "revision")
	for key, dst := range map[string]*int{"usageWeight": &p.UsageWeight, "speedWeight": &p.SpeedWeight, "reliabilityWeight": &p.ReliabilityWeight} {
		if n, ok := row[key].(float64); ok {
			*dst = int(n)
		}
		if n, ok := row[key].(int); ok {
			*dst = n
		}
	}
	return p
}

func (p AppRoutingPolicy) allows(app string) bool {
	return p.Mode == "auto" || p.Mode == "roundRobin" || p.Mode == app
}

// AppQuotaRefused means the app reported a quota refusal with zero model
// usage, no served model and no structured result. Other failures may have
// caused effects and are never safe to replay in another app.
type AppQuotaRefused struct{ Reason string }

func (e *AppQuotaRefused) Error() string { return e.Reason }
func (e *AppQuotaRefused) Code() string  { return "app_quota_refused" }
func (e *AppQuotaRefused) Unwrap() error { return ErrAppUnavailable }

func IsAppQuotaRefusal(message string) bool {
	message = strings.ToLower(message)
	for _, phrase := range []string{"hit your weekly limit", "hit your limit", "usage limit", "insufficient_quota", "credit balance is too low", "out of credits", "rate_limit", "rate limit exceeded"} {
		if strings.Contains(message, phrase) {
			return true
		}
	}
	return false
}

type appSignal struct {
	tokens, duration   float64
	samples, successes int
	usageSamples       int
}

func appSignals(rows []map[string]any, now time.Time) map[string]appSignal {
	out := map[string]appSignal{}
	for _, row := range rows {
		key := mapString(row, "app")
		s := out[key]
		end, _ := time.Parse(time.RFC3339Nano, mapString(row, "endedAt"))
		start, _ := time.Parse(time.RFC3339Nano, mapString(row, "startedAt"))
		if end.IsZero() || now.Sub(end) > 2*time.Hour {
			continue
		}
		usage, _ := row["usage"].(map[string]any)
		number := func(k string) float64 { v, _ := usage[k].(float64); return v }
		if usage["known"] == true {
			s.tokens += number("inputTokens") + number("outputTokens")
			s.usageSamples++
		}
		if mapString(row, "status") == "ended" || mapString(row, "status") == "failed" {
			s.samples++
			if mapString(row, "status") == "ended" {
				s.successes++
			}
			if !start.IsZero() && end.After(start) {
				s.duration += float64(end.Sub(start).Milliseconds())
			}
		}
		out[key] = s
	}
	return out
}

func (e *MemQLEngine) CheckMachineAppRouting(ctx context.Context, registration, app string) error {
	row, err := e.modelSetupRow(ContextWithFreshRead(ctx), registration)
	if err != nil {
		return err
	}
	features, _ := row["ownerFeatures"].(map[string]any)
	if mapString(row, "revokedAt") != "" || features["apps"] == false || !AppRoutingPolicyFrom(row["appRouting"]).allows(app) {
		return fmt.Errorf("this machine's app settings do not allow %s", app)
	}
	return nil
}

func (e *MemQLEngine) FilterAppDoors(ctx context.Context, doors []AppDoor) ([]AppDoor, error) {
	res, err := e.Execute(ContextWithFreshRead(ctx), "query appRoutingEvidence()")
	if err != nil {
		return nil, err
	}
	recent := MaterializeRows(res)
	filtered := make([]AppDoor, 0, len(doors))
	for _, door := range doors {
		kept := AppDoor{AppId: door.AppId}
		for _, machine := range door.Machines {
			if err := e.CheckMachineAppRouting(ctx, machine.RegistrationId, door.AppId); err != nil {
				continue
			}
			row, err := e.modelSetupRow(ContextWithFreshRead(ctx), machine.RegistrationId)
			if err != nil {
				return nil, err
			}
			settings, _ := row["appRouting"].(map[string]any)
			until, err := e.appBlockedUntil(ctx, machine.RegistrationId, door.AppId, settings, recent)
			if err != nil {
				return nil, err
			}
			if !time.Now().Before(until) {
				kept.Machines = append(kept.Machines, machine)
			}
		}
		filtered = append(filtered, kept)
	}
	return filtered, nil
}

func appScore(p AppRoutingPolicy, s appSignal, maxTokens, fastest float64) float64 {
	usage, speed, reliability := .5, .5, .5
	if s.usageSamples > 0 && maxTokens > 0 {
		usage = 1 - s.tokens/maxTokens
	}
	if s.samples > 0 {
		reliability = float64(s.successes) / float64(s.samples)
		if s.duration > 0 && fastest > 0 {
			speed = fastest / (s.duration / float64(s.samples))
		}
	}
	return float64(p.UsageWeight)*usage + float64(p.SpeedWeight)*speed + float64(p.ReliabilityWeight)*reliability
}

// SelectAppForCall uses persisted owner settings and observations. The row
// version fence reserves the next round-robin choice across replicas without
// holding a database lock while an app runs.
func (e *MemQLEngine) SelectAppForCall(ctx context.Context, req AppCallRequest, localOnly bool, excluded []string, doors []AppDoor) (AppSelection, error) {
	if auth.ActsForNoPerson(ctx, req.ActingUserId) || !sameUserId(actingUserFromContext(ctx), req.ActingUserId) {
		return AppSelection{}, fmt.Errorf("app selection requires its owner's authority")
	}
	for attempt := 0; attempt < 8; attempt++ {
		read := ContextWithFreshRead(ctx)
		res, err := e.Execute(read, "query appRoutingEvidence()")
		if err != nil {
			return AppSelection{}, err
		}
		recent := MaterializeRows(res)
		signals := appSignals(recent, time.Now())
		maxTokens, fastest := 0.0, math.Inf(1)
		for _, s := range signals {
			maxTokens = math.Max(maxTokens, s.tokens)
			if s.samples > 0 && s.duration > 0 {
				fastest = math.Min(fastest, s.duration/float64(s.samples))
			}
		}
		type candidate struct {
			AppSelection
			score    float64
			last     bool
			revision string
		}
		var candidates []candidate
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
			for _, machine := range door.Machines {
				if !machine.Online || (!machine.LocalStream && (localOnly || !machine.Forwardable)) || (req.Schema != nil && !machine.StructuredResult) {
					continue
				}
				row, err := e.modelSetupRow(read, machine.RegistrationId)
				if err != nil {
					return AppSelection{}, err
				}
				settings, _ := row["appRouting"].(map[string]any)
				until, err := e.appBlockedUntil(read, machine.RegistrationId, door.AppId, settings, recent)
				if err != nil {
					return AppSelection{}, err
				}
				if time.Now().Before(until) {
					continue
				}
				p := AppRoutingPolicyFrom(settings)
				if !p.allows(door.AppId) {
					continue
				}
				state, _ := row["appSelection"].(map[string]any)
				score := 0.0
				if p.Mode == "auto" {
					score = appScore(p, signals[door.AppId], maxTokens, fastest)
				}
				candidates = append(candidates, candidate{AppSelection{door.AppId, machine.RegistrationId}, score, mapString(state, "lastApp") == door.AppId, p.Revision})
			}
		}
		if len(candidates) == 0 {
			return AppSelection{}, &AppUnavailable{AppId: req.AppId, LastError: "no enabled app matches this machine's strategy and current availability"}
		}
		sort.SliceStable(candidates, func(i, j int) bool {
			a, b := candidates[i], candidates[j]
			if a.score != b.score {
				return a.score > b.score
			}
			if a.last != b.last {
				return !a.last
			}
			if a.RegistrationId != b.RegistrationId {
				return a.RegistrationId < b.RegistrationId
			}
			return a.AppId < b.AppId
		})
		chosen := candidates[0].AppSelection
		// Fence first, then recheck the cursor we ranked. A concurrent selector
		// or setting change makes us rank fresh evidence again.
		fenced, err := e.fenceWriteTarget(ctx, WorkerRegistrationConcept, chosen.RegistrationId)
		if err != nil {
			return AppSelection{}, err
		}
		fresh, err := e.modelSetupRow(ContextWithFreshRead(fenced), chosen.RegistrationId)
		if err != nil {
			return AppSelection{}, err
		}
		state, _ := fresh["appSelection"].(map[string]any)
		if (mapString(state, "lastApp") == chosen.AppId) != candidates[0].last {
			continue
		}
		p := AppRoutingPolicyFrom(fresh["appRouting"])
		if p.Revision != candidates[0].revision || !p.allows(chosen.AppId) {
			continue
		}
		call, _ := parser.RenderCall("recordWorkerAppSelection", map[string]any{"registrationId": chosen.RegistrationId, "app": chosen.AppId})
		_, err = e.Execute(auth.ContextWithInternalOrigin(fenced), call)
		if errors.Is(err, ErrRowVersionChanged) {
			continue
		}
		if err != nil {
			return AppSelection{}, err
		}
		return chosen, nil
	}
	return AppSelection{}, ErrRowVersionChanged
}

func (e *MemQLEngine) evaluateFleetSetAppRouting(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	registration := stringArg(args, "registrationId")
	if _, err := e.modelSetupRow(ctx, registration); err != nil {
		return nil, err
	}
	ctx, err := e.fenceWriteTarget(ctx, WorkerRegistrationConcept, registration)
	if err != nil {
		return nil, err
	}
	row, err := e.modelSetupRow(ContextWithFreshRead(ctx), registration)
	if err != nil {
		return nil, err
	}
	if mapString(row, "revokedAt") != "" {
		return nil, fmt.Errorf("pair this machine again before changing its apps")
	}
	if AppRoutingPolicyFrom(row["appRouting"]).Revision != stringArg(args, "revision") {
		return nil, fmt.Errorf("app settings changed; reload before saving")
	}
	p := AppRoutingPolicyFrom(args)
	if p.Mode != "auto" && p.Mode != "roundRobin" && p.Mode != "claude-code" && p.Mode != "codex" {
		return nil, fmt.Errorf("choose Auto, Round robin, Claude Code only or Codex only")
	}
	for _, n := range []int{p.UsageWeight, p.SpeedWeight, p.ReliabilityWeight} {
		if n < 0 || n > 100 {
			return nil, fmt.Errorf("app balance weights must be between 0 and 100")
		}
	}
	if p.UsageWeight+p.SpeedWeight+p.ReliabilityWeight == 0 {
		return nil, fmt.Errorf("at least one Auto balance weight must be positive")
	}
	revision := id.NewShortId()
	settings := map[string]any{"mode": p.Mode, "usageWeight": p.UsageWeight, "speedWeight": p.SpeedWeight, "reliabilityWeight": p.ReliabilityWeight, "revision": revision}
	previous, _ := row["appRouting"].(map[string]any)
	if retries, ok := previous["availabilityRetriedAt"]; ok {
		settings["availabilityRetriedAt"] = retries
	}
	call, _ := parser.RenderCall("setWorkerAppRouting", map[string]any{"registrationId": registration, "settings": settings})
	if _, err = e.Execute(auth.ContextWithInternalOrigin(ctx), call); err != nil {
		return nil, err
	}
	return singleVirtualRow("v1:worker:appRoutingResult", registration, settings)
}
