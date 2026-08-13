// Command webauthn-rpid is the throwaway harness for the memql#3405 spike:
// does a WebAuthn ceremony with an RP ID under the `.localhost` TLD actually
// work, on a desktop platform authenticator and over hybrid (QR -> phone)
// transport?
//
// WHY A HARNESS AND NOT A UNIT TEST. The question is not what the WebAuthn
// spec says -- that leg is settled analytically (see README.md). It is what
// four independent implementations DO: the browser's RP ID validator, the
// desktop platform authenticator, and the iOS and Android passkey providers
// reached over hybrid transport. None of those is reachable from Go. The only
// instrument that answers the last three is a human holding a phone in front
// of a browser.
//
// THE FIRST ONE, THOUGH, IS AUTOMATABLE, AND THIS SERVES IT AUTOMATICALLY.
// `navigator.credentials.create()` runs its RP ID validation BEFORE consulting
// any authenticator, so a call aborted after a couple of seconds still reaches
// the validator and still reports its verdict. The page therefore probes every
// RP ID case the moment it loads -- with both controls -- and POSTs the results
// back here. That is what makes "measure Safari's validator" a matter of
// opening a URL on a Mac rather than a scripted browser automation nobody has
// (memql#3405 leg 1). The human legs stay buttons.
//
// WHY IT DOES NO SERVER-SIDE VERIFICATION. The spike asks whether the ceremony
// is PERMITTED, not whether an attestation verifies. Both failure modes --
// SecurityError from the browser's RP ID validation, NotAllowedError from the
// ceremony itself -- are visible from the promise alone, which is why this
// server never needs the go-webauthn dependency, a database, or any of
// component/identity. Keeping it that self-contained is also what lets it run
// against a domain the real cluster is not serving.
//
// WHY IT WRITES THE TABLE ITSELF. memql#3405's acceptance criteria are a
// filled-in results table with exact browser versions. A table transcribed by
// hand from a scrolling log, hours after the fact, is the step where a
// `NotAllowedError` becomes "didn't work" and the finding stops being usable.
// Every outcome -- probed or clicked -- lands in results.jsonl as it happens
// and is rendered to results.md, user-agent string included.
//
// DELETE ME once memql#3405's findings are recorded. This is a time-boxed
// spike harness, not a supported tool.
//
// Usage: see run.sh, which is the supported entry point. Directly:
//
//	go run ./scripts/spikes/webauthn-rpid \
//	  --rp-id=memql.localhost \
//	  --addr=127.0.0.1:8443 \
//	  --cert=/tmp/memql-spike-3405/_wildcard.memql.localhost+1.pem \
//	  --key=/tmp/memql-spike-3405/_wildcard.memql.localhost+1-key.pem \
//	  --results=/tmp/memql-spike-3405
//
// then open https://identity.memql.localhost:8443/ in the browser under test.
package main

import (
	"crypto/tls"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var assets embed.FS

// result is one measured outcome: either a validator probe the page ran by
// itself, or a ceremony a human drove to completion.
//
// UserAgent rides on EVERY row rather than being recorded once per session.
// The whole point of the table is which BROWSER did what, and a spike run
// across Safari and Chrome on one machine within the same minute is the normal
// case, not the exotic one.
type result struct {
	At        string `json:"at"`
	Leg       string `json:"leg"`
	RPID      string `json:"rpId"`
	Origin    string `json:"origin"`
	Outcome   string `json:"outcome"`
	ErrorName string `json:"errorName,omitempty"`
	Detail    string `json:"detail,omitempty"`
	UserAgent string `json:"userAgent"`
	Note      string `json:"note,omitempty"`
}

// recorder appends results to a JSONL file and re-renders the markdown table
// after each one.
//
// JSONL is the record of truth and the markdown is derived, not the other way
// round: the table is a lossy view (it drops the detail string and rounds the
// timestamp), and re-deriving it means a run interrupted halfway still leaves
// both files consistent with each other.
type recorder struct {
	mu   sync.Mutex
	dir  string
	rows []result
}

func newRecorder(dir string) (*recorder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating results directory: %w", err)
	}
	r := &recorder{dir: dir}
	// Load what is already there so a second run (the control domain, or the
	// operator's Safari pass after their Chrome one) EXTENDS the table rather
	// than starting a new one. The two runs are halves of a single finding.
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *recorder) load() error {
	raw, err := os.ReadFile(filepath.Join(r.dir, "results.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading prior results: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row result
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			// A corrupt line is not worth aborting a spike over, but it must
			// not be silent either -- a dropped row is a missing measurement.
			log.Printf("WARNING: skipping unparseable results line: %v", err)
			continue
		}
		r.rows = append(r.rows, row)
	}
	return nil
}

func (r *recorder) add(row result) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	row.At = time.Now().UTC().Format(time.RFC3339)
	r.rows = append(r.rows, row)

	encoded, err := json.Marshal(row)
	if err != nil {
		return fmt.Errorf("encoding result: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(r.dir, "results.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening results file: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("appending result: %w", err)
	}
	return r.renderLocked()
}

// renderLocked writes results.md. Caller holds the mutex.
func (r *recorder) renderLocked() error {
	var b strings.Builder
	b.WriteString("# memql#3405 -- measured results\n\n")
	b.WriteString("Generated by `scripts/spikes/webauthn-rpid`. Paste into the issue and the design doc's section 8.\n\n")

	// Grouped by browser, because that is the axis the finding turns on: the
	// open question is whether WebKit diverges from Chromium, and a table
	// sorted by time interleaves them into noise.
	byUA := map[string][]result{}
	for _, row := range r.rows {
		byUA[row.UserAgent] = append(byUA[row.UserAgent], row)
	}
	agents := make([]string, 0, len(byUA))
	for ua := range byUA {
		agents = append(agents, ua)
	}
	sort.Strings(agents)

	for _, ua := range agents {
		b.WriteString("## " + ua + "\n\n")
		b.WriteString("| Leg | RP ID | Origin | Outcome | Error | Note |\n")
		b.WriteString("|---|---|---|---|---|---|\n")
		for _, row := range byUA[ua] {
			b.WriteString(fmt.Sprintf(
				"| %s | `%s` | `%s` | **%s** | %s | %s |\n",
				md(row.Leg), md(row.RPID), md(row.Origin), md(row.Outcome),
				dash(md(row.ErrorName)), dash(md(row.Note)),
			))
		}
		b.WriteString("\n")
	}

	if len(r.rows) == 0 {
		b.WriteString("_No results recorded yet._\n")
	}
	return os.WriteFile(filepath.Join(r.dir, "results.md"), []byte(b.String()), 0o644)
}

// md escapes the two characters that would break out of a markdown table cell.
// Everything here is untrusted by construction -- a user-agent string and an
// authenticator's own error text -- and a table that renders as garbage is a
// finding nobody can read.
func md(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.ReplaceAll(s, "\n", " ")
}

func dash(s string) string {
	if s == "" {
		return "--"
	}
	return s
}

func main() {
	var (
		rpID     = flag.String("rp-id", "memql.localhost", "the RP ID the ceremony buttons use (the parent-domain case)")
		addr     = flag.String("addr", "127.0.0.1:8443", "listen address")
		certFile = flag.String("cert", "", "TLS certificate (mkcert); required")
		keyFile  = flag.String("key", "", "TLS private key (mkcert); required")
		origin   = flag.String("origin", "", "origin the page will be reached at; defaults to https://identity.<rp-id>:<port>")
		results  = flag.String("results", "", "directory to write results.jsonl / results.md into; required")
	)
	flag.Parse()

	if *certFile == "" || *keyFile == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --cert and --key are required. Run scripts/spikes/webauthn-rpid/run.sh instead.")
		os.Exit(2)
	}
	if *results == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --results is required -- the point of the harness is the table it writes.")
		os.Exit(2)
	}

	shownOrigin := *origin
	if shownOrigin == "" {
		_, port, _ := splitHostPort(*addr)
		shownOrigin = "https://identity." + *rpID + ":" + port
	}

	rec, err := newRecorder(*results)
	if err != nil {
		log.Fatalf("ERROR: %v", err)
	}

	tmpl, err := template.ParseFS(assets, "index.html")
	if err != nil {
		log.Fatalf("ERROR: parsing embedded page: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// No CSP header at all: a spike page that blocks its own inline script
		// reports a failure that has nothing to do with the question.
		if err := tmpl.Execute(w, map[string]string{
			"RPID":   *rpID,
			"Origin": shownOrigin,
		}); err != nil {
			log.Printf("ERROR: rendering page: %v", err)
		}
	})

	mux.HandleFunc("POST /result", func(w http.ResponseWriter, r *http.Request) {
		var row result
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&row); err != nil {
			http.Error(w, "bad result payload", http.StatusBadRequest)
			return
		}
		if err := rec.add(row); err != nil {
			log.Printf("ERROR: recording result: %v", err)
			http.Error(w, "could not record", http.StatusInternalServerError)
			return
		}
		// Echoed to the terminal too. The operator is watching this window
		// while the phone is in their other hand, and a spike whose progress
		// is only visible in a browser tab they are not looking at is one they
		// will re-run because they could not tell whether it took.
		log.Printf("RESULT: %-22s rp=%-26s %-8s %s", row.Leg, row.RPID, row.Outcome, row.ErrorName)
		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}

	log.Printf("INFO: rp id      %s", *rpID)
	log.Printf("INFO: listening  %s", *addr)
	log.Printf("INFO: results    %s/results.md", *results)
	log.Printf("INFO: open       %s", shownOrigin)
	log.Printf("INFO: the validator probes run on page load; the ceremony buttons need you.")

	if err := serveLoopback(srv, *addr, *certFile, *keyFile); err != nil {
		log.Fatalf("ERROR: serving: %v", err)
	}
}

// serveLoopback listens on BOTH loopback stacks when the address names a
// loopback host, and on the given address alone otherwise.
//
// WHY. `identity.memql.localhost` does not resolve the same way twice. macOS
// and systemd-resolved both synthesise an answer for `*.localhost` with no
// hosts entry, but systemd-resolved answers `::1` while an /etc/hosts line
// answers `127.0.0.1`, and Chrome bypasses the resolver altogether. A server
// bound to 127.0.0.1 alone therefore refuses the connection on exactly the
// machines where the name resolved fine -- which surfaces as "connection
// refused" seconds after the log line said it was listening, and costs the
// operator the first twenty minutes of a time-boxed spike.
//
// Binding both loopback addresses rather than `:port` is the point: the wide
// bind would fix the same problem by putting the harness on every interface,
// including the LAN. It serves a page that starts WebAuthn ceremonies, and a
// throwaway spike is not the thing to be casual with.
func serveLoopback(srv *http.Server, addr, certFile, keyFile string) error {
	host, port, _ := splitHostPort(addr)
	if !isLoopbackHost(host) {
		return srv.ListenAndServeTLS(certFile, keyFile)
	}

	addrs := []string{net.JoinHostPort("127.0.0.1", port), net.JoinHostPort("::1", port)}
	errs := make(chan error, len(addrs))
	bound := 0
	for _, a := range addrs {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			// One stack missing is normal (an IPv6-disabled kernel, a
			// v4-only container) and is not a reason to refuse to run --
			// but it IS the reason a later connection fails, so say it.
			log.Printf("WARNING: not listening on %s: %v", a, err)
			continue
		}
		bound++
		log.Printf("INFO: bound     %s", a)
		go func() { errs <- srv.ServeTLS(ln, certFile, keyFile) }()
	}
	if bound == 0 {
		return fmt.Errorf("could not bind either loopback stack on port %s", port)
	}
	return <-errs
}

func isLoopbackHost(host string) bool {
	switch host {
	case "127.0.0.1", "::1", "[::1]", "localhost", "":
		return true
	}
	return false
}

// splitHostPort is a forgiving split that tolerates a missing port, because a
// bad --addr should surface as a listen error rather than as a panic here.
func splitHostPort(addr string) (host, port string, ok bool) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i], addr[i+1:], true
		}
	}
	return addr, "443", false
}
