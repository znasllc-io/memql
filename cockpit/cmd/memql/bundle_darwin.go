//go:build darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
)

// Finder launches the bundle's main executable without CLI arguments. Open
// its embedded menu helper; the memql CLI alias still prints ordinary help.
func openBundledApplication() bool {
	if len(os.Args) != 1 || filepath.Base(os.Args[0]) != "MemQL" {
		return false
	}
	executable, err := os.Executable()
	if err != nil {
		return false
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return false
	}
	contents := filepath.Dir(filepath.Dir(executable))
	helper := filepath.Join(contents, "Library", "LoginItems", "MemQL Menu.app")
	if _, err = os.Stat(filepath.Join(helper, "Contents", "MacOS", "MemQLCockpit")); err != nil {
		return false
	}
	if err = exec.Command("/usr/bin/open", helper).Run(); err != nil {
		return false
	}
	return true
}
