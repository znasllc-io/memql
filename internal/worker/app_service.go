package worker

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// ===========================================================================
// MemQL.app REGISTERS ITS OWN SERVICES (memql-cockpit#545)
// ===========================================================================
// The worker and the menu used to run from per-user plists in
// ~/Library/LaunchAgents. macOS ties such a plist to its app only when both
// carry the same Team ID, and an ad-hoc signed build carries none, so Login
// Items listed two bare executables -- "MemQL" and "MemQLCockpit" -- with
// the generic exec icon. An agent registered through SMAppService from a
// plist inside the bundle is recorded under the app that registered it,
// signature or not: one MemQL, with its icon.
//
// So both plists live in MemQL.app/Contents/Library/LaunchAgents, and this
// executable -- the bundle's main one, which SMAppService resolves them
// against -- registers them: `memql worker service register|unregister|
// status`. The headless install has no bundle and keeps its plists.
//
// A plist inside the signed bundle is the same for every account, so it
// cannot carry what the per-user plist did: this account's HOME, the log
// under it, the Ollama variables the installer passes. The embedded worker
// plist sets only MEMQL_SERVICE=app, and prepareAppService fills the rest
// in before anything reads them.

const (
	workerAgentPlist = "com.znasllc.memql-worker.plist"
	menuAgentPlist   = "com.znasllc.memql-cockpit-menubar.plist"

	appServiceVariable = "MEMQL_SERVICE"
	// serviceEnvFile, under the state dir, is this account's half of the
	// worker's service definition. activate-app.sh writes it.
	serviceEnvFile = "service.env"
)

// serviceEnvNames are the only variables service.env may set: the
// installing shell's Ollama variables (memql-cockpit#524) and a PATH the
// owner chooses, which ensureServicePath then extends as for any service.
var serviceEnvNames = map[string]bool{"PATH": true, "OLLAMA_HOST": true, "OLLAMA_MODELS": true}

// prepareAppService runs first in `worker run`. A worker anything other
// than MemQL.app's agent started is left exactly as it is.
func prepareAppService() {
	if os.Getenv(appServiceVariable) != "app" {
		return
	}
	if os.Getenv("HOME") == "" {
		if u, err := user.Current(); err == nil && u.HomeDir != "" {
			_ = os.Setenv("HOME", u.HomeDir)
		}
	}
	state := defaultStateDir()
	for name, value := range readServiceEnv(filepath.Join(state, serviceEnvFile)) {
		_ = os.Setenv(name, value)
	}
	// Where the per-user plist's StandardOutPath sent it, and where the
	// menu's log window reads (control_logs.go).
	redirectServiceOutput(filepath.Join(state, "worker.log"))
}

// readServiceEnv reads NAME=value lines from a regular file this account
// owns. Blank lines, # comments and names outside serviceEnvNames are
// skipped; a missing or foreign file sets nothing.
func readServiceEnv(path string) map[string]string {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || !ownedByCurrentUser(info) {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		name, value, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") || !serviceEnvNames[name] {
			continue
		}
		values[name] = value
	}
	return values
}

// The words `worker service` prints for SMAppService's states.
var appServiceStates = map[int]string{0: "not_registered", 1: "enabled", 2: "requires_approval", 3: "not_found"}

func appServiceState(plist string) string {
	if word, ok := appServiceStates[appServiceStatus(plist)]; ok {
		return word
	}
	return "unavailable"
}

// ensureAppService always asks: the status alone cannot say whether launchd
// has the job. macOS answers "enabled" for a label it remembers from the old
// per-user plist, before anything here was registered, and the agent never
// started. A change that is already true comes back as success.
func ensureAppService(plist string, on bool) error {
	if err := changeAppService(plist, on); err != nil {
		return err
	}
	if on && appServiceState(plist) == "requires_approval" {
		return errors.New("macOS is waiting for you to allow MemQL in System Settings > General > Login Items")
	}
	return nil
}

func handleService(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: memql worker service register [--menu=on|off] | unregister [--worker-only] | status")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("worker service "+args[0], flag.ExitOnError)
	menu := fs.String("menu", "", "register: on starts the menu with MemQL, off removes it; empty leaves it as it is")
	workerOnly := fs.Bool("worker-only", false, "unregister: stop only the worker and keep the menu")
	_ = fs.Parse(args[1:])
	if !appServicesAvailable() {
		fmt.Fprintln(os.Stderr, "ERROR: only MemQL.app registers macOS services; run Contents/MacOS/MemQL inside the installed app")
		os.Exit(3)
	}
	var err error
	switch args[0] {
	case "register":
		if *menu != "" && *menu != "on" && *menu != "off" {
			fmt.Fprintln(os.Stderr, "ERROR: --menu takes on or off")
			os.Exit(2)
		}
		err = ensureAppService(workerAgentPlist, true)
		if err == nil && *menu != "" {
			err = ensureAppService(menuAgentPlist, *menu == "on")
		}
	case "unregister":
		if !*workerOnly {
			err = ensureAppService(menuAgentPlist, false)
		}
		if err == nil {
			err = ensureAppService(workerAgentPlist, false)
		}
	case "status":
	default:
		fmt.Fprintf(os.Stderr, "ERROR: unknown service action %q (register, unregister or status)\n", args[0])
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	out, _ := json.Marshal(map[string]string{"worker": appServiceState(workerAgentPlist), "menu": appServiceState(menuAgentPlist)})
	fmt.Println(string(out))
}
