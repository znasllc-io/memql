package worker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/inference"
	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// inference_cmd.go is the ONE surface this feature gives a person:
// `memql worker setup --inference`, plus the --pull / --allow half of
// `memql worker models`.
//
// EVERY PHASE PRINTS ITS HEADING BEFORE THE WORK, never after. Somebody
// who interrupts a four-gigabyte download at second 40 has to be able to
// read off the screen what they interrupted, and a spinner that turns out
// to have installed something is the exact failure this shape exists to
// prevent. The failure the whole file is written against is narrower than
// that: a multi-gigabyte download that looks identical to a hang,
// followed by a machine that still does not appear in the fleet, with
// nothing on screen saying which of the five possible reasons it was.
//
// THE SENTENCES ARE THE PRODUCT, the same way they are in
// internal/worker/inference. A refusal is read by somebody in a terminal
// at the moment they are blocked and it is the only thing they get, so
// each one states the true thing first and then the command that fixes
// it, and the tests assert them. Rewording one is meant to be a decision
// in a diff a reviewer can read, not a tidy-up.
//
// NOTHING HERE RUNS SUDO. Where a fix needs it, the command is printed
// for the PERSON to run and is LABELLED as such -- a line the cockpit
// prints and a line the cockpit runs look identical on screen, and that
// distinction only survives if the copy carries it.
//
// The refusals this file composes are printed to stdout in full, so the
// process exits through alreadySaid rather than through a message the
// dispatcher would print a second time under them.

const (
	// inferenceWrapWidth is where prose paragraphs wrap. Narrow enough
	// to survive an 80-column terminal over SSH once the two-space
	// indent is added, which is where these sentences are actually read.
	inferenceWrapWidth = 76

	// pullBarCells is the width of the progress bar's track. Fixed, so
	// the bar does not reflow when a terminal is resized mid-pull --
	// a redraw at a new width leaves the tail of the old line on screen.
	pullBarCells = 40

	// runtimeReadyWithin is how long a freshly started runtime gets to
	// answer before the setup calls it a failure. An install command
	// exiting zero says a service was asked to start, not that anything
	// listens, and the very next step pulls against that socket. Thirty
	// seconds covers a first `ollama serve` discovering its GPU on a
	// slow disk; a runtime silent past that is not coming.
	runtimeReadyWithin = 30 * time.Second
)

// repeatedFlag collects a flag the operator may give more than once
// (--model, --pull, --allow).
//
// The order typed is the order kept, because it is the order the ids are
// pulled in and the order they are read back in the closing block. A set
// would sort them into an order nobody typed, and an operator comparing
// the output against their own command line would have to hunt.
type repeatedFlag []string

func (r *repeatedFlag) String() string { return strings.Join(*r, ",") }

func (r *repeatedFlag) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return errors.New("empty value")
	}
	*r = append(*r, v)
	return nil
}

// alreadySaid is an exit code for a failure whose whole message is
// already on stdout, composed in the shape this file's brief specifies.
//
// The dispatcher's generic "ERROR: %v" would print a lower-case fragment
// underneath a refusal that was written to be read on its own, in
// capitals the copy rules forbid. The empty Msg is the signal, and it is
// deliberate rather than an oversight: SetupExitCode reads the Code, and
// the caller prints nothing for an empty message.
func alreadySaid(code int) error { return &SetupError{Code: code, Msg: ""} }

// -----------------------------------------------------------------------------
// `memql worker setup --inference`
// -----------------------------------------------------------------------------

// inferenceSetup is one run of the command. Every external effect is a
// seam so the whole flow -- every sentence, every exit code -- is
// exercisable on a CI runner with no Ollama, no Docker, no GPU and no
// policy.yaml. A flow that can only be driven by hand on a Mac Studio is
// one nobody drives, and its refusals are the half that never gets read
// until somebody is blocked by one.
type inferenceSetup struct {
	out io.Writer
	in  io.Reader
	// tty says stdout is a terminal, and it decides the progress
	// display alone -- see pullDisplay.
	tty bool

	modelIDs       []string
	nonInteractive bool
	runtimeFlag    string
	policyPath     string

	gather  func(ctx context.Context) (inference.Host, error)
	base    func() string
	install func(ctx context.Context, p inference.Plan, consent func([]string) bool, run inference.Runner, stage inference.Stager) error
	// stage fetches and unpacks a native Linux runtime; ready waits for
	// whatever was just started to answer. Both are seams so the flow
	// runs on a CI runner with no network and nothing listening.
	stage       inference.Stager
	ready       func(ctx context.Context, base string) error
	pull        func(ctx context.Context, base, model string, onProgress func(inference.Progress)) error
	allow       func(policyPath string, ids ...string) error
	readvertise func(ctx context.Context) error
	inventory   func(ctx context.Context) models.Inventory
	pullAllowed func() bool
}

// newInferenceSetup wires the real machine in. The policy is read HERE
// and again after the allow-list is written, because the closing block
// reports what the cluster will see and that answer depends on the file
// this command just changed.
func newInferenceSetup(configPath string, ids []string, nonInteractive bool, runtimeFlag string) *inferenceSetup {
	policyPath := filepath.Join(filepath.Dir(configPath), "policy.yaml")
	discoverer := newDiscoverer()
	// The closing block prints the `max` the cluster will see, and this runs
	// in the owner's shell: it reads the worker's environment, not its own.
	followWorkerEnvironment(discoverer, configPath)
	return &inferenceSetup{
		out:            os.Stdout,
		in:             os.Stdin,
		tty:            isInteractiveTTY(),
		modelIDs:       ids,
		nonInteractive: nonInteractive,
		runtimeFlag:    runtimeFlag,
		policyPath:     policyPath,
		gather:         func(ctx context.Context) (inference.Host, error) { return inference.Gather(ctx, discoverer) },
		// The base the DISCOVERER resolved, never a second reading of
		// OLLAMA_HOST: two resolvers drift, and the failure is a pull
		// that lands where discovery will never look, so the model
		// arrives and is never advertised.
		base:    discoverer.ResolvedOllamaBaseURL,
		install: inference.InstallRuntime,
		stage:   inference.StageRuntime,
		ready: func(ctx context.Context, base string) error {
			return inference.WaitReady(ctx, base, runtimeReadyWithin)
		},
		pull:        inference.Pull,
		allow:       inference.Allow,
		readvertise: inference.Readvertise,
		inventory:   func(ctx context.Context) models.Inventory { return probeWithPolicy(ctx, discoverer, policyPath) },
		pullAllowed: func() bool { return policyAt(policyPath).ModelsPullAllowed() },
	}
}

// policyAt reads policy.yaml, falling back to the defaults when it
// cannot. A policy that will not load is already the worker's problem
// and is reported by `memql worker models`; refusing the whole setup on
// it would block the command an operator runs to fix the machine.
func policyAt(policyPath string) *tools.Policy {
	policy, err := tools.LoadPolicy(policyPath)
	if err != nil {
		return tools.DefaultPolicy()
	}
	return policy
}

// probeWithPolicy re-reads policy.yaml and probes, which is what the
// worker itself does at the next SIGHUP. Reading the file rather than
// appending the new ids to the list in memory is what makes the closing
// block a report of the machine instead of a restatement of the command
// line -- an id that Allow refused to write is then visibly absent.
func probeWithPolicy(ctx context.Context, d *models.Discoverer, policyPath string) models.Inventory {
	policy := policyAt(policyPath)
	return d.Probe(ctx, modelsRequest(policy))
}

// runInferenceSetup runs the flow and returns the process exit code.
//
// It prints everything itself, including the failures, so the caller
// exits on the code alone. See alreadySaid.
func runInferenceSetup(ctx context.Context, s *inferenceSetup) int {
	err := s.run(ctx)
	if err != nil {
		if msg := err.Error(); msg != "" {
			// No "ERROR:" prefix: this surface's copy rules rule out
			// all-caps, and the message below is a sentence rather than
			// a log line.
			fmt.Fprintln(os.Stderr, msg)
		}
	}
	return SetupExitCode(err)
}

func (s *inferenceSetup) run(ctx context.Context) error {
	host, err := s.gather(ctx)
	if err != nil {
		return setupFailed("this machine could not be inspected: %v", err)
	}
	// The preference goes INTO the host before the decision, because on
	// Linux it chooses between two plans rather than vetoing one: the
	// native runtime by default, the container under --runtime docker.
	// The disk figure follows it -- the two runtimes keep models on
	// different volumes, and the pull path refuses on the wrong one
	// otherwise.
	pref, err := s.runtimePreference()
	if err != nil {
		return err
	}
	host.Runtime = pref
	if pref == inference.RuntimeDocker && host.DockerFreeDisk != 0 {
		host.FreeDisk = host.DockerFreeDisk
	}
	plan := inference.Decide(host)
	plan, err = s.applyRuntimeFlag(host, plan)
	if err != nil {
		return err
	}

	// The default pair comes from the PLAN, not from a list in this
	// file: inference.Decide populates DefaultModels on every plan
	// including a refusal, and a second copy here would be the one that
	// goes stale.
	wanted := cleanModelIDs(s.modelIDs)
	if len(wanted) == 0 {
		wanted = plan.DefaultModels
	}

	s.preamble(host, plan, wanted)

	if plan.Refusal != "" {
		s.printRefusal(host, plan)
		return alreadySaid(SetupExitPrereq)
	}
	if err := s.ensureRuntime(ctx, plan); err != nil {
		return err
	}
	if !s.pullAllowed() {
		s.paragraph("This machine will not pull a model: models.pull is false in " +
			s.policyPath + ". Remove that key, or set it to true, and run this again.")
		return alreadySaid(SetupExitPrereq)
	}
	for _, id := range wanted {
		if err := s.pullOne(ctx, id, host.FreeDisk); err != nil {
			return err
		}
	}
	if err := s.allow(s.policyPath, wanted...); err != nil {
		return setupFailed("the models were pulled and %v", err)
	}
	s.printAllowed(wanted)
	s.printOffer(ctx)
	s.printReadvertise(ctx)
	return nil
}

// applyRuntimeFlag honours --runtime, or refuses.
//
// THE FLAG CANNOT WIDEN WHAT THE PLATFORM SERVES, and that is the whole
// of its behaviour. Design D1 fixes the mapping -- native Ollama on
// Apple Silicon, the container with GPU passthrough on Linux -- so the
// only thing an override can do is disagree, and a disagreement is
// REFUSED rather than ignored. Silently ignoring it is the failure worth
// naming: an operator who passed --runtime docker on a Mac and watched a
// successful setup would believe their models were being served from a
// container, and every later question they asked about it would start
// from a false premise.
//
// The one case where an override is honoured is a Linux machine that is
// ALREADY serving natively. Nothing has to be installed there, so
// nothing about the platform is being worked around -- the machine is
// simply reporting what it has.
func (s *inferenceSetup) applyRuntimeFlag(h inference.Host, p inference.Plan) (inference.Plan, error) {
	switch h.Runtime {
	case inference.RuntimeDocker:
		if p.Runtime == inference.RuntimeDocker || p.Refusal != "" {
			// Honoured (Linux), or refused by the plan itself in words
			// that already name the flag's way out.
			return p, nil
		}
		s.paragraph("This machine cannot serve models from Docker: on " + platformName(h.GOOS) +
			" a container has no access to the GPU, so it would serve from the CPU -- which is" +
			" what the hardware floor exists to prevent, and a machine serving that way is not" +
			" advertised at all.")
		s.line("")
		s.paragraph("Drop --runtime docker and this command installs Ollama natively.")
		return p, alreadySaid(SetupExitUsage)

	default:
		// Native is every platform's own default now, so asking for it
		// by name changes nothing -- and a machine already serving from
		// a container keeps doing so, since the runtime is a fact rather
		// than a choice and Install is empty either way.
		return p, nil
	}
}

// runtimePreference reads --runtime into the value Decide takes, or
// refuses a spelling it does not know as a usage error.
func (s *inferenceSetup) runtimePreference() (inference.Runtime, error) {
	switch strings.ToLower(strings.TrimSpace(s.runtimeFlag)) {
	case "":
		return inference.RuntimeNone, nil
	case "native":
		return inference.RuntimeNative, nil
	case "docker":
		return inference.RuntimeDocker, nil
	default:
		return inference.RuntimeNone, setupUsage("--runtime takes docker or native, not %q", s.runtimeFlag)
	}
}

// platformName spells a GOOS the way a person would say it. A refusal
// that reads "on darwin" sends somebody looking for a platform they do
// not own one of.
func platformName(goos string) string {
	switch goos {
	case "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	case "":
		return "this platform"
	default:
		return goos
	}
}

// preamble is the whole diagnosis in three lines, and each line is one
// of the three things that can be wrong. It is a fixed-width table
// rather than prose because it is READ RATHER THAN FOLLOWED: an operator
// scanning three machines' output compares the same column each time.
func (s *inferenceSetup) preamble(h inference.Host, p inference.Plan, wanted []string) {
	s.line("Setting this machine up to run local models.")
	s.line("")
	s.field("Hardware", hardwareLine(h))
	// The class earns its line because the model list stopped being a
	// constant: an operator who sees three models on one machine and
	// five on another, from the same command, is owed the reason on the
	// same screen. Without it the only available explanation is that
	// one of the two runs went wrong.
	s.field("Class", classLine(h.Hardware))
	s.field("Runtime", runtimeLine(p, s.base()))
	s.field("Models", strings.Join(wanted, ", "))
	s.line("")
}

func (s *inferenceSetup) field(label, value string) {
	fmt.Fprintf(s.out, "  %-11s%s\n", label, value)
}

// hardwareLine states what was observed and the verdict on it, in that
// order. Detail is what the probes SAW and can be empty when nothing
// could be established at all, which is why the verdict is never built
// out of it alone -- an empty Detail would print a line that begins with
// a dash and reads as truncated.
func hardwareLine(h inference.Host) string {
	verdict := "below the floor"
	if h.Floor.Met {
		verdict = "meets the floor"
	}
	if detail := strings.TrimSpace(h.Floor.Detail); detail != "" {
		return detail + " -- " + verdict
	}
	return verdict
}

// runtimeLine keeps the distinction between a runtime that is ALREADY
// RUNNING and one that is about to be installed. It is worth the word:
// the two states put a completely different meaning on everything that
// follows, and a person who reads "Ollama" on a machine that has none
// spends the pull wondering why it is slow.
func runtimeLine(p inference.Plan, base string) string {
	name := runtimeName(p.Runtime)
	switch {
	case p.RuntimePresent && base != "":
		return name + ", already running at " + base
	case p.RuntimePresent:
		return name + ", already running"
	case p.Refusal != "":
		return "none -- see below"
	case len(p.Install) > 0:
		return name + ", not installed yet"
	default:
		return name
	}
}

func runtimeName(r inference.Runtime) string {
	switch r {
	case inference.RuntimeNative:
		return "Ollama"
	case inference.RuntimeDocker:
		return "Ollama in Docker"
	default:
		return "none"
	}
}

// printRefusal prints the PLAN'S OWN sentence and nothing competing with
// it. A second sentence for the same fact drifts from the first, and
// then two surfaces disagree about why a machine is not in the model
// list.
//
// The closing line is not decoration. Somebody who reads "cannot serve
// local models" and nothing else concludes their pairing is broken and
// goes to unpair a machine that is working perfectly well.
func (s *inferenceSetup) printRefusal(h inference.Host, p inference.Plan) {
	if h.Floor.Met {
		s.line("This machine cannot serve local models yet.")
	} else {
		s.line("This machine cannot serve local models.")
	}
	s.line("")
	// The plan's sentence VERBATIM, with only its first letter raised.
	// The floor writes its reasons to be embedded ("NOT met -- this
	// machine has 8 GB..."), and here one stands alone as a paragraph;
	// raising the letter is a rendering of the same words, where
	// rewriting the sentence would be a second version of it that
	// drifts from the one `memql worker models` prints.
	s.paragraph(capitalise(p.Refusal))
	if strings.Contains(p.Refusal, "sudo") {
		s.line("")
		// Rule 8 of the brief: a line the cockpit prints and a line the
		// cockpit runs look identical on screen, so the copy has to say
		// which this is. Nothing in this process ever runs sudo --
		// install.go refuses such a command outright.
		s.paragraph("The commands above that start with sudo are for you to run yourself." +
			" This command never runs one.")
	}
	s.line("")
	if !strings.Contains(p.Refusal, "full worker") {
		s.line("It stays a full worker for everything else.")
	}
}

// ensureRuntime installs the runtime after asking, or explains why
// nothing was installed.
//
// The commands are PRINTED BEFORE THE QUESTION and the question covers
// all of them at once. Asking per command lets somebody approve `brew
// install ollama`, decline `brew services start ollama`, and be left
// with a binary and no runtime -- which InstallRuntime refuses to allow
// and this flow must not talk them into.
func (s *inferenceSetup) ensureRuntime(ctx context.Context, p inference.Plan) error {
	if p.RuntimePresent || len(p.Install) == 0 {
		return nil
	}
	if p.Stage != nil {
		base := strings.TrimRight(s.base(), "/")
		if base != "http://127.0.0.1:11434" && base != "http://localhost:11434" {
			return setupPrereq("OLLAMA_HOST selects %s, but native setup installs a loopback service at 127.0.0.1:11434. Start your chosen runtime yourself, or unset OLLAMA_HOST and run setup again; nothing was installed.", base)
		}
	}

	s.line(runtimeName(p.Runtime) + " is not installed. This machine needs it to serve models.")
	if note := strings.TrimSpace(p.Note); note != "" {
		s.line("")
		s.paragraph(note)
	}
	s.line("")
	// A native Linux plan does two kinds of thing, and both are on the
	// screen before the one question: the download and the file it
	// writes as sentences, the commands as the lines that will run. The
	// contract is the same for both -- nothing happens that was not shown.
	if p.Stage != nil {
		s.line("This will:")
		for _, l := range p.Stage.Lines() {
			s.line("  " + l)
		}
		s.line("and then run:")
	} else {
		s.line("These commands will run:")
	}
	for _, cmd := range p.Install {
		s.line("  " + cmd)
	}

	// A nil consent is a NO inside InstallRuntime, which is exactly what
	// --non-interactive means here: there is nobody to ask. Passing a
	// function that returned false would reach the same place, but the
	// nil says why in the type.
	var consent func([]string) bool
	if !s.nonInteractive {
		consent = s.askToInstall
	}

	// The stager gets this command's display, so a 1.4 GB download draws
	// the way a pull does rather than going silent for minutes.
	stage := func(ctx context.Context, st inference.Stage, _ func(inference.Progress)) error {
		d := newStageDisplay(s.out, s.tty)
		err := s.stage(ctx, st, d.handle)
		d.finish()
		return err
	}
	err := s.install(ctx, p, consent, inference.ExecRunner(s.out), stage)
	switch {
	case err == nil:
		if s.ready != nil {
			if rerr := s.ready(ctx, s.base()); rerr != nil {
				where := ""
				if p.Stage != nil {
					where = " Its log is " + p.Stage.LogPath + "."
				}
				return setupFailed("the runtime was installed and started, but %v.%s", rerr, where)
			}
		}
		s.line("")
		s.line("The runtime is installed and started.")
		s.line("")
		return nil

	case errors.Is(err, inference.ErrConsentRefused):
		s.line("")
		if s.nonInteractive {
			s.line("Nothing was installed: --non-interactive cannot answer that question.")
			s.line("Run this again without it, or run the commands above yourself.")
		} else {
			s.line("Nothing was installed.")
		}
		return alreadySaid(SetupExitRefused)

	case errors.Is(err, inference.ErrRefused):
		s.line("")
		s.paragraph(err.Error())
		return alreadySaid(SetupExitPrereq)

	default:
		return setupFailed("%v", err)
	}
}

// askToInstall is the consent question. [y/N] with the capital on the
// default, and anything but an explicit yes is a no -- including a
// closed stdin, which is what a piped caller that forgot
// --non-interactive presents as.
func (s *inferenceSetup) askToInstall(_ []string) bool {
	fmt.Fprint(s.out, "\nRun them now? [y/N] ")
	line, err := bufio.NewReader(s.in).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		fmt.Fprintln(s.out, "")
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// pullOne pulls one model and renders it.
func (s *inferenceSetup) pullOne(ctx context.Context, id string, freeDisk uint64) error {
	s.line("Pulling " + id)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	d := newPullDisplay(s.out, s.tty, freeDisk, cancel)
	err := s.pull(ctx, s.base(), id, d.handle)
	d.finish()

	// The disk refusal wins over the error the cancellation produced.
	// Pull reports a cancelled context as "pulling X was cancelled",
	// which is true and says nothing about why -- and "why" here is a
	// number the operator can act on.
	if over := d.overflow(id); over != nil {
		s.line("")
		s.paragraph(over.Error())
		s.line("")
		s.paragraph("Free some space on that volume, or pass --model with a smaller model, and run this again.")
		return alreadySaid(SetupExitOpFailed)
	}
	if err != nil {
		s.line("")
		s.paragraph(err.Error())
		return alreadySaid(SetupExitOpFailed)
	}
	s.line("")
	return nil
}

func (s *inferenceSetup) printAllowed(ids []string) {
	s.line("Allowed in " + s.policyPath)
	for _, id := range ids {
		s.line("  " + id)
	}
	s.line("")
}

// printOffer is the payoff: what the cluster will see, in the cluster's
// own words. It reads the machine again rather than restating the
// command line, so an id that never arrived is visibly absent instead of
// being reported as offered.
func (s *inferenceSetup) printOffer(ctx context.Context) {
	inv := s.inventory(ctx)
	advertised := inv.Advertised()
	if len(advertised) == 0 {
		s.paragraph("This machine offers no models yet. " + whyNothing(inv, s.policyPath))
		s.line("")
		return
	}
	s.line(fmt.Sprintf("This machine now offers %s. The cluster will see:", countModels(len(advertised))))
	pad := 0
	for _, m := range advertised {
		if len(m.ID) > pad {
			pad = len(m.ID)
		}
	}
	for _, m := range advertised {
		fmt.Fprintf(s.out, "  %-*s%s\n", pad+3, m.ID, attributeLine(m.Attributes))
	}
	s.line("")
}

func countModels(n int) string {
	if n == 1 {
		return "1 model"
	}
	return fmt.Sprintf("%d models", n)
}

// printReadvertise says what the SIGHUP achieved, and refuses to
// overstate it.
//
// A reload changes this machine's own answer to "may I serve this
// model" at once. THE CLUSTER DOES NOT SEE THE MODEL YET: model labels
// are bound at Register and Heartbeat carries none, so a newly allowed
// model becomes visible only after a reconnect the worker takes for
// itself. "Available now" would send somebody to the Fleet page to watch
// for a row that is not due yet, and they would read the delay as a
// failure -- which is the confusion `memql worker models` exists to end.
func (s *inferenceSetup) printReadvertise(ctx context.Context) {
	err := s.readvertise(ctx)
	if err == nil {
		s.line("The running worker was signalled and re-advertises within a minute or two.")
		return
	}
	s.paragraph(capitalise(err.Error()) + ".")
}

// capitalise raises the first letter of a sentence assembled from an
// error value. The errors are written lower-case for wrapping into
// other errors, and this is the one place they are read as prose.
func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (s *inferenceSetup) line(text string) { fmt.Fprintln(s.out, text) }

// paragraph wraps prose to inferenceWrapWidth. The refusals are single
// long sentences by construction -- they are asserted verbatim in the
// inference package's tests -- and a 300-column line over SSH on a bad
// connection is a sentence nobody finishes reading.
func (s *inferenceSetup) paragraph(text string) {
	for _, l := range wrapText(text, inferenceWrapWidth) {
		fmt.Fprintln(s.out, l)
	}
}

func wrapText(text string, width int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	var out []string
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) > width {
			out = append(out, line)
			line = w
			continue
		}
		line += " " + w
	}
	return append(out, line)
}

// cleanModelIDs trims, drops the empties and collapses duplicates,
// keeping the order they were typed in. A duplicate --model would
// otherwise pull the same model twice and print it twice in the closing
// block, which reads as two models on the machine.
func cleanModelIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// -----------------------------------------------------------------------------
// The progress display
// -----------------------------------------------------------------------------

// pullDisplay renders one model's pull, on a terminal and in a log file.
//
// OLLAMA'S PROGRESS IS PER-LAYER, AND A NAIVE BAR IS THEREFORE BROKEN.
// A model is several blobs, and Completed and Total belong to the blob
// named in Status -- the runtime restarts both counts for each one (see
// inference.Progress). A single bar keyed on them reaches the end, jumps
// to zero and starts again, which on screen is indistinguishable from a
// download that failed and restarted.
//
// So the layers are TRACKED BY DIGEST -- the status line carries it
// ("pulling 8934d96d3f08"), which is why Progress has no separate field
// for it -- and what is shown is the total across the layers seen so
// far. Completed and Total are accumulated as per-layer MAXIMA, so both
// counters are monotone by construction: a retried chunk cannot subtract
// bytes, and a duplicate event cannot add them twice.
//
// THE PERCENTAGE IS HELD RATHER THAN REWOUND. The denominator only ever
// grows -- a layer that has not started yet has announced no total -- so
// a small layer finishing before a large one is announced would drop the
// true fraction from 100% to single digits. That is the same "looks
// broken" failure by a different route, so the bar keeps the furthest
// position it has reached and starts moving again when the true fraction
// passes it. The BYTES beside it are never adjusted: they are the truth
// on that line, and the new total is announced on its own line at the
// moment it grows, so nothing on screen contradicts anything.
type pullDisplay struct {
	w    io.Writer
	tty  bool
	free uint64
	stop func()

	layers    map[string]*layerBytes
	completed uint64
	total     uint64
	percent   int

	announced uint64 // the total last announced on its own line
	status    string
	decile    int
	drawn     bool
	over      bool
	overNeed  uint64
}

type layerBytes struct{ completed, total uint64 }

func newPullDisplay(w io.Writer, tty bool, freeDisk uint64, stop func()) *pullDisplay {
	return &pullDisplay{
		w:      w,
		tty:    tty,
		free:   freeDisk,
		stop:   stop,
		layers: map[string]*layerBytes{},
		decile: -1,
	}
}

// handle takes one progress event. It runs on the goroutine reading the
// stream, so it does no work a slow terminal would turn into
// backpressure on the pull.
func (d *pullDisplay) handle(p inference.Progress) {
	// Once the pull is being stopped for space, NOTHING more is drawn.
	// Cancellation is not instant -- the runtime has already queued
	// events this side has not read -- and letting a bar run on to 100%
	// for a download that is being aborted is the display telling the
	// operator the opposite of what is about to be printed.
	if d.over {
		return
	}
	if p.Total > 0 || p.Completed > 0 {
		l := d.layers[p.Status]
		if l == nil {
			l = &layerBytes{}
			d.layers[p.Status] = l
		}
		if p.Total > l.total {
			d.total += p.Total - l.total
			l.total = p.Total
		}
		if p.Completed > l.completed {
			d.completed += p.Completed - l.completed
			l.completed = p.Completed
		}
	}
	if pct := d.truePercent(); pct > d.percent {
		d.percent = pct
	}

	// THE DISK CHECK LIVES HERE, and it cannot live in a pre-flight.
	// inference.Gather measures the free space and Decide deliberately
	// does not read it, because refusing on disk needs the SIZE OF THE
	// MODELS ACTUALLY REQUESTED (design section 5, "refused with both
	// numbers") -- and that size is knowable only once the runtime's own
	// stream reports a layer total. Pretending to a pre-flight would
	// mean guessing a size from a model id, which is a refusal that
	// names a number nobody can check.
	//
	// Free space of zero is NOT ESTABLISHED rather than "no space": the
	// statfs behind it fails on a volume this process cannot see, and
	// refusing on a fact that could not be read would block a machine
	// with a terabyte free.
	if d.free > 0 && d.total > d.free && !d.over {
		d.over = true
		d.overNeed = d.total
		if d.stop != nil {
			d.stop()
		}
		return
	}

	if d.tty {
		d.drawTTY(p.Status)
		return
	}
	d.drawLog(p.Status)
}

func (d *pullDisplay) truePercent() int {
	if d.total == 0 {
		return 0
	}
	pct := int(d.completed * 100 / d.total)
	if pct > 100 {
		return 100
	}
	return pct
}

// drawTTY redraws in place with a carriage return. The line is padded to
// a fixed width so a shorter line never leaves the tail of a longer one
// behind it -- no ANSI erase sequence, because this output is also read
// in `script` captures and over links that mangle them.
func (d *pullDisplay) drawTTY(status string) {
	if d.announceTotal() {
		d.endLine()
		fmt.Fprintf(d.w, "  %s total\n", humanBytes(d.total))
	}
	fmt.Fprintf(d.w, "\r  %-*s", inferenceWrapWidth, d.barLine(status))
	d.drawn = true
}

// drawLog prints ONE line per state change, which is the whole reason
// the TTY check exists. Two thousand carriage returns in an install
// script's log file is the failure being avoided, and a log that says
// nothing at all for the eight minutes of a four-gigabyte download is
// the other one -- so a decile of the aggregate counts as a state change
// alongside the runtime's own status lines. That is eleven lines per
// model at the very most.
func (d *pullDisplay) drawLog(status string) {
	if d.announceTotal() {
		fmt.Fprintf(d.w, "  %s total\n", humanBytes(d.total))
	}
	if status != "" && status != d.status {
		d.status = status
		if !isByteStatus(status) {
			fmt.Fprintf(d.w, "  %s\n", status)
		}
	}
	if d.total == 0 {
		return
	}
	if decile := d.percent / 10; decile > d.decile {
		d.decile = decile
		fmt.Fprintf(d.w, "  %3d%%   %s / %s\n", d.percent, humanBytes(d.completed), humanBytes(d.total))
	}
}

// announceTotal reports that the download's known size has grown, which
// is the honest way to show a denominator that is still being learned.
// It fires on the first layer and again on any later one, so a total
// that moves is stated rather than quietly swapped under the bar.
func (d *pullDisplay) announceTotal() bool {
	if d.total == 0 || d.total == d.announced {
		return false
	}
	d.announced = d.total
	return true
}

// isByteStatus reports a status line that names a blob. Those are the
// lines the bar already renders, and echoing "pulling 8934d96d3f08"
// beside a byte count adds a digest nobody can act on.
func isByteStatus(status string) bool {
	return strings.HasPrefix(status, "pulling ") && status != "pulling manifest"
}

func (d *pullDisplay) barLine(status string) string {
	if d.total == 0 {
		return strings.TrimSpace(status)
	}
	return fmt.Sprintf("%s %3d%%   %s / %s", progressBar(d.percent), d.percent, humanBytes(d.completed), humanBytes(d.total))
}

func progressBar(percent int) string {
	filled := percent * pullBarCells / 100
	if filled < 0 {
		filled = 0
	}
	if filled > pullBarCells {
		filled = pullBarCells
	}
	return "[" + strings.Repeat("=", filled) + strings.Repeat(" ", pullBarCells-filled) + "]"
}

// finish closes the in-place line so whatever is printed next starts on
// a fresh one. It runs on the failure paths too: an error message that
// began halfway along a progress bar is one nobody can read.
func (d *pullDisplay) finish() { d.endLine() }

// stageDisplay draws a runtime archive download: one heading per
// archive, then the same bar a pull gets on a terminal or one line per
// decile in a log. It is not the pull display because that one reads
// Ollama's per-layer statuses, and an archive has one stream of bytes.
type stageDisplay struct {
	w      io.Writer
	tty    bool
	name   string
	decile int
	drawn  bool
}

func newStageDisplay(w io.Writer, tty bool) *stageDisplay {
	return &stageDisplay{w: w, tty: tty, decile: -1}
}

func (d *stageDisplay) handle(p inference.Progress) {
	if p.Model != d.name {
		d.endLine()
		d.name = p.Model
		d.decile = -1
		fmt.Fprintf(d.w, "  %s\n", p.Status)
		if p.Total > 0 {
			fmt.Fprintf(d.w, "  %s total\n", humanBytes(p.Total))
		}
	}
	if p.Total == 0 {
		return
	}
	percent := int(p.Completed * 100 / p.Total)
	if d.tty {
		fmt.Fprintf(d.w, "\r  %-*s", inferenceWrapWidth,
			fmt.Sprintf("%s %3d%%   %s / %s", progressBar(percent), percent, humanBytes(p.Completed), humanBytes(p.Total)))
		d.drawn = true
		return
	}
	if decile := percent / 10; decile > d.decile {
		d.decile = decile
		fmt.Fprintf(d.w, "  %3d%%   %s / %s\n", percent, humanBytes(p.Completed), humanBytes(p.Total))
	}
}

func (d *stageDisplay) finish() { d.endLine() }

func (d *stageDisplay) endLine() {
	if !d.drawn {
		return
	}
	fmt.Fprintln(d.w, "")
	d.drawn = false
}

func (d *pullDisplay) endLine() {
	if !d.drawn {
		return
	}
	fmt.Fprintln(d.w, "")
	d.drawn = false
}

// overflow is the disk refusal, naming BOTH numbers and the model. Nil
// when the pull was not stopped for space.
//
// It breaks Go's error-string convention on purpose -- capital, full
// stop -- because this value is never wrapped into another error and
// never logged: its only reader is a person, in a terminal, and it is
// printed as the standalone sentence it is written as.
func (d *pullDisplay) overflow(model string) error {
	if !d.over {
		return nil
	}
	return fmt.Errorf("There is not enough disk to pull %s: it needs %s, and %s is free on the volume this machine keeps models on.",
		model, humanBytes(d.overNeed), humanBytes(d.free))
}

// -----------------------------------------------------------------------------
// `memql worker models --pull / --allow`
// -----------------------------------------------------------------------------

// runModelPulls pulls each requested model into the local runtime.
//
// It does NOT write models.allow. Pulling and allowing are separate acts
// -- one spends bandwidth and disk, the other spends this machine's GPU
// on somebody else's prompt -- and a --pull that quietly granted the
// second would be a policy change nobody typed. The closing line names
// --allow instead.
func runModelPulls(ctx context.Context, s *inferenceSetup, ids []string) error {
	if !s.pullAllowed() {
		s.paragraph("This machine will not pull a model: models.pull is false in " +
			s.policyPath + ". Remove that key, or set it to true, and run this again.")
		return alreadySaid(SetupExitPrereq)
	}
	host, err := s.gather(ctx)
	if err != nil {
		return setupFailed("this machine could not be inspected: %v", err)
	}
	for _, id := range ids {
		if err := s.pullOne(ctx, id, host.FreeDisk); err != nil {
			return err
		}
	}
	return nil
}

// runModelActs runs --pull and then --allow, in that order, and returns
// the process exit code.
//
// THE ORDER IS NOT ARBITRARY. Allowing a model puts it in front of the
// cluster's router; pulling one puts it on the disk. Allow-then-pull
// would leave a window in which the machine advertises a model it cannot
// serve, and the calls that land in it fail on somebody else's prompt.
func runModelActs(ctx context.Context, s *inferenceSetup, pull, allow []string) int {
	var err error
	if len(pull) > 0 {
		err = runModelPulls(ctx, s, pull)
	}
	if err == nil && len(allow) > 0 {
		err = runModelAllows(ctx, s, allow)
	}
	if err == nil && len(pull) > 0 {
		s.notePulledButBlocked(pull)
	}
	if err != nil {
		if msg := err.Error(); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
	}
	return SetupExitCode(err)
}

// notePulledButBlocked names the next command for a model that is now on
// the disk and still not offered.
//
// models.allow is default-deny, so a pull on its own changes nothing the
// cluster can see -- and a command that reported a successful pull and
// stopped there is indistinguishable, from the portal, from one that
// never ran. Naming the exact command is the difference between an
// operator who finishes and one who files a bug about a model that
// "does not show up".
func (s *inferenceSetup) notePulledButBlocked(pulled []string) {
	allowed := map[string]struct{}{}
	for _, id := range policyAt(s.policyPath).ModelsAllow() {
		allowed[strings.TrimSpace(id)] = struct{}{}
	}
	var blocked []string
	for _, id := range pulled {
		if _, ok := allowed[id]; !ok {
			blocked = append(blocked, id)
		}
	}
	if len(blocked) == 0 {
		return
	}
	s.paragraph("Pulled, and not offered: models.allow in " + s.policyPath +
		" is default-deny, so the cluster is told about none of these yet.")
	s.line("")
	s.line("  memql worker models --allow " + strings.Join(blocked, " --allow "))
	s.line("")
}

// runModelAllows adds ids to models.allow and tells the running worker.
func runModelAllows(ctx context.Context, s *inferenceSetup, ids []string) error {
	if err := s.allow(s.policyPath, ids...); err != nil {
		return setupFailed("%v", err)
	}
	s.printAllowed(ids)
	s.printReadvertise(ctx)
	s.line("")
	return nil
}

// humanBytes renders a byte count the way `ollama list` does: decimal
// units, one decimal place, the trailing .0 trimmed. An operator
// compares this against what the runtime's own CLI shows them, so a
// binary "4.3 GiB" beside Ollama's "4.7 GB" for the same blob would read
// as two different files.
func humanBytes(n uint64) string {
	switch {
	case n >= 1_000_000_000:
		return oneDecimal(float64(n)/1e9) + " GB"
	case n >= 1_000_000:
		return oneDecimal(float64(n)/1e6) + " MB"
	case n >= 1_000:
		return oneDecimal(float64(n)/1e3) + " kB"
	default:
		return strconv.FormatUint(n, 10) + " B"
	}
}

// humanParams renders a parameter COUNT the way a model card does. The
// label carries the count itself (the engine ranks on it numerically);
// this is the spelling an operator matches against `ollama list`.
func humanParams(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return oneDecimal(float64(n)/1e9) + "B"
	case n >= 1_000_000:
		return oneDecimal(float64(n)/1e6) + "M"
	default:
		return strconv.FormatInt(n, 10) + " parameters"
	}
}

func oneDecimal(f float64) string {
	return strings.TrimSuffix(strconv.FormatFloat(f, 'f', 1, 64), ".0")
}
