//go:build !darwin

package worker

// InstallLaunchAgent on non-darwin builds falls back to user-
// systemd. The install-linux.sh script ships a systemd unit
// template; the wizard delegates the install there for now and
// returns ErrNoLaunchAgent so the wizard surfaces a clear "use
// scripts/install/install-linux.sh" instruction instead of
// pretending it installed something.
//
// Future improvement: write the systemd unit directly here so
// the wizard handles the install on Linux too. For now scripts/
// install owns it.
func InstallLaunchAgent(_ string) error {
	return ErrNoLaunchAgent
}

func UninstallLaunchAgent() error {
	return ErrNoLaunchAgent
}
