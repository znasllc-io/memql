// `memql worker consent ...` -- operator-facing controls
// for the consent gate (memql-cockpit#64). Talks to a running
// worker daemon over the Unix socket the worker opens at startup.
//
// Subcommands:
//
//	consent grant --window=<duration> [--strict] [--cluster <id>]   Open a window for the requested duration
//	consent revoke [--cluster <id>]                                  Close windows IMMEDIATELY (all, or one)
//	consent status [--cluster <id>]                                  Show current state
//	consent watch [--cluster <id>]                                   Live tail of dispatch / grant / revoke events
//
// The duration is anything time.ParseDuration accepts: 5m, 1h, 30s.
//
// CONSENT IS PER CLUSTER (memql-cockpit#433). A machine paired with two
// clusters holds a window for each, and --cluster names which. It may be
// left off wherever it cannot be ambiguous: a grant on a machine that
// serves one cluster, and every status. A revoke without it closes EVERY
// window -- the kill switch never asks which.

package worker

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/consent"
)

func handleConsentCmd(args []string) {
	if len(args) == 0 {
		printConsentUsage()
		os.Exit(1)
	}
	switch args[0] {
	case "grant":
		handleConsentGrant(args[1:])
	case "revoke":
		handleConsentRevoke(args[1:])
	case "status":
		handleConsentStatus(args[1:])
	case "watch":
		handleConsentWatch(args[1:])
	case "-h", "--help", "help":
		printConsentUsage()
	default:
		fmt.Fprintf(os.Stderr, "ERROR: unknown consent subcommand %q\n\n", args[0])
		printConsentUsage()
		os.Exit(1)
	}
}

func printConsentUsage() {
	fmt.Println("Usage: memql worker consent <subcommand> [--cluster <id>]")
	fmt.Println("")
	fmt.Println("Subcommands:")
	fmt.Println("  grant --window=<duration> [--strict]   Open a consent window (e.g. --window=1h)")
	fmt.Println("  revoke                                  Close windows IMMEDIATELY (every cluster's,")
	fmt.Println("                                          or only --cluster's)")
	fmt.Println("  status                                  Show the consent state of every cluster")
	fmt.Println("  watch                                   Live tail of grant / revoke / dispatch events")
	fmt.Println("")
	fmt.Println("Consent is per cluster. On a machine paired with more than one, --cluster <id>")
	fmt.Println("names the one a grant is for (the ids are in `memql worker config`).")
	fmt.Println("")
	fmt.Println("Environment:")
	fmt.Println("  MEMQL_WORKER_CONSENT_SOCKET   Override the socket path (default: ~/.memql/worker.sock)")
}

func consentClient(home string) *consent.Client {
	return &consent.Client{
		Path:    consent.DefaultSocketPath(),
		Timeout: 2 * time.Second,
		Home:    strings.TrimSpace(home),
	}
}

func handleConsentGrant(args []string) {
	fs := flag.NewFlagSet("worker consent grant", flag.ExitOnError)
	window := fs.Duration("window", 0, "consent window duration (e.g. 5m, 1h, 30s); REQUIRED")
	strict := fs.Bool("strict", false, "require per-action approval for high-risk calls (v1: enforcement is a follow-up under #64)")
	cluster := fs.String("cluster", "", "the cluster (home id) this window is for; required when this machine serves more than one")
	_ = fs.Parse(args)

	if *window <= 0 {
		fmt.Fprintln(os.Stderr, "ERROR: --window is required and must be > 0 (e.g. --window=1h)")
		os.Exit(1)
	}
	if *window > 24*time.Hour {
		fmt.Fprintf(os.Stderr, "WARNING: long window (%s) -- consider a shorter duration for security.\n", *window)
	}

	// The CLI grant path never sets a strict-mode region -- the
	// in-region exemption (memql-cockpit#131) is configured through
	// the TUI Workers-pane region picker, which needs the visual
	// box-draw surface to be usable. CLI callers get plain strict
	// mode (every high-risk call gated).
	resp, err := consentClient(*cluster).Grant(*window, *strict, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		fmt.Fprintln(os.Stderr, "Hint: is the worker running? Start it with `memql worker run`.")
		os.Exit(1)
	}
	if !resp.OK {
		fmt.Fprint(os.Stderr, consentRefusal(resp, *window, *strict))
		os.Exit(1)
	}
	fmt.Printf("Consent granted%s. Window: %s. Expires at: %s.\n",
		forHome(resp.Home), *window, resp.Status.ExpiresAt.Local().Format(time.RFC3339))
	fmt.Println("Approved directory reads are resumed until you stop access or revoke the saved permission in Nexus.")
	if *strict {
		fmt.Println("Strict mode: ON (per-action approval ships under #64 follow-up).")
	}
}

// consentRefusal phrases a refused request. A grant that needed a cluster
// gets the commands that would have worked, one per cluster, with the
// window the person typed -- the worker cannot write that sentence, since
// only this side knows the window.
func consentRefusal(resp consent.Response, window time.Duration, strict bool) string {
	var b strings.Builder
	switch resp.Code {
	case consent.CodeHomeRequired:
		fmt.Fprintf(&b, "This machine serves %d clusters: %s.\n", len(resp.Homes), strings.Join(homeIDs(resp.Homes), ", "))
		fmt.Fprintln(&b, "Consent is per cluster -- say which one this window is for:")
		fmt.Fprintln(&b)
		flags := ""
		if strict {
			flags = " --strict"
		}
		for _, h := range resp.Homes {
			fmt.Fprintf(&b, "  memql worker consent grant --window=%s%s --cluster %s\n", shortDuration(window), flags, h.Home)
		}
	case consent.CodeUnknownHome:
		fmt.Fprintf(&b, "ERROR: no cluster by that id on this machine. It serves: %s.\n", strings.Join(homeIDs(resp.Homes), ", "))
	case consent.CodeNoHomes:
		fmt.Fprintln(&b, "ERROR: the worker is not serving any cluster right now, so there is no window to open.")
	default:
		fmt.Fprintf(&b, "ERROR: %s\n", resp.Error)
	}
	return b.String()
}

func handleConsentRevoke(args []string) {
	fs := flag.NewFlagSet("worker consent revoke", flag.ExitOnError)
	cluster := fs.String("cluster", "", "close only this cluster's window (default: every cluster's)")
	_ = fs.Parse(args)

	resp, err := consentClient(*cluster).Revoke()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	if !resp.OK {
		fmt.Fprint(os.Stderr, consentRefusal(resp, 0, false))
		os.Exit(1)
	}
	fmt.Print(revokeSentence(resp, time.Now()))
}

// revokeSentence says which windows closed -- and, after a revoke of one
// cluster, which others are still open, because "revoked" without a name
// reads as "this machine is locked" when it may not be.
func revokeSentence(resp consent.Response, now time.Time) string {
	var b strings.Builder
	if resp.Home == "" {
		switch len(resp.Homes) {
		case 0, 1:
			fmt.Fprintln(&b, "Consent revoked. Future worker tool calls will be denied until you grant a new window.")
		default:
			fmt.Fprintf(&b, "Consent revoked on every cluster (%s). Future worker tool calls will be denied until you grant a new window.\n",
				strings.Join(homeIDs(resp.Homes), ", "))
		}
		return b.String()
	}
	fmt.Fprintf(&b, "Consent revoked for %s. Its tool calls will be denied until you grant a new window.\n", resp.Home)
	for _, h := range resp.Homes {
		if h.Home != resp.Home && h.Status.Granted {
			fmt.Fprintf(&b, "%s still has a window open (%s left); memql worker consent revoke closes every window.\n",
				h.Home, shortDuration(h.Status.ExpiresAt.Sub(now).Round(time.Second)))
		}
	}
	return b.String()
}

func handleConsentStatus(args []string) {
	fs := flag.NewFlagSet("worker consent status", flag.ExitOnError)
	cluster := fs.String("cluster", "", "show only this cluster (default: every cluster)")
	_ = fs.Parse(args)

	resp, err := consentClient(*cluster).Status()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		fmt.Fprintln(os.Stderr, "Hint: is the worker running? Start it with `memql worker run`.")
		os.Exit(1)
	}
	if !resp.OK {
		fmt.Fprint(os.Stderr, consentRefusal(resp, 0, false))
		os.Exit(1)
	}
	fmt.Print(renderConsentStatus(resp, time.Now()))
}

// renderConsentStatus is `consent status`: one cluster in the familiar
// block, several in a table. A pure function of the response and the
// clock, so its words are asserted rather than eyeballed.
func renderConsentStatus(resp consent.Response, now time.Time) string {
	var b strings.Builder
	if resp.Home != "" || len(resp.Homes) <= 1 {
		st, home := resp.Status, resp.Home
		if home == "" && len(resp.Homes) == 1 {
			st, home = resp.Homes[0].Status, resp.Homes[0].Home
		}
		multi := len(resp.Homes) > 1
		fmt.Fprintf(&b, "Approved directory reads%s: %s\n", forHome(home), directoryReadsStatus(st))
		if !st.Granted {
			fmt.Fprintf(&b, "Consent%s: NOT GRANTED\n", forHome(home))
			cmd := "memql worker consent grant --window=<duration>"
			if multi {
				cmd += " --cluster " + home
			}
			fmt.Fprintf(&b, "Run `%s` to open a window.\n", cmd)
			return b.String()
		}
		fmt.Fprintf(&b, "Consent%s: GRANTED\n", forHome(home))
		fmt.Fprintf(&b, "  Expires at:   %s\n", st.ExpiresAt.Local().Format(time.RFC3339))
		fmt.Fprintf(&b, "  Remaining:    %s\n", st.ExpiresAt.Sub(now).Round(time.Second))
		if st.Window > 0 {
			fmt.Fprintf(&b, "  Window:       %s\n", st.Window)
		}
		if st.Strict {
			fmt.Fprintln(&b, "  Strict mode:  ON")
		}
		return b.String()
	}

	fmt.Fprintf(&b, "Consent is per cluster. This machine serves %d:\n\n", len(resp.Homes))
	var table strings.Builder
	tw := tabwriter.NewWriter(&table, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "  CLUSTER\tCONSENT\tEXPIRES\tREMAINING\tSTRICT")
	for _, h := range resp.Homes {
		if !h.Status.Granted {
			fmt.Fprintf(tw, "  %s\tnot granted\t\t\t\n", h.Home)
			continue
		}
		strict := "off"
		if h.Status.Strict {
			strict = "ON"
		}
		fmt.Fprintf(tw, "  %s\tGRANTED\t%s\t%s\t%s\n", h.Home,
			h.Status.ExpiresAt.Local().Format("15:04:05"),
			shortDuration(h.Status.ExpiresAt.Sub(now).Round(time.Second)), strict)
	}
	_ = tw.Flush()
	// tabwriter pads the last column too; a line that ends in spaces is
	// noise to anyone who copies it.
	for _, line := range strings.Split(strings.TrimRight(table.String(), "\n"), "\n") {
		fmt.Fprintln(&b, strings.TrimRight(line, " "))
	}
	fmt.Fprintln(&b)
	for _, h := range resp.Homes {
		fmt.Fprintf(&b, "Approved directory reads for %s: %s\n", h.Home, directoryReadsStatus(h.Status))
	}
	fmt.Fprintln(&b, "Open a window:  memql worker consent grant --window=<duration> --cluster <cluster>")
	fmt.Fprintln(&b, "Close them all: memql worker consent revoke")
	return b.String()
}

func directoryReadsStatus(st consent.Status) string {
	if st.DirectoryReadsPaused {
		return "paused on this machine"
	}
	return "available with a matching Nexus approval"
}

func handleConsentWatch(args []string) {
	fs := flag.NewFlagSet("worker consent watch", flag.ExitOnError)
	cluster := fs.String("cluster", "", "watch only this cluster (default: every cluster)")
	_ = fs.Parse(args)

	fmt.Fprintln(os.Stderr, "Watching consent events (Ctrl+C to exit)...")
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()
	err := consentClient(*cluster).Watch(ctx, func(line []byte) {
		// First message is the initial status response; the rest
		// are events. We pretty-print both shapes here without
		// over-modeling -- the operator can use jq if they want
		// structure.
		var probe map[string]any
		if jerr := json.Unmarshal(line, &probe); jerr != nil {
			fmt.Println(string(line))
			return
		}
		if k, ok := probe["kind"].(string); ok && k != "" {
			fmt.Printf("[%s]", time.Now().Format("15:04:05"))
			if home, _ := probe["home"].(string); home != "" {
				fmt.Printf(" %s:", home)
			}
			fmt.Printf(" %s", k)
			if tool, _ := probe["tool"].(string); tool != "" {
				fmt.Printf(" %s.%v", tool, probe["action"])
			}
			if class, _ := probe["class"].(string); class != "" {
				fmt.Printf(" class=%s", class)
			}
			if allowed, ok := probe["allowed"].(bool); ok {
				outcome := "DENIED"
				if allowed {
					outcome = "ALLOWED"
				}
				fmt.Printf(" %s", outcome)
			}
			if reason, _ := probe["reason"].(string); reason != "" {
				fmt.Printf(" -- %s", reason)
			}
			fmt.Println()
			return
		}
		// Initial Response shape: the state every event is a change to.
		var resp consent.Response
		if json.Unmarshal(line, &resp) == nil && resp.OK {
			fmt.Print(renderConsentStatus(resp, time.Now()))
			fmt.Println()
			return
		}
		out, _ := json.MarshalIndent(probe, "", "  ")
		fmt.Println(string(out))
	})
	if err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
}

// shortDuration renders a duration the way a person types one: 1h, 30m,
// 1h30m, 45s -- not time.Duration's 1h0m0s.
func shortDuration(d time.Duration) string {
	out := d.String()
	if strings.HasSuffix(out, "m0s") {
		out = strings.TrimSuffix(out, "0s")
	}
	if strings.HasSuffix(out, "h0m") {
		out = strings.TrimSuffix(out, "0m")
	}
	return out
}

// forHome is " for <home>" when there is a home to name.
func forHome(home string) string {
	if home == "" {
		return ""
	}
	return " for " + home
}

func homeIDs(homes []consent.HomeStatus) []string {
	out := make([]string, 0, len(homes))
	for _, h := range homes {
		out = append(out, h.Home)
	}
	return out
}
