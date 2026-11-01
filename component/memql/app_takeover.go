package memql

// app_takeover.go -- when an app stops partway through a step, the next ready app
// carries on from what it left.
//
// Owner, 2026-10-30: "If during usage we run out of credits, then that's fine. We get
// the error message, but actually what should happen is that instead of just saying, oh,
// I run out of credits for Codex, it should now attempt Claude, for Claude Code to take
// over."
//
// THIS IS NOT A REPLAY. A step that has already done work cannot be run again: the
// actions it took have effects. So the next app is not given the step as if nothing had
// happened. It is given the step, what is established to have happened, and where the
// work is, and asked to carry on. The rule is that a takeover needs three things:
//
//  1. THE MACHINE'S WORD THAT THE APP STOPPED ON ITS USAGE OR ITS LOGIN
//     (AppSessionEnd.stop_reason), read off the app's own protocol. Those reasons are
//     about the app and not about the work, and the app has finished its turn with no
//     call in flight. A sentence is not enough here, as it is before any work: anything
//     else that failed, a crash or a timeout included, may have stopped halfway through
//     an action, and is never taken over.
//
//  2. A COMPLETE RECORD. Every action the machine reported is on the journal, none was
//     dropped, and every output reached the Library. Otherwise nobody can tell the next
//     app what happened, and the call fails with what the first app said. What the app
//     CLAIMS does not establish anything, so its last words travel as its account, and
//     the journal is what is presented as fact.
//
//  3. A DOOR THAT ALLOWS IT. An app a person pinned stays that app, an Only strategy
//     stays that app, and a chat turn with no step has no record to hand over.
//
// WHERE THE WORK IS decides the machine. A run's sessions share one directory on a
// machine, so the next app there finds the files as they were left, and that machine is
// asked first. Another of the owner's machines may carry on when it is the only one with
// a ready app: its directory is not there, so what reached the Library is delivered as
// inputs and the brief says what stayed behind.
//
// A TAKEOVER'S RECORDING IS NOT A RECORDING OF THE GOAL. It starts in the middle, so its
// child run carries no goal signature, and procedure learning never mines it.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
)

// The reasons a machine may report for an app stopping partway, and the stable tag the
// failure carries on a decision row.
const (
	AppStopOutOfUsage     = "out_of_usage"
	AppStopSignedOut      = "signed_out"
	AppStoppedMidWorkCode = "app_stopped_mid_work"
)

// The bounds on what a brief carries. A long step must not push the request itself out
// of the next app's context, and what is cut is counted in the brief.
const (
	appTakeoverMaxActions     = 150
	appTakeoverMaxArgsRunes   = 300
	appTakeoverLastWordsRunes = 6000
	// appTakeoverMaxInputs bounds what is delivered to another machine. The rest stay
	// in the Library, and the brief says how many.
	appTakeoverMaxInputs = 64
	// appMCPActionPrefix starts the step id of a call the MCP node recorded. The machine
	// never saw those calls, so they are not what its count of actions counts.
	appMCPActionPrefix = "mcp-"
	// observationKindToolResult is the observation kind a recorded action is written as.
	observationKindToolResult = "tool_result"
)

// AppWorkAction is one recorded action of a session, as a brief presents it.
type AppWorkAction struct {
	Seq      int
	Tool     string
	Args     string
	ExitCode *int
	IsError  bool
	Error    string
	// FromMachine is false for a call the MCP node recorded on another replica.
	FromMachine bool
}

// AppStoppedMidWork means an app did work on a step and then stopped on its usage or
// its login, with a complete record of what it did.
//
// IT DOES NOT UNWRAP TO ErrAppUnavailable, unlike the refusals before any work: to every
// reader but the session door this is a failure of a step that may have caused effects,
// and it must not read as a door that was shut and can simply be tried again.
type AppStoppedMidWork struct {
	AppId          string
	RegistrationId string
	// Kind is AppStopOutOfUsage or AppStopSignedOut.
	Kind string
	// Reason is the app's own sentence.
	Reason string
	// SessionId and ChildRunId name where the whole record is, for a person.
	SessionId  string
	ChildRunId string
	// RecordedActions is how many actions the machine reported.
	RecordedActions int
	// Journal is what is established to have happened. The delegate fills it from the
	// recording run, and refuses a takeover when it cannot.
	Journal []AppWorkAction
	// LastWords is the end of what the app wrote. It is its account, not evidence.
	LastWords string
	// ArtifactIds are the Library artifacts its session produced.
	ArtifactIds []string
}

func (e *AppStoppedMidWork) Error() string {
	actions := "actions"
	if e.RecordedActions == 1 {
		actions = "action"
	}
	return fmt.Sprintf("%s %s after %d recorded %s: %s", appDisplayName(e.AppId), e.stoppedWords(), e.RecordedActions, actions, e.Reason)
}

// Code is the stable tag a decision row carries it under.
func (e *AppStoppedMidWork) Code() string { return AppStoppedMidWorkCode }

func (e *AppStoppedMidWork) stoppedWords() string {
	if e.Kind == AppStopSignedOut {
		return "lost its login"
	}
	return "ran out of usage"
}

// AppTakeoverBrief is what the next app is told, added to the end of the step's own
// prompt. machine is the registration it will run on.
func AppTakeoverBrief(stops []*AppStoppedMidWork, machine, app string) string {
	var b strings.Builder
	b.WriteString("[MemQL handover]\nYou are taking over this step partway through. Do not start it again.")
	for _, stop := range stops {
		b.WriteString("\n\n")
		writeAppTakeoverStop(&b, stop, machine)
	}
	b.WriteString("\n\nFinish the step as the request above asks. If your answer is prose, say in one sentence that you took over and why.")
	return b.String()
}

func writeAppTakeoverStop(b *strings.Builder, stop *AppStoppedMidWork, machine string) {
	name := appDisplayName(stop.AppId)
	fmt.Fprintf(b, "%s stopped because it %s. Its words: %q\n\n", name, stop.stoppedWords(), stop.Reason)

	listed, skipped := appTakeoverListed(stop.Journal)
	if len(listed) == 0 && skipped == 0 {
		b.WriteString("MemQL recorded no actions before it stopped.\n\n")
	} else {
		b.WriteString("What ran: MemQL recorded these actions as they happened, so they happened, whatever the account below says.")
		if skipped > 0 {
			fmt.Fprintf(b, " %d reads and notes are left out.", skipped)
		}
		b.WriteString("\n")
		omitted := 0
		if len(listed) > appTakeoverMaxActions {
			omitted = len(listed) - appTakeoverMaxActions
			listed = listed[omitted:]
		}
		if omitted > 0 {
			fmt.Fprintf(b, "(%d earlier actions are not listed.)\n", omitted)
		}
		for _, action := range listed {
			b.WriteString("- ")
			b.WriteString(appTakeoverActionLine(action))
			b.WriteString("\n")
		}
		b.WriteString("Do not repeat an action that reached outside the working directory (a push, a message, a payment, a deploy) without first checking that it did not take effect.\n\n")
	}

	if stop.RegistrationId == machine {
		b.WriteString("Where the work is: you are on the same machine, in the same working directory, so its files are as it left them. Read them before changing anything.")
	} else {
		delivered := len(stop.ArtifactIds)
		if delivered > appTakeoverMaxInputs {
			delivered = appTakeoverMaxInputs
		}
		plural := "s"
		if delivered == 1 {
			plural = ""
		}
		fmt.Fprintf(b, "Where the work is: you are on a different machine from the one it ran on, so its working directory is not here. %d file%s it produced were delivered into your working directory under their file names. Anything else it changed stayed on that machine.", delivered, plural)
		if extra := len(stop.ArtifactIds) - delivered; extra > 0 {
			fmt.Fprintf(b, " %d more are in the Library.", extra)
		}
	}

	if words := appLastRunes(strings.TrimSpace(stop.LastWords), appTakeoverLastWordsRunes); words != "" {
		fmt.Fprintf(b, "\n\nThe end of what %s wrote, which is its own account and not evidence:\n<<<\n%s\n>>>", name, words)
	}
}

// appTakeoverListed is the actions worth listing, and how many were left out. A read
// changes nothing and the app's own bookkeeping moves nothing outside it, and on a long
// step they would crowd out the actions that matter.
func appTakeoverListed(journal []AppWorkAction) (listed []AppWorkAction, skipped int) {
	for _, action := range journal {
		switch action.Tool {
		case "fs_read", "agent":
			skipped++
		default:
			listed = append(listed, action)
		}
	}
	return listed, skipped
}

func appTakeoverActionLine(action AppWorkAction) string {
	line := action.Tool
	if args := strings.TrimSpace(action.Args); args != "" {
		line += " " + appFirstRunes(args, appTakeoverMaxArgsRunes)
	}
	if action.ExitCode != nil {
		line += " exit " + strconv.Itoa(*action.ExitCode)
	}
	if action.IsError {
		line += " failed"
		if why := strings.TrimSpace(action.Error); why != "" {
			line += ": " + appFirstRunes(why, appTakeoverMaxArgsRunes)
		}
	}
	return line
}

func appFirstRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}

func appLastRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return "..." + string(runes[len(runes)-n:])
}

// AppTakeoverInputs is what is delivered to the next app's working directory. On the
// machine holding the work the files are already there, and pulling them flat beside
// themselves would only confuse it.
func AppTakeoverInputs(stops []*AppStoppedMidWork, machine string) []string {
	var out []string
	for _, stop := range stops {
		if stop.RegistrationId == machine {
			continue
		}
		ids := stop.ArtifactIds
		if len(ids) > appTakeoverMaxInputs {
			ids = ids[:appTakeoverMaxInputs]
		}
		out = append(out, ids...)
	}
	return out
}

// ReadAppWorkJournal reads what a session's recording run says happened, as its owner
// and past this node's result cache: a replica is reading rows that the machine's holder
// and, for MCP calls, a different node wrote moments ago.
func ReadAppWorkJournal(ctx context.Context, exec interface {
	Execute(context.Context, string) (*ExecuteResult, error)
}, owner, runId string) ([]AppWorkAction, error) {
	call, err := parser.RenderCall("workObservationsForOwnerRun", map[string]any{"runId": runId})
	if err != nil {
		return nil, err
	}
	res, err := exec.Execute(auth.ContextWithUserActor(ContextWithFreshRead(ctx), owner), "query "+call)
	if err != nil {
		return nil, err
	}
	return appWorkJournalFromRows(MaterializeRows(res)), nil
}

// appWorkJournalFromRows keeps the recorded actions, in the order they happened.
func appWorkJournalFromRows(rows []map[string]any) []AppWorkAction {
	var out []AppWorkAction
	for _, row := range rows {
		if mapString(row, "kind") != observationKindToolResult {
			continue
		}
		data, _ := row["data"].(map[string]any)
		tool := mapString(data, "tool")
		if tool == "" {
			continue
		}
		action := AppWorkAction{
			Tool:        tool,
			Args:        appWorkArgs(data["args"]),
			Error:       mapString(data, "error"),
			FromMachine: !strings.HasPrefix(mapString(data, "appActionId"), appMCPActionPrefix),
		}
		action.IsError, _ = data["isError"].(bool)
		if seq, ok := data["seq"].(float64); ok {
			// narrowing: GUARDED -- a seq is a small count written by this engine.
			action.Seq = int(seq)
		}
		if code, ok := data["exitCode"].(float64); ok {
			// narrowing: GUARDED -- a process exit status fits an int.
			n := int(code)
			action.ExitCode = &n
		}
		out = append(out, action)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// appWorkArgs renders an action's arguments, which the writer stores as encoded text.
func appWorkArgs(raw any) string {
	switch v := raw.(type) {
	case nil:
		return ""
	case string:
		return v
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// appWorkActionsFromMachine counts the actions the machine reported, which is what its
// RecordedActions is a count of. The MCP node's own steps are extra, and counting them
// could hide a lost one.
func appWorkActionsFromMachine(journal []AppWorkAction) int {
	n := 0
	for _, action := range journal {
		if action.FromMachine {
			n++
		}
	}
	return n
}

// AppJournalComplete reports whether the journal holds every action the machine
// reported. A write that failed is logged and swallowed on the recording path, so it is
// here that a hole is noticed.
func AppJournalComplete(journal []AppWorkAction, recorded int) bool {
	return appWorkActionsFromMachine(journal) >= recorded
}
