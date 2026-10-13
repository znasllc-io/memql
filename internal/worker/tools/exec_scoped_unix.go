//go:build darwin || linux

package tools

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func execScopedCommandChild(args []string) error {
	directory := os.NewFile(3, "approved-command-directory")
	defer directory.Close()
	info, err := directory.Stat()
	if err != nil || !info.IsDir() {
		return fmt.Errorf("scoped command requires an inherited directory")
	}
	if err := directory.Chdir(); err != nil {
		return fmt.Errorf("enter approved command directory: %w", err)
	}
	if err := directory.Close(); err != nil {
		return err
	}
	// Do not retain a stale PWD from the worker's own launch directory.
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := os.Setenv("PWD", cwd); err != nil {
		return err
	}
	return unix.Exec("/bin/sh", args, os.Environ())
}
