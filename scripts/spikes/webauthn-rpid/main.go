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
// instrument that answers is a human holding a phone in front of a browser.
//
// WHY IT DOES NO SERVER-SIDE VERIFICATION. The spike asks whether the ceremony
// is PERMITTED, not whether an attestation verifies. `navigator.credentials
// .create()` rejects with a SecurityError before any authenticator is invoked
// when the RP ID fails validation, and the authenticator refuses before
// returning when it is the one objecting. Both failure modes are visible from
// the promise alone, which is why this server never needs the go-webauthn
// dependency, a database, or any of component/identity. Keeping it that
// self-contained is also what lets it run against a domain the real cluster is
// not serving.
//
// DELETE ME once memql#3405's findings are recorded. This is a time-boxed
// spike harness, not a supported tool.
//
// Usage:
//
//	go run ./scripts/spikes/webauthn-rpid \
//	  --rp-id=memql.localhost \
//	  --addr=127.0.0.1:8443 \
//	  --cert=./identity.memql.localhost.pem \
//	  --key=./identity.memql.localhost-key.pem
//
// then open https://identity.memql.localhost:8443/ in the browser under test.
// See README.md for the mkcert and /etc/hosts prerequisites.
package main

import (
	"crypto/tls"
	"embed"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"time"
)

//go:embed index.html
var assets embed.FS

func main() {
	var (
		rpID     = flag.String("rp-id", "memql.localhost", "RP ID to attempt the ceremony with")
		addr     = flag.String("addr", "127.0.0.1:8443", "listen address")
		certFile = flag.String("cert", "", "TLS certificate (mkcert); required")
		keyFile  = flag.String("key", "", "TLS private key (mkcert); required")
		origin   = flag.String("origin", "", "origin the page will be reached at; defaults to https://identity.<rp-id>:<port>")
	)
	flag.Parse()

	if *certFile == "" || *keyFile == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --cert and --key are required. See README.md for the mkcert invocation.")
		os.Exit(2)
	}

	shownOrigin := *origin
	if shownOrigin == "" {
		_, port, _ := splitHostPort(*addr)
		shownOrigin = "https://identity." + *rpID + ":" + port
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

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}

	log.Printf("INFO: rp id      %s", *rpID)
	log.Printf("INFO: listening  %s", *addr)
	log.Printf("INFO: open       %s", shownOrigin)
	log.Printf("INFO: the ceremony runs entirely in the browser; this server only serves the page over TLS.")

	if err := srv.ListenAndServeTLS(*certFile, *keyFile); err != nil {
		log.Fatalf("ERROR: serving: %v", err)
	}
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
