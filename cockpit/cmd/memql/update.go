package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/znasllc-io/memql/cockpit/internal/selfupdate"
)

func handleUpdate(args []string) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	check := fs.Bool("check", false, "Check for a newer stable release")
	install := fs.Bool("install", false, "Install the checked release and restart the worker")
	status := fs.Bool("status", false, "Read the latest update progress")
	bootstrap := fs.Bool("bootstrap", false, "Upgrade an older installation using a verified release helper")
	installed := fs.String("installed-executable", "", "Exact existing managed worker executable")
	expected := fs.String("installed-version", "", "Exact version currently installed")
	confirmation := fs.String("confirm", "", "restart-worker authorizes a legacy worker restart after its work finishes")
	target := fs.String("version", "", "Exact stable version shown by the update check")
	asJSON := fs.Bool("json", false, "Print a machine-readable result")
	err := fs.Parse(args)
	if err == flag.ErrHelp {
		fmt.Println("Usage: memql update --check | --status | --install --version=X.Y.Z [--json]\nOne-time upgrade: update --bootstrap --installed-executable=/absolute/path --installed-version=X.Y.Z --version=X.Y.Z [--confirm=restart-worker] [--json]\nUpdates preserve saved connections, worker identity and policy. macOS may require permission approval again.")
		return 0
	}
	count := 0
	for _, set := range []bool{*check, *install, *status, *bootstrap} {
		if set {
			count++
		}
	}
	if err != nil || count != 1 || fs.NArg() != 0 || !*install && !*bootstrap && *target != "" || (*install || *bootstrap) && *target == "" ||
		*bootstrap && (*installed == "" || *expected == "") || !*bootstrap && (*installed != "" || *expected != "" || *confirmation != "") {
		result := selfupdate.Result{Error: "Choose --check, --status, or --install --version=X.Y.Z", Phase: "error", CurrentVersion: version, Variant: buildVariant}
		// A malformed option must still return the JSON contract when --json was
		// requested, including flags after the parser's first error.
		for _, arg := range args {
			if arg == "--json" || arg == "--json=true" {
				*asJSON = true
			}
		}
		printUpdateResult(result, *asJSON)
		return 2
	}
	executable, err := os.Executable()
	var client *selfupdate.Client
	if err == nil {
		if *bootstrap {
			client, err = selfupdate.NewBootstrap(version, buildVariant, executable, *installed, *expected, *confirmation)
		} else {
			client, err = selfupdate.New(version, buildVariant, executable)
		}
	}
	var result selfupdate.Result
	if err != nil {
		result = selfupdate.Result{Error: err.Error(), Phase: "error", CurrentVersion: version, Variant: buildVariant}
	} else {
		switch {
		case *check:
			result = client.Check(context.Background())
		case *status:
			result = client.Status()
		case *install, *bootstrap:
			result = client.Install(context.Background(), *target)
		}
	}
	printUpdateResult(result, *asJSON)
	if !result.OK {
		return 1
	}
	return 0
}
func printUpdateResult(result selfupdate.Result, asJSON bool) {
	if asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(result)
		return
	}
	if result.Error != "" {
		fmt.Fprintln(os.Stderr, result.Error)
	} else if result.Message != "" {
		fmt.Println(result.Message)
	} else {
		fmt.Println("No Cockpit update is running")
	}
}
