package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// readiness.go answers whether an app on this machine can take a call right now:
// signed in AND with usage left.
//
// A FILE CANNOT ANSWER THIS. An expired login leaves its files in place, and
// both apps' own status commands read those files: `claude auth status` printed
// loggedIn:true and `codex login status` printed "Logged in using ChatGPT" for a
// login whose refresh token was dead (measured 2026-10-29, Claude Code 2.1.304,
// codex-cli 0.162.1). So a check makes the cheapest real request each app offers:
//
//   - Codex reads its account and its limits over the app-server. No thread, no
//     turn, no tokens.
//   - Claude Code has no such read (`/usage` in print mode is local session
//     totals), so it is pinged: 2 input, 5 output and 590 cache-creation tokens
//     on the plan's cheapest model. A refused ping spends nothing.
//
// A CHECK NEVER BLOCKS ON DOUBT. Anything it cannot read is ReadinessUnknown, and
// unknown stays selectable. The worst a wrong "unknown" costs is one failed turn,
// which the engine moves past, where a wrong "signed_out" takes a working app off
// a machine for no reason anyone can see.

// The readiness words, as AppInfo.readiness carries them. Mirrored from the
// engine's component/worker/apps.go.
const (
	ReadinessReady      = "ready"
	ReadinessSignedOut  = "signed_out"
	ReadinessOutOfUsage = "out_of_usage"
	ReadinessUnknown    = "unknown"
)

// ReadinessSignal is what an app said about itself, from a check or a real turn.
type ReadinessSignal struct {
	State string
	// ResetsAt is when the limit behind this state resets, zero when the app
	// did not say.
	ResetsAt time.Time
	// MeteredOverage means the app works and is spending metered overage past
	// its plan, so a ping would be paid for. Pings wait for ResetsAt.
	MeteredOverage bool
	// Spent means this check may have used tokens: a ping the model answered, or
	// one whose outcome could not be read. It is what the spending ceiling counts.
	Spent bool
	// Detail is one sentence for this machine's log. It is the app's own words at
	// most and never leaves the machine.
	Detail string
}

// CheckSpec is one check's inputs.
type CheckSpec struct {
	Binary string
	// Dir is an empty directory the check's processes run in, so no project's
	// instructions or settings reach a ping.
	Dir string
	// Launch is the supervisor every process comes from, as in Spec.
	Launch LaunchFunc
	// Environ is the worker's environment, read for what could bill API credit.
	Environ []string
}

func unknownSignal(detail string) ReadinessSignal {
	return ReadinessSignal{State: ReadinessUnknown, Detail: detail}
}

// appSaysSignedOut and appSaysOutOfUsage read a failed call's own sentence.
//
// They mirror the engine's IsAppAuthRefusal and IsAppQuotaRefusal by hand, because
// this module cannot import component/memql. The measured sentences are pinned on
// both sides: TestIsAppAuthRefusalNamesTheMeasuredSentences there, the recorded
// streams in readiness_test.go here.
func appSaysSignedOut(message string) bool {
	message = strings.ToLower(message)
	for _, phrase := range []string{"failed to authenticate", "oauth session expired", "not logged in", "run /login", "401 unauthorized", "unauthorized (401)", "could not be refreshed", "sign in again", "signing in again", "authentication required"} {
		if strings.Contains(message, phrase) {
			return true
		}
	}
	return false
}

func appSaysOutOfUsage(message string) bool {
	message = strings.ToLower(message)
	for _, phrase := range []string{"hit your weekly limit", "hit your limit", "usage limit", "insufficient_quota", "credit balance is too low", "out of credits"} {
		if strings.Contains(message, phrase) {
			return true
		}
	}
	return false
}

// claudeBillingSwitches are the settings under which a `claude` process may bill
// something other than the subscription. A ping is never sent under one: the
// owner's rule is that a check never spends paid API credit, and the sessions
// themselves keep whatever this machine was set up to use.
var claudeBillingSwitches = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_BASE_URL",
	"CLAUDE_CODE_USE_BEDROCK",
	"CLAUDE_CODE_USE_VERTEX",
	"CLAUDE_CODE_USE_FOUNDRY",
}

func claudeBillingSwitch(environ []string) string {
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		for _, candidate := range claudeBillingSwitches {
			if name == candidate {
				return name
			}
		}
	}
	return ""
}

// claudePingArgs is the ping, as run for real on 2.1.304: the plan's cheapest
// model, one turn, a short system prompt so the app's own long one is not cached
// at the owner's expense, no tools, none of this machine's settings or MCP
// servers, and nothing left behind in the session history.
var claudePingArgs = []string{
	"-p", "--model", "haiku", "--max-turns", "1",
	"--system-prompt", "Reply OK.", "--tools", "", "--setting-sources=", "--strict-mcp-config",
	"--no-session-persistence", "--output-format", "stream-json", "--verbose", "--", "ok",
}

// CheckClaudeCode measures Claude Code, spending as little as it can.
//
// In order: what could bill API credit (nothing is run), `auth status` (no network
// and no tokens, so a signed-out machine costs nothing and a console login is never
// pinged), then the ping. `auth status` failing or printing something unreadable is
// an older CLI and moves on to the ping, whose init event is the last guard.
func CheckClaudeCode(ctx context.Context, spec CheckSpec) ReadinessSignal {
	if name := claudeBillingSwitch(spec.Environ); name != "" {
		return unknownSignal(name + " is set, so a ping could bill API credit; not pinged")
	}
	status, known := claudeAuthStatus(ctx, spec)
	if known && !status.LoggedIn {
		return ReadinessSignal{State: ReadinessSignedOut, Detail: "claude auth status says no one is logged in"}
	}
	if known && status.AuthMethod != "" && status.AuthMethod != "claude.ai" {
		return unknownSignal("signed in through " + status.AuthMethod + ", which may bill API credit; not pinged")
	}
	return claudePing(ctx, spec)
}

type claudeAuthStatusAnswer struct {
	LoggedIn   bool   `json:"loggedIn"`
	AuthMethod string `json:"authMethod"`
}

func claudeAuthStatus(ctx context.Context, spec CheckSpec) (claudeAuthStatusAnswer, bool) {
	out, err := collectOutput(ctx, spec, []string{spec.Binary, "auth", "status"}, claudeEnv(nil))
	if err != nil && len(out) == 0 {
		return claudeAuthStatusAnswer{}, false
	}
	// `auth status` exits 1 when signed out, so the exit code is not the answer.
	var raw struct {
		LoggedIn   *bool  `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	if json.Unmarshal(out, &raw) != nil || raw.LoggedIn == nil {
		return claudeAuthStatusAnswer{}, false
	}
	return claudeAuthStatusAnswer{LoggedIn: *raw.LoggedIn, AuthMethod: strings.TrimSpace(raw.AuthMethod)}, true
}

func claudePing(ctx context.Context, spec CheckSpec) ReadinessSignal {
	proc, err := spec.Launch(ctx, spec.Dir, append([]string{spec.Binary}, claudePingArgs...), claudeEnv(nil), false)
	if err != nil {
		return unknownSignal("the ping could not start: " + err.Error())
	}
	turn := &claudeTurn{sink: SinkFunc(func(string, []byte) {}), rec: newRecording(), cwd: spec.Dir, stopUnlessSubscription: proc.Terminate}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			proc.Terminate()
		case <-done:
		}
	}()
	var pumps sync.WaitGroup
	pumps.Add(1)
	go func() {
		defer pumps.Done()
		turn.pumpStderr(proc.Stderr())
	}()
	turn.pumpStdout(proc.Stdout())
	pumps.Wait()
	_ = proc.Wait()
	close(done)

	if turn.billingStopped {
		return ReadinessSignal{State: ReadinessUnknown, Spent: true,
			Detail: "the ping was stopped: Claude Code reported API key source " + turn.apiKeySource}
	}
	spent := true
	if ev := turn.result; ev != nil {
		usage := ev.usage()
		spent = len(ev.ModelUsage) != 0 || usage.InputTokens != 0 || usage.OutputTokens != 0
	}
	if signal := turn.readiness(); signal != nil {
		signal.Spent = spent
		return *signal
	}
	detail := strings.TrimSpace(string(turn.stderrTail))
	if turn.result != nil {
		detail = strings.TrimSpace(turn.result.Result)
	}
	return ReadinessSignal{State: ReadinessUnknown, Spent: spent, Detail: "the ping's outcome could not be read: " + detail}
}

// claudeRateLimitEvent is Claude Code's stream-json rate_limit_event, as 2.1.304
// printed it. status is allowed, allowed_warning or rejected.
type claudeRateLimitEvent struct {
	Info struct {
		Status         string `json:"status"`
		ResetsAt       int64  `json:"resetsAt"`
		IsUsingOverage bool   `json:"isUsingOverage"`
	} `json:"rate_limit_info"`
}

func (e *claudeRateLimitEvent) resetsAt() time.Time {
	if e == nil || e.Info.ResetsAt <= 0 {
		return time.Time{}
	}
	return time.Unix(e.Info.ResetsAt, 0)
}

// readiness is what this turn says about the app, or nil when it says nothing.
//
// ONLY A STATEMENT COUNTS. A turn that failed some other way (a tool crash, a
// dropped connection) says nothing about the login or the plan.
func (t *claudeTurn) readiness() *ReadinessSignal {
	ev := t.result
	if ev != nil && !ev.IsError {
		signal := &ReadinessSignal{State: ReadinessReady}
		if t.rateLimit != nil && t.rateLimit.Info.IsUsingOverage {
			signal.MeteredOverage = true
			signal.ResetsAt = t.rateLimit.resetsAt()
		}
		return signal
	}
	text := ""
	if ev != nil {
		text = ev.Result + " " + strings.Join(ev.Errors, " ")
	}
	if t.authFailed || appSaysSignedOut(text) {
		return &ReadinessSignal{State: ReadinessSignedOut, Detail: strings.TrimSpace(text)}
	}
	if (t.rateLimit != nil && t.rateLimit.Info.Status == "rejected") || appSaysOutOfUsage(text) {
		return &ReadinessSignal{State: ReadinessOutOfUsage, ResetsAt: t.rateLimit.resetsAt(), Detail: strings.TrimSpace(text)}
	}
	return nil
}

// collectOutput runs one short command to its end and returns its stdout.
func collectOutput(ctx context.Context, spec CheckSpec, argv, env []string) ([]byte, error) {
	proc, err := spec.Launch(ctx, spec.Dir, argv, env, false)
	if err != nil {
		return nil, err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			proc.Terminate()
		case <-done:
		}
	}()
	var pumps sync.WaitGroup
	pumps.Add(1)
	go func() {
		defer pumps.Done()
		_, _ = io.Copy(io.Discard, proc.Stderr())
	}()
	out, readErr := io.ReadAll(io.LimitReader(proc.Stdout(), 64*1024))
	_, _ = io.Copy(io.Discard, proc.Stdout())
	pumps.Wait()
	if waitErr := proc.Wait(); waitErr != nil {
		return out, waitErr
	}
	return out, readErr
}

// codexMethodAccountRead and codexMethodRateLimitsRead are the two reads a check
// makes. Source: `codex app-server generate-json-schema`, codex-cli 0.162.1.
const (
	codexMethodAccountRead    = "account/read"
	codexMethodRateLimitsRead = "account/rateLimits/read"
)

type codexAccountRead struct {
	Account *struct {
		Type string `json:"type"`
	} `json:"account"`
	RequiresOpenaiAuth bool `json:"requiresOpenaiAuth"`
}

type codexLimitWindow struct {
	UsedPercent float64 `json:"usedPercent"`
	ResetsAt    int64   `json:"resetsAt"`
}

type codexRateLimitsRead struct {
	OrdinaryUsageAllowed *bool `json:"ordinaryUsageAllowed"`
	RateLimits           struct {
		Primary              *codexLimitWindow `json:"primary"`
		Secondary            *codexLimitWindow `json:"secondary"`
		RateLimitReachedType *string           `json:"rateLimitReachedType"`
		SpendControlReached  bool              `json:"spendControlReached"`
		Credits              *struct {
			HasCredits bool `json:"hasCredits"`
			Unlimited  bool `json:"unlimited"`
		} `json:"credits"`
	} `json:"rateLimits"`
	RateLimitUpsell *struct {
		ResetAt int64 `json:"reset_at"`
	} `json:"rateLimitUpsell"`
}

// signal decides from Codex's own answer. ordinaryUsageAllowed is the plain word
// for it on 0.162.1; a Codex that does not state it is read from its windows.
//
// CREDITS ARE READ BEFORE ALL OF THAT, because they are how a person brings an
// exhausted plan back without waiting: the window stays at 100% and the word stays
// false, since this is no longer ordinary usage. Only spending being stopped outranks
// them. A plan with no credits was measured; one with credits was not, because none
// were bought here. If Codex refused a turn on credits anyway, the turn would read out
// of usage and the next check ready, so a real call would meet one refusal every five
// minutes, which costs nothing and moves on to the next app.
func (r codexRateLimitsRead) signal() ReadinessSignal {
	var latest int64
	exhausted := false
	for _, window := range []*codexLimitWindow{r.RateLimits.Primary, r.RateLimits.Secondary} {
		if window != nil && window.UsedPercent >= 100 {
			exhausted = true
			if window.ResetsAt > latest {
				latest = window.ResetsAt
			}
		}
	}
	if latest == 0 && r.RateLimitUpsell != nil {
		latest = r.RateLimitUpsell.ResetAt
	}
	blocked := r.RateLimits.SpendControlReached
	if credits := r.RateLimits.Credits; credits == nil || (!credits.HasCredits && !credits.Unlimited) {
		blocked = blocked || r.RateLimits.RateLimitReachedType != nil
		if r.OrdinaryUsageAllowed != nil {
			blocked = blocked || !*r.OrdinaryUsageAllowed
		} else {
			blocked = blocked || exhausted
		}
	}
	if !blocked {
		return ReadinessSignal{State: ReadinessReady}
	}
	signal := ReadinessSignal{State: ReadinessOutOfUsage, Detail: "Codex says this plan is out of usage"}
	if latest > 0 {
		signal.ResetsAt = time.Unix(latest, 0)
	}
	return signal
}

// CheckCodex measures Codex through its app-server. It opens no thread and runs no
// turn, so it spends nothing, and it is the one place a login or a limit can be
// asked about directly.
func CheckCodex(ctx context.Context, spec CheckSpec) ReadinessSignal {
	proc, err := spec.Launch(ctx, spec.Dir, []string{spec.Binary, "app-server"}, nil, true)
	if err != nil {
		return unknownSignal("the app-server could not start: " + err.Error())
	}
	conn := newJSONRPCConn(proc, func(string, json.RawMessage, []byte) {})
	defer conn.close()
	if _, err := conn.call(ctx, "codex check", codexMethodInitialize, codexInitializeParams()); err != nil {
		return unknownSignal("the app-server did not answer: " + err.Error())
	}
	if err := conn.notifyServer(codexMethodInitialized, nil); err != nil {
		return unknownSignal("the app-server did not answer: " + err.Error())
	}

	var account codexAccountRead
	raw, accountErr := conn.call(ctx, "codex check", codexMethodAccountRead, map[string]any{"refreshToken": false})
	accountKnown := accountErr == nil && json.Unmarshal(raw, &account) == nil
	if accountKnown && account.Account != nil && account.Account.Type == "apiKey" {
		return unknownSignal("Codex is signed in with an API key, which has no plan limit to read")
	}

	raw, limitsErr := conn.call(ctx, "codex check", codexMethodRateLimitsRead, nil)
	if limitsErr != nil {
		var refusal *rpcError
		if errors.As(limitsErr, &refusal) && appSaysSignedOut(refusal.Message) {
			return ReadinessSignal{State: ReadinessSignedOut, Detail: refusal.Message}
		}
		if accountKnown && account.Account == nil && account.RequiresOpenaiAuth {
			return ReadinessSignal{State: ReadinessSignedOut, Detail: "Codex has no account signed in"}
		}
		return unknownSignal(fmt.Sprintf("Codex could not read its limits: %v", limitsErr))
	}
	var limits codexRateLimitsRead
	if err := json.Unmarshal(raw, &limits); err != nil {
		return unknownSignal("Codex answered with limits this build cannot read: " + err.Error())
	}
	return limits.signal()
}

// codexTurnReadiness is what a finished turn says about the app, or nil. The error
// info is the structured word where Codex gives one; workspace routing's 401 comes
// as "other", so the sentence is read too.
func codexTurnReadiness(status, reason string, info json.RawMessage) *ReadinessSignal {
	switch status {
	case codexTurnCompleted:
		return &ReadinessSignal{State: ReadinessReady}
	case codexTurnFailed:
		words := string(info)
		if strings.Contains(words, "usageLimitExceeded") || appSaysOutOfUsage(reason) {
			return &ReadinessSignal{State: ReadinessOutOfUsage, Detail: reason}
		}
		if strings.Contains(words, `"httpStatusCode":401`) || strings.Contains(words, "unauthorized") || appSaysSignedOut(reason) {
			return &ReadinessSignal{State: ReadinessSignedOut, Detail: reason}
		}
	}
	return nil
}
