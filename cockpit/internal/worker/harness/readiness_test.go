package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// readiness_test.go drives the checks against fakes that print what the real
// apps printed.
//
// THE FIXTURES ARE RECORDED, measured on 2026-10-29 with Claude Code 2.1.304 and
// codex-cli 0.162.1, against a real signed-in Mac, a real out-of-usage Codex, and
// fabricated expired logins in scratch config directories. Ids are replaced and
// fields no decision reads are left out; every field that remains is spelled as
// it was printed.

const (
	recordedClaudeInit = `{"type":"system","subtype":"init","session_id":"s1","tools":[],"mcp_servers":[],"model":"claude-haiku-5-5","apiKeySource":"none","claude_code_version":"2.1.304"}`

	recordedClaudeAnswered = recordedClaudeInit + `
{"type":"assistant","message":{"model":"claude-haiku-5-5","id":"m1","type":"message","role":"assistant","content":[{"type":"text","text":"OK."}]},"session_id":"s1"}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1793346600,"rateLimitType":"five_hour","overageStatus":"rejected","overageDisabledReason":"org_level_disabled","isUsingOverage":false,"unifiedWindows":{"five_hour":{"utilization":0.14,"resetsAt":1793346600},"seven_day":{"utilization":0.56,"resetsAt":1793487600}}},"session_id":"s1"}
{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed","duration_api_ms":1531,"total_cost_usd":0.0001207,"num_turns":1,"result":"OK.","usage":{"input_tokens":2,"output_tokens":5,"cache_creation_input_tokens":590,"cache_read_input_tokens":0},"modelUsage":{"claude-haiku-5-5":{"inputTokens":2,"outputTokens":5,"costUSD":0.0001207}},"session_id":"s1"}`

	recordedClaudeExpired = recordedClaudeInit + `
{"type":"assistant","message":{"model":"<synthetic>","id":"m1","role":"assistant","type":"message","content":[{"type":"text","text":"Failed to authenticate: OAuth session expired and could not be refreshed"}]},"session_id":"s1","error":"authentication_failed","is_api_error_message":true}
{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error","duration_api_ms":0,"total_cost_usd":0,"num_turns":1,"result":"Failed to authenticate: OAuth session expired and could not be refreshed","usage":{"input_tokens":0,"output_tokens":0},"modelUsage":{},"session_id":"s1"}`

	recordedClaudeNotLoggedIn = recordedClaudeInit + `
{"type":"assistant","message":{"model":"<synthetic>","id":"m1","role":"assistant","type":"message","content":[{"type":"text","text":"Not logged in · Please run /login"}]},"session_id":"s1","error":"authentication_failed","is_api_error_message":true}
{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error","duration_api_ms":0,"total_cost_usd":0,"num_turns":1,"result":"Not logged in · Please run /login","usage":{"input_tokens":0,"output_tokens":0},"modelUsage":{},"session_id":"s1"}`

	// Not reproducible on a healthy plan, so this one is shaped from the answered
	// stream's rate_limit_event with the status the event's own vocabulary names,
	// and the sentence TestQuotaRefusals in the engine already pins.
	shapedClaudeRejected = recordedClaudeInit + `
{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1793487600,"rateLimitType":"seven_day","overageStatus":"rejected","isUsingOverage":false,"unifiedWindows":{"five_hour":{"utilization":0.14,"resetsAt":1793346600},"seven_day":{"utilization":1,"resetsAt":1793487600}}},"session_id":"s1"}
{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error","duration_api_ms":0,"total_cost_usd":0,"num_turns":1,"result":"You've hit your weekly limit · resets Oct 31 at 4pm (America/Phoenix)","usage":{"input_tokens":0,"output_tokens":0},"modelUsage":{},"session_id":"s1"}`

	shapedClaudeOverage = recordedClaudeInit + `
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1793487600,"rateLimitType":"seven_day","overageStatus":"allowed","isUsingOverage":true,"unifiedWindows":{"seven_day":{"utilization":1,"resetsAt":1793487600}}},"session_id":"s1"}
{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed","result":"OK.","usage":{"input_tokens":2,"output_tokens":5},"modelUsage":{"claude-haiku-5-5":{"inputTokens":2,"outputTokens":5}},"session_id":"s1"}`

	recordedAuthStatusSignedIn  = `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","subscriptionType":"max"}`
	recordedAuthStatusSignedOut = `{"loggedIn":false,"authMethod":"none","apiProvider":"firstParty"}`
)

// fakeClaudeForChecks answers `auth status` with status and every other command
// with the ping's stream, logging each command line. It must never be the real
// thing, so it is a script in a directory of its own.
func fakeClaudeForChecks(t *testing.T, status string, statusExit int, ping string, pingExit int) (binary, log string) {
	t.Helper()
	log = filepath.Join(t.TempDir(), "argv.log")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> '" + log + "'\n" +
		"if [ \"$1 $2\" = 'auth status' ]; then\n" +
		"cat <<'STATUS_JSON'\n" + status + "\nSTATUS_JSON\n" +
		"exit " + strconv.Itoa(statusExit) + "\nfi\n" +
		"cat <<'PING_JSON'\n" + ping + "\nPING_JSON\n" +
		"exit " + strconv.Itoa(pingExit) + "\n"
	return fakeBinary(t, "claude", script), log
}

func claudeCheckSpec(t *testing.T, binary string) CheckSpec {
	t.Helper()
	return CheckSpec{Binary: binary, Dir: t.TempDir(), Launch: testLaunch}
}

func commandLines(t *testing.T, log string) []string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read command log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func TestClaudeCheckReadsAnAnsweredPingAsReady(t *testing.T) {
	binary, log := fakeClaudeForChecks(t, recordedAuthStatusSignedIn, 0, recordedClaudeAnswered, 0)
	got := CheckClaudeCode(context.Background(), claudeCheckSpec(t, binary))
	if got.State != ReadinessReady || !got.Spent {
		t.Fatalf("got %+v", got)
	}
	lines := commandLines(t, log)
	if len(lines) != 2 || lines[0] != "auth status" {
		t.Fatalf("expected the free question before the ping: %q", lines)
	}
	// The ping must not load the machine's own settings, servers or tools, and
	// must leave no session behind. Every flag here was run for real.
	for _, flag := range []string{"-p", "--model haiku", "--max-turns 1", "--setting-sources=", "--strict-mcp-config", "--no-session-persistence", "--output-format stream-json", "--verbose"} {
		if !strings.Contains(lines[1], flag) {
			t.Errorf("ping is missing %q: %q", flag, lines[1])
		}
	}
}

// The login is expired and `auth status` still says loggedIn: true, which is the
// whole reason a status command cannot answer this and a real call must.
func TestClaudeCheckReadsAnExpiredLoginAsSignedOut(t *testing.T) {
	binary, _ := fakeClaudeForChecks(t, recordedAuthStatusSignedIn, 0, recordedClaudeExpired, 1)
	got := CheckClaudeCode(context.Background(), claudeCheckSpec(t, binary))
	if got.State != ReadinessSignedOut {
		t.Fatalf("got %+v", got)
	}
}

func TestClaudeCheckAsksAuthStatusBeforeSpendingAnything(t *testing.T) {
	binary, log := fakeClaudeForChecks(t, recordedAuthStatusSignedOut, 1, recordedClaudeAnswered, 0)
	got := CheckClaudeCode(context.Background(), claudeCheckSpec(t, binary))
	if got.State != ReadinessSignedOut || got.Spent {
		t.Fatalf("got %+v", got)
	}
	if lines := commandLines(t, log); len(lines) != 1 {
		t.Fatalf("pinged an app that says it is signed out: %q", lines)
	}
}

// A ping must never spend API credit. These are every way this machine could
// bill something other than the subscription that the check can see beforehand.
func TestClaudeCheckNeverPingsWhenTheEnvironmentCouldBillAPICredit(t *testing.T) {
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"} {
		binary, log := fakeClaudeForChecks(t, recordedAuthStatusSignedIn, 0, recordedClaudeAnswered, 0)
		spec := claudeCheckSpec(t, binary)
		spec.Environ = []string{"HOME=/h", name + "=x"}
		got := CheckClaudeCode(context.Background(), spec)
		if got.State != ReadinessUnknown || got.Spent {
			t.Fatalf("%s: got %+v", name, got)
		}
		if lines := commandLines(t, log); len(lines) != 0 {
			t.Fatalf("%s: ran something anyway: %q", name, lines)
		}
	}
}

func TestClaudeCheckDoesNotPingAConsoleLogin(t *testing.T) {
	binary, log := fakeClaudeForChecks(t, `{"loggedIn":true,"authMethod":"console","apiProvider":"firstParty"}`, 0, recordedClaudeAnswered, 0)
	got := CheckClaudeCode(context.Background(), claudeCheckSpec(t, binary))
	if got.State != ReadinessUnknown || got.Spent {
		t.Fatalf("got %+v", got)
	}
	if lines := commandLines(t, log); len(lines) != 1 {
		t.Fatalf("pinged a console login: %q", lines)
	}
}

// The last guard, for a billing source only the app can see, such as a managed
// apiKeyHelper: stop on the init event, before the request is answered.
func TestClaudeCheckStopsAPingThatWouldBillAnAPIKey(t *testing.T) {
	init := strings.Replace(recordedClaudeInit, `"apiKeySource":"none"`, `"apiKeySource":"ANTHROPIC_API_KEY"`, 1)
	script := "#!/bin/sh\n" +
		"if [ \"$1 $2\" = 'auth status' ]; then echo '" + recordedAuthStatusSignedIn + "'; exit 0; fi\n" +
		"echo '" + init + "'\n" +
		"exec sleep 30\n"
	spec := claudeCheckSpec(t, fakeBinary(t, "claude", script))
	started := time.Now()
	got := CheckClaudeCode(context.Background(), spec)
	if got.State != ReadinessUnknown || !got.Spent {
		t.Fatalf("got %+v", got)
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Fatalf("the ping was not stopped: it took %s", took)
	}
}

// A refused ping spends nothing, so it must not use up the ceiling: a signed-out
// machine re-checks often and would otherwise run out of pings before anyone signs
// in.
func TestClaudeCheckCountsOnlyWhatItSpent(t *testing.T) {
	expired, _ := fakeClaudeForChecks(t, recordedAuthStatusSignedIn, 0, recordedClaudeExpired, 1)
	if got := CheckClaudeCode(context.Background(), claudeCheckSpec(t, expired)); got.Spent {
		t.Fatalf("an expired login spent nothing: %+v", got)
	}
	unreadable, _ := fakeClaudeForChecks(t, recordedAuthStatusSignedIn, 0, "garbage", 0)
	if got := CheckClaudeCode(context.Background(), claudeCheckSpec(t, unreadable)); !got.Spent {
		t.Fatalf("what cannot be read must count as spent: %+v", got)
	}
}

func TestClaudeCheckReadsARejectedLimitAsOutOfUsageWithItsReset(t *testing.T) {
	binary, _ := fakeClaudeForChecks(t, recordedAuthStatusSignedIn, 0, shapedClaudeRejected, 1)
	got := CheckClaudeCode(context.Background(), claudeCheckSpec(t, binary))
	if got.State != ReadinessOutOfUsage || !got.ResetsAt.Equal(time.Unix(1793487600, 0)) {
		t.Fatalf("got %+v", got)
	}
}

// On metered overage a working app is ready, and each ping would be paid for.
func TestClaudeCheckNotesMeteredOverage(t *testing.T) {
	binary, _ := fakeClaudeForChecks(t, recordedAuthStatusSignedIn, 0, shapedClaudeOverage, 0)
	got := CheckClaudeCode(context.Background(), claudeCheckSpec(t, binary))
	if got.State != ReadinessReady || !got.MeteredOverage || !got.ResetsAt.Equal(time.Unix(1793487600, 0)) {
		t.Fatalf("got %+v", got)
	}
}

// Anything the check does not recognise reads as unknown, and unknown stays
// selectable: an older CLI that refuses a flag must not take a machine out of
// routing.
func TestClaudeCheckThatCannotTellReportsUnknown(t *testing.T) {
	for name, build := range map[string]func(t *testing.T) string{
		"a flag the CLI does not know": func(t *testing.T) string {
			binary, _ := fakeClaudeForChecks(t, recordedAuthStatusSignedIn, 0, "error: unknown option '--tools'", 1)
			return binary
		},
		"an older CLI with no auth status": func(t *testing.T) string {
			binary, _ := fakeClaudeForChecks(t, "error: unknown command 'auth'", 1, "Credit check failed: unexpected network error", 1)
			return binary
		},
		"a failure it has no word for": func(t *testing.T) string {
			binary, _ := fakeClaudeForChecks(t, recordedAuthStatusSignedIn, 0, recordedClaudeInit+"\n"+`{"type":"result","subtype":"success","is_error":true,"result":"API Error: Connection error."}`, 1)
			return binary
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := CheckClaudeCode(context.Background(), claudeCheckSpec(t, build(t)))
			if got.State != ReadinessUnknown {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestClaudeCheckThatCannotStartReportsUnknown(t *testing.T) {
	got := CheckClaudeCode(context.Background(), claudeCheckSpec(t, filepath.Join(t.TempDir(), "absent")))
	if got.State != ReadinessUnknown {
		t.Fatalf("got %+v", got)
	}
}

func TestClaudeTurnReportsWhatTheAppSaidAboutItself(t *testing.T) {
	for name, tt := range map[string]struct {
		stream string
		exit   int
		want   string
	}{
		"a completed turn proves it works": {recordedClaudeAnswered, 0, ReadinessReady},
		"an expired login":                 {recordedClaudeExpired, 1, ReadinessSignedOut},
		"no login":                         {recordedClaudeNotLoggedIn, 1, ReadinessSignedOut},
		"a refused limit":                  {shapedClaudeRejected, 1, ReadinessOutOfUsage},
	} {
		t.Run(name, func(t *testing.T) {
			binary, _ := fakeClaude(t, prints(tt.stream)+"exit "+strconv.Itoa(tt.exit)+"\n")
			h := &claudeHeadless{}
			if err := h.Start(context.Background(), Spec{Binary: binary, Workspace: t.TempDir(), Launch: testLaunch}); err != nil {
				t.Fatal(err)
			}
			res, _ := h.Turn(context.Background(), "hello", SinkFunc(func(string, []byte) {}))
			if res.Readiness == nil || res.Readiness.State != tt.want {
				t.Fatalf("got %+v", res.Readiness)
			}
		})
	}
}

func TestClaudeTurnThatFailedForAnotherReasonSaysNothingAboutReadiness(t *testing.T) {
	binary, _ := fakeClaude(t, prints(recordedClaudeInit+"\n"+`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Bash tool crashed"}`)+"exit 1\n")
	h := &claudeHeadless{}
	if err := h.Start(context.Background(), Spec{Binary: binary, Workspace: t.TempDir(), Launch: testLaunch}); err != nil {
		t.Fatal(err)
	}
	res, _ := h.Turn(context.Background(), "hello", SinkFunc(func(string, []byte) {}))
	if res.Readiness != nil {
		t.Fatalf("a tool crash is no statement about the login or the plan: %+v", res.Readiness)
	}
}

// fakeCodexForChecks answers the two account methods the check sends, with
// recorded payloads, and logs every line it was sent.
func fakeCodexForChecks(t *testing.T, account, rateLimits string) (binary, log string) {
	t.Helper()
	log = filepath.Join(t.TempDir(), "wire.log")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" != \"app-server\" ]; then exit 64; fi\n" +
		"while IFS= read -r line; do\n" +
		"  printf '%s\\n' \"$line\" >> '" + log + "'\n" +
		"  id=$(printf '%s' \"$line\" | sed -n 's/^{\"jsonrpc\":\"2.0\",\"id\":\\([0-9]*\\),.*/\\1/p')\n" +
		"  case \"$line\" in\n" +
		"    *'\"method\":\"initialize\"'*) printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"userAgent\":\"codex-fake/1\"}}\\n' \"$id\" ;;\n" +
		"    *'\"method\":\"account/read\"'*) cat <<ACCOUNT_JSON\n{\"jsonrpc\":\"2.0\",\"id\":$id," + account + "}\nACCOUNT_JSON\n ;;\n" +
		"    *'\"method\":\"account/rateLimits/read\"'*) cat <<LIMITS_JSON\n{\"jsonrpc\":\"2.0\",\"id\":$id," + rateLimits + "}\nLIMITS_JSON\n ;;\n" +
		"  esac\n" +
		"done\n"
	return fakeBinary(t, "codex", script), log
}

const (
	recordedCodexAccount    = `"result":{"account":{"type":"chatgpt","email":"person@example.com","planType":"pro"},"requiresOpenaiAuth":true}`
	recordedCodexNoAccount  = `"result":{"account":null,"requiresOpenaiAuth":true,"workspaceRouting":null}`
	recordedCodexOutOfUsage = `"result":{"ordinaryUsageAllowed":false,"rateLimits":{"limitId":"codex","primary":{"usedPercent":100,"windowDurationMins":10080,"resetsAt":1793775123},"secondary":null,"credits":{"hasCredits":false,"unlimited":false,"balance":"0"},"spendControlReached":false,"planType":"pro","rateLimitReachedType":"rate_limit_reached"},"rateLimitUpsell":{"reset_at":1793775123}}`
	recordedCodexAuthNeeded = `"error":{"code":-32600,"message":"codex account authentication required to read rate limits"}`
	recordedCodexExpired    = `"error":{"code":-32603,"message":"failed to fetch codex rate limits: GET https://chatgpt.com/backend-api/wham/usage failed: 401 Unauthorized; content-type=text/plain; body={\\"error\\":{\\"message\\":\\"Could not parse your authentication token. Please try signing in again.\\"}}"}`
)

func TestCodexCheckReadsTheAccountAndItsLimits(t *testing.T) {
	reset := time.Unix(1793775123, 0)
	for name, tt := range map[string]struct {
		account, limits string
		want            string
		wantReset       time.Time
	}{
		"out of usage, as measured": {recordedCodexAccount, recordedCodexOutOfUsage, ReadinessOutOfUsage, reset},
		"signed out, as measured":   {recordedCodexNoAccount, recordedCodexAuthNeeded, ReadinessSignedOut, time.Time{}},
		"expired, as measured":      {recordedCodexNoAccount, recordedCodexExpired, ReadinessSignedOut, time.Time{}},
		"expired behind a cached account": {
			recordedCodexAccount, recordedCodexExpired, ReadinessSignedOut, time.Time{},
		},
		"usage left": {
			recordedCodexAccount, `"result":{"ordinaryUsageAllowed":true,"rateLimits":{"primary":{"usedPercent":12,"resetsAt":1793775123},"credits":{"hasCredits":false,"unlimited":false},"rateLimitReachedType":null}}`,
			ReadinessReady, time.Time{},
		},
		"an older Codex that states only its windows": {
			recordedCodexAccount, `"result":{"rateLimits":{"primary":{"usedPercent":100,"resetsAt":1793775123},"secondary":{"usedPercent":40,"resetsAt":1793000000},"credits":{"hasCredits":false,"unlimited":false}}}`,
			ReadinessOutOfUsage, reset,
		},
		"an exhausted window with credits to spend": {
			recordedCodexAccount, `"result":{"rateLimits":{"primary":{"usedPercent":100,"resetsAt":1793775123},"credits":{"hasCredits":true,"unlimited":false,"balance":"12"}}}`,
			ReadinessReady, time.Time{},
		},
		"an API key has no plan limit to read": {
			`"result":{"account":{"type":"apiKey"},"requiresOpenaiAuth":true}`, `"error":{"code":-32600,"message":"chatgpt authentication required to read rate limits"}`,
			ReadinessUnknown, time.Time{},
		},
		"the network is down": {
			recordedCodexAccount, `"error":{"code":-32603,"message":"failed to fetch codex rate limits: error sending request for url"}`,
			ReadinessUnknown, time.Time{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			binary, log := fakeCodexForChecks(t, tt.account, tt.limits)
			got := CheckCodex(context.Background(), CheckSpec{Binary: binary, Dir: t.TempDir(), Launch: testLaunch})
			if got.State != tt.want || !got.ResetsAt.Equal(tt.wantReset) {
				t.Fatalf("got %+v, want %s until %s", got, tt.want, tt.wantReset)
			}
			if got.Spent {
				t.Fatal("reading limits spends nothing, so it must not count against the ceiling")
			}
			if wire := strings.Join(commandLines(t, log), "\n"); strings.Contains(wire, "turn/start") || strings.Contains(wire, "thread/start") {
				t.Fatalf("a check opened a thread: %s", wire)
			}
		})
	}
}

// The owner: "The probe's reset time and credits drive when it comes back." The plan's
// window stays exhausted after credits are bought, and ordinaryUsageAllowed stays false
// because this is no longer ordinary usage, so the credits have to be read.
func TestCodexLimitsReadCreditsAndSpendControl(t *testing.T) {
	const window = `"primary":{"usedPercent":100,"windowDurationMins":10080,"resetsAt":1793775123},"secondary":null`
	read := func(ordinary, rest string) codexRateLimitsRead {
		var limits codexRateLimitsRead
		raw := `{"ordinaryUsageAllowed":` + ordinary + `,"rateLimits":{` + window + `,` + rest + `}}`
		if err := json.Unmarshal([]byte(raw), &limits); err != nil {
			t.Fatal(err)
		}
		return limits
	}
	for name, tt := range map[string]struct {
		limits codexRateLimitsRead
		want   string
	}{
		"no credits, as measured on an exhausted plan": {
			read("false", `"credits":{"hasCredits":false,"unlimited":false,"balance":"0"},"spendControlReached":false,"rateLimitReachedType":"rate_limit_reached"`),
			ReadinessOutOfUsage,
		},
		"credits bought after the plan ran out": {
			read("false", `"credits":{"hasCredits":true,"unlimited":false,"balance":"12"},"spendControlReached":false,"rateLimitReachedType":"rate_limit_reached"`),
			ReadinessReady,
		},
		"unlimited credits": {
			read("false", `"credits":{"hasCredits":false,"unlimited":true,"balance":"0"},"spendControlReached":false`),
			ReadinessReady,
		},
		"credits with spending stopped": {
			read("false", `"credits":{"hasCredits":true,"unlimited":false,"balance":"12"},"spendControlReached":true`),
			ReadinessOutOfUsage,
		},
	} {
		if got := tt.limits.signal().State; got != tt.want {
			t.Errorf("%s: got %s, want %s", name, got, tt.want)
		}
	}
}

func TestCodexCheckThatCannotStartReportsUnknown(t *testing.T) {
	got := CheckCodex(context.Background(), CheckSpec{Binary: filepath.Join(t.TempDir(), "absent"), Dir: t.TempDir(), Launch: testLaunch})
	if got.State != ReadinessUnknown {
		t.Fatalf("got %+v", got)
	}
}

func TestCodexTurnReportsWhatTheAppSaidAboutItself(t *testing.T) {
	completed := func(status, errorJSON string) string {
		return "      printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"turn\":{\"id\":\"turn_1\"}}}\\n' \"$id\"\n" +
			"      printf '{\"jsonrpc\":\"2.0\",\"method\":\"turn/completed\",\"params\":{\"threadId\":\"THREAD\",\"turn\":{\"id\":\"turn_1\",\"status\":\"" + status + "\",\"error\":" + errorJSON + ",\"items\":[]}}}\\n'\n"
	}
	for name, tt := range map[string]struct {
		body string
		want string
	}{
		"a usage limit, as measured": {
			completed("failed", `{\"message\":\"You have hit your usage limit. try again at Nov 3rd, 2026 11:52 PM.\",\"codexErrorInfo\":\"usageLimitExceeded\"}`),
			ReadinessOutOfUsage,
		},
		"no login, as measured": {
			completed("failed", `{\"message\":\"unexpected status 401 Unauthorized: Missing bearer or basic authentication in header, url: https://api.openai.com/v1/responses\",\"codexErrorInfo\":{\"httpConnectionFailed\":{\"httpStatusCode\":401}}}`),
			ReadinessSignedOut,
		},
		"an expired login, as measured": {
			completed("failed", `{\"message\":\"workspace routing discovery unauthorized (401)\",\"codexErrorInfo\":\"other\"}`),
			ReadinessSignedOut,
		},
		"a finished turn": {
			completed("completed", "null"),
			ReadinessReady,
		},
	} {
		t.Run(name, func(t *testing.T) {
			binary, _ := fakeCodexAppServer(t, tt.body)
			h := startCodexAppServer(t, codexSpec(t, binary))
			res, _ := h.Turn(context.Background(), "hello", SinkFunc(func(string, []byte) {}))
			if res.Readiness == nil || res.Readiness.State != tt.want {
				t.Fatalf("got %+v", res.Readiness)
			}
		})
	}
}

func TestCodexTurnThatFailedForAnotherReasonSaysNothingAboutReadiness(t *testing.T) {
	body := "      printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"turn\":{\"id\":\"turn_1\"}}}\\n' \"$id\"\n" +
		"      printf '{\"jsonrpc\":\"2.0\",\"method\":\"turn/completed\",\"params\":{\"threadId\":\"THREAD\",\"turn\":{\"id\":\"turn_1\",\"status\":\"failed\",\"error\":{\"message\":\"stream disconnected\",\"codexErrorInfo\":\"other\"},\"items\":[]}}}\\n'\n"
	binary, _ := fakeCodexAppServer(t, body)
	h := startCodexAppServer(t, codexSpec(t, binary))
	res, _ := h.Turn(context.Background(), "hello", SinkFunc(func(string, []byte) {}))
	if res.Readiness != nil {
		t.Fatalf("a dropped stream is no statement about the login or the plan: %+v", res.Readiness)
	}
}
