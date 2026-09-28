//go:build agent || planner

package worker

// app_gate_refusal.go -- the app gate's named refusals, in the file both the
// agent build and the planner build compile.
//
// The GATE runs on the agent (app_gate.go): it reads the owner's kill switch,
// and it is asked on the replica that would open the session. The REFUSAL
// crosses the mesh: a planner whose app-door call was forwarded to the agent
// holding the machine gets the gate's answer back over AppCallForward and
// rebuilds it here, so a refused call reads the same -- same code, same type
// under errors.As -- whichever node the call started on.

// AppGateKillSwitchEngaged is the refusal code when the owner's computer-use
// kill switch is engaged. The same code the tool path uses, because it is the
// same switch.
const AppGateKillSwitchEngaged = "kill_switch_engaged"

// AppGateKillSwitchUnreadable is the refusal code when the owner's kill switch
// could not be read.
//
// SEPARATE FROM AppGateKillSwitchEngaged on purpose. A read that failed says
// nothing about what the owner did: admitting on it would open a session on a
// machine they may have switched off, and reporting it as the switch would
// tell them they switched computer use off when they did not.
const AppGateKillSwitchUnreadable = "kill_switch_unreadable"

// AppGateNotNamedByOwner is the refusal code for an app door reached through
// an explicit pin the session's owner did not make.
const AppGateNotNamedByOwner = "app_not_named_by_owner"

// appGateRefusal is a named refusal from the app gate. Both doors wrap it, so
// errors.As finds it under whatever a door adds in front.
type appGateRefusal struct {
	code    string
	message string
}

func (r *appGateRefusal) Error() string { return r.code + ": " + r.message }

// Code is the stable machine-readable tag, in the shape the engine's other
// refusals expose it (memqlengine.AppVisionStagingFailed.Code).
func (r *appGateRefusal) Code() string { return r.code }

// isAppGateCode reports whether a refusal code is the app gate's. A gate
// refusal is about the OWNER -- their pin, their kill switch -- and not about
// one machine, so a forwarded call that met one stops there rather than
// trying the owner's next machine, which the same gate would refuse.
func isAppGateCode(code string) bool {
	switch code {
	case AppGateKillSwitchEngaged, AppGateKillSwitchUnreadable, AppGateNotNamedByOwner:
		return true
	}
	return false
}
