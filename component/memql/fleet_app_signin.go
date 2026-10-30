package memql

// The Fleet app's Sign in action. A person presses Sign in on one of their machines,
// and the app on that machine runs its own login while they finish it wherever they
// are, because the page opens in their browser and no browser opens on the machine.
//
// THE SHAPE IS THE MODEL PULL'S: a builtin decides and writes a row, the replica
// holding the machine's stream does the work, and the row is how anyone watches. It
// differs in one way. A start is forwarded directly rather than claimed from an event,
// because a sign-in also takes a pasted code, which must reach the same replica
// without being written anywhere, so the forward has to exist anyway, and one
// mechanism is better than two. A forward also answers at once, so "that machine
// has gone" reads as a refusal rather than a spinner.
//
// CREDENTIALS STAY ON THE MACHINE. The app writes its own login there. The row holds
// a page's address and a device code, and a pasted code is passed on and forgotten:
// it is on no row and in no log line or reply. That the code is an authorization code
// bound to the verifier on the machine (PKCE) is why passing it through at all is safe.
//
// ONLY THE MACHINE'S OWNER. The row is read through modelSetupRow, which answers only
// for the caller's own machines, so a cluster owner gets the same refusal as a stranger.
// The holder checks again against the verified authority (HandleForwardedAppSignIn).

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/id"
)

const (
	appSignInResultConcept = "v1:worker:appSignInResult"
	// appSignInLifetime is how long a sign-in may take before Fleet reads it as over.
	// Both apps stop waiting at about 15 minutes, as Codex prints.
	appSignInLifetime = 16 * time.Minute
	// maxAppSignInCode bounds a pasted code. Real ones are a few hundred bytes.
	maxAppSignInCode = 4096
)

// The actions an AppSignInRelay carries.
const (
	AppSignInStart  = "start"
	AppSignInCode   = "code"
	AppSignInCancel = "cancel"
)

// AppSignInAction is one thing a person did to a sign-in, for the machine.
type AppSignInAction struct {
	Action         string
	RegistrationId string
	OwnerUserId    string
	SignInId       string
	App            string
	// Code is what the person pasted. It must reach no log and no row.
	Code string
}

// AppSignInRelay reaches the replica holding a machine's stream. A seam for the reason
// AppInference is one: the code that can reach a stream sits in a package that imports
// this one. An unset relay refuses.
type AppSignInRelay interface {
	RelayAppSignIn(ctx context.Context, holderNodeId string, action AppSignInAction) error
}

// SetAppSignInRelay installs the relay. Called once during cluster wiring.
func (e *MemQLEngine) SetAppSignInRelay(r AppSignInRelay) {
	if e == nil {
		return
	}
	e.appSignInRelay = r
}

// planAppSignIn decides whether this machine can take a sign-in to this app and which
// replica holds it. Pure, so every refusal a person can meet can be asserted without a
// fleet. Ownership is already settled: the row came through modelSetupRow.
func planAppSignIn(row map[string]any, app string) (holder string, err error) {
	if app != "claude-code" && app != "codex" {
		return "", modelPullRefusal{Code: "app_unknown", Message: "choose a supported app"}
	}
	label := appSignInMachineLabel(row)
	if mapString(row, "revokedAt") != "" {
		return "", modelPullRefusal{Code: "machine_revoked", Message: label + " was removed. Pair it again to sign in to its apps."}
	}
	features, _ := row["ownerFeatures"].(map[string]any)
	if features["apps"] == false {
		return "", modelPullRefusal{Code: "apps_disabled", Message: "Apps are turned off for " + label + " in Settings."}
	}
	entry := appRowEntry(row, app)
	if entry == nil {
		return "", modelPullRefusal{Code: "app_not_installed", Message: app + " is not installed on " + label + "."}
	}
	if allowed, _ := entry["allowed"].(bool); !allowed {
		return "", modelPullRefusal{Code: "app_not_allowed", Message: "Allow " + app + " on " + label + " for this cluster first. That permission belongs to the machine."}
	}
	descriptors, _ := row["appDescriptors"].([]any)
	canSignIn := false
	for _, raw := range descriptors {
		descriptor, _ := raw.(map[string]any)
		if mapString(descriptor, "id") == app {
			canSignIn, _ = descriptor["signIn"].(bool)
		}
	}
	if !canSignIn {
		return "", modelPullRefusal{Code: "sign_in_unsupported", Message: "Update Cockpit on " + label + " to sign in from here, or run the app's own sign-in on that machine."}
	}
	holder = strings.TrimSpace(mapString(row, "connectedNodeId"))
	if holder == "" {
		return "", modelPullRefusal{Code: "machine_offline", Message: label + " is not connected to the cluster right now. Wake it, or check its Cockpit is running, and try again."}
	}
	return holder, nil
}

func appSignInMachineLabel(row map[string]any) string {
	for _, key := range []string{"displayName", "name"} {
		if label := strings.TrimSpace(mapString(row, key)); label != "" {
			return label
		}
	}
	return "this machine"
}

func (e *MemQLEngine) evaluateFleetAppSignIn(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	registration := strings.TrimSpace(stringArg(args, "registrationId"))
	app := strings.TrimSpace(stringArg(args, "app"))
	row, err := e.modelSetupRow(ctx, registration)
	if err != nil {
		return nil, err
	}
	holder, err := planAppSignIn(row, app)
	if err != nil {
		return nil, err
	}
	if e.appSignInRelay == nil {
		return nil, modelPullRefusal{Code: "sign_in_unavailable", Message: "This node cannot reach a machine's stream, so sign-in is unavailable here."}
	}
	now := time.Now().UTC()
	signInId := id.NewShortId()
	create, err := parser.RenderCall("createAppSignIn", map[string]any{
		"signInId":     signInId,
		"workerId":     mapString(row, "id"),
		"app":          app,
		"targetNodeId": holder,
		"requestedAt":  now.Format(time.RFC3339Nano),
		"expiresAt":    now.Add(appSignInLifetime).Format(time.RFC3339Nano),
	})
	if err != nil {
		return nil, fmt.Errorf("fleetAppSignIn: render the record: %w", err)
	}
	if _, err := e.Execute(auth.ContextWithInternalOrigin(ctx), create); err != nil {
		return nil, fmt.Errorf("fleetAppSignIn: open the record: %w", err)
	}
	relayErr := e.appSignInRelay.RelayAppSignIn(ctx, holder, AppSignInAction{
		Action: AppSignInStart, RegistrationId: mapString(row, "id"), OwnerUserId: actingUserFromContext(ctx), SignInId: signInId, App: app,
	})
	if relayErr != nil {
		// The row must not sit at requested: a person would watch it until it expired.
		e.closeAppSignIn(ctx, signInId, "failed", relayErr.Error())
		return nil, modelPullRefusal{Code: "sign_in_not_delivered", Message: relayErr.Error()}
	}
	return singleVirtualRow(appSignInResultConcept, signInId, map[string]any{"signInId": signInId, "workerId": mapString(row, "id"), "app": app, "status": "requested"})
}

// appSignInForCaller reads one of the caller's sign-ins, fresh: it is answered right
// after the machine moved it, possibly on another replica.
func (e *MemQLEngine) appSignInForCaller(ctx context.Context, signInId string) (map[string]any, error) {
	notYours := modelPullRefusal{Code: "sign_in_not_found", Message: "no sign-in of yours has that id"}
	if strings.TrimSpace(signInId) == "" || strings.TrimSpace(actingUserFromContext(ctx)) == "" {
		return nil, notYours
	}
	call, err := parser.RenderCall("appSignInById", map[string]any{"signInId": signInId})
	if err != nil {
		return nil, notYours
	}
	res, err := e.Execute(ContextWithFreshRead(ctx), "query "+call)
	if err != nil {
		return nil, err
	}
	for _, row := range MaterializeRows(res) {
		if trimConceptPrefix(mapString(row, "id")) == trimConceptPrefix(signInId) {
			return row, nil
		}
	}
	return nil, notYours
}

// holderOfAppSignIn is the replica to tell, read from the machine's row rather than
// from the sign-in's, because it may have moved. If it has, the holder says that
// sign-in is gone, which is the truth: the process waiting on it went with the stream.
func (e *MemQLEngine) holderOfAppSignIn(ctx context.Context, signIn map[string]any) (string, map[string]any, error) {
	machine, err := e.modelSetupRow(ctx, mapString(signIn, "workerId"))
	if err != nil {
		return "", nil, err
	}
	holder := strings.TrimSpace(mapString(machine, "connectedNodeId"))
	if holder == "" {
		return "", nil, modelPullRefusal{Code: "machine_offline", Message: appSignInMachineLabel(machine) + " is not connected to the cluster right now."}
	}
	return holder, machine, nil
}

func (e *MemQLEngine) evaluateFleetAppSignInCode(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	signInId := strings.TrimSpace(stringArg(args, "signInId"))
	code := strings.TrimSpace(stringArg(args, "code"))
	// ONE LINE OF PRINTABLE TEXT. It goes to a process's stdin, and a newline would be
	// a second answer to a prompt nobody has shown yet.
	if code == "" || len(code) > maxAppSignInCode || strings.IndexFunc(code, unicode.IsControl) >= 0 {
		return nil, modelPullRefusal{Code: "sign_in_code_invalid", Message: "That is not a code from the sign-in page. Copy it again and paste only the code."}
	}
	signIn, err := e.appSignInForCaller(ctx, signInId)
	if err != nil {
		return nil, err
	}
	if mapString(signIn, "status") != "waiting" {
		return nil, modelPullRefusal{Code: "sign_in_not_waiting", Message: "That sign-in has ended. Sign in again."}
	}
	if needsCode, _ := signIn["needsCode"].(bool); !needsCode {
		return nil, modelPullRefusal{Code: "sign_in_takes_no_code", Message: "This sign-in finishes on its own page and takes no code here."}
	}
	holder, machine, err := e.holderOfAppSignIn(ctx, signIn)
	if err != nil {
		return nil, err
	}
	if e.appSignInRelay == nil {
		return nil, modelPullRefusal{Code: "sign_in_unavailable", Message: "This node cannot reach a machine's stream, so sign-in is unavailable here."}
	}
	err = e.appSignInRelay.RelayAppSignIn(ctx, holder, AppSignInAction{
		Action: AppSignInCode, RegistrationId: mapString(machine, "id"), OwnerUserId: actingUserFromContext(ctx), SignInId: trimmedSignInId(signIn), Code: code,
	})
	if err != nil {
		return nil, modelPullRefusal{Code: "sign_in_not_delivered", Message: err.Error()}
	}
	return singleVirtualRow(appSignInResultConcept, signInId, map[string]any{"signInId": signInId, "status": "waiting"})
}

func (e *MemQLEngine) evaluateFleetAppSignInCancel(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	signInId := strings.TrimSpace(stringArg(args, "signInId"))
	signIn, err := e.appSignInForCaller(ctx, signInId)
	if err != nil {
		return nil, err
	}
	switch mapString(signIn, "status") {
	case "requested", "waiting":
	default:
		return singleVirtualRow(appSignInResultConcept, signInId, map[string]any{"signInId": signInId, "status": mapString(signIn, "status")})
	}
	// BEST EFFORT TOWARD THE MACHINE, AND THE ROW CLOSES EITHER WAY. A person who pressed
	// Cancel is done whatever the machine can hear, and a process left waiting stops at
	// its own expiry.
	if holder, machine, holderErr := e.holderOfAppSignIn(ctx, signIn); holderErr == nil && e.appSignInRelay != nil {
		_ = e.appSignInRelay.RelayAppSignIn(ctx, holder, AppSignInAction{
			Action: AppSignInCancel, RegistrationId: mapString(machine, "id"), OwnerUserId: actingUserFromContext(ctx), SignInId: trimmedSignInId(signIn),
		})
	}
	e.closeAppSignIn(ctx, signInId, "cancelled", "")
	return singleVirtualRow(appSignInResultConcept, signInId, map[string]any{"signInId": signInId, "status": "cancelled"})
}

// trimmedSignInId is the id the machine was told: the bare one, as this file minted it.
func trimmedSignInId(signIn map[string]any) string {
	return trimConceptPrefix(mapString(signIn, "id"))
}

// closeAppSignIn writes a terminal status from this side.
func (e *MemQLEngine) closeAppSignIn(ctx context.Context, signInId, status, message string) {
	at := time.Now().UTC().Format(time.RFC3339Nano)
	call, err := parser.RenderCall("updateAppSignIn", map[string]any{"signInId": signInId, "status": status, "errorMessage": message, "updatedAt": at, "endedAt": at})
	if err != nil {
		return
	}
	_, _ = e.Execute(auth.ContextWithInternalOrigin(ctx), call)
}
