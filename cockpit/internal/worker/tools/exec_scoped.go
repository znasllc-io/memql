package tools

import "fmt"

const scopedCommandChildFlag = "--memql-internal-scoped-exec"

// RunScopedCommandChild handles the private child-process entry point before
// normal CLI initialization. The parent has already checked consent and policy;
// this child only enters inherited directory fd 3 and execs the prepared shell.
// It has no additional privileges and cannot open an engine-supplied pathname.
// A successful invocation replaces the process and never returns.
func RunScopedCommandChild(args []string) (bool, error) {
	if len(args) == 0 || args[0] != scopedCommandChildFlag {
		return false, nil
	}
	if len(args) < 4 || args[1] != "/bin/sh" || args[2] != "-c" {
		return true, fmt.Errorf("invalid scoped command child invocation")
	}
	return true, execScopedCommandChild(args[1:])
}
