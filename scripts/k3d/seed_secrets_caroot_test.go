package k3d

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// seed_secrets_caroot_test.go -- znasllc-io/memql#4069.
//
// WHAT HAPPENED. The install graph's `localCA` step runs
//
//	install.mkcert --caroot=$HOME/.memql/mkcert --confirm=install-memql-ca
//
// and succeeds: the CA is created under THAT root, because a packaged editor
// resolves mkcert's own default out of a revision-scoped XDG directory that
// moves on the next refresh (memql#3576). Two steps later `clusterUp` runs
// k3d.up, which runs this script, which calls install.mkcert AGAIN to issue the
// front-door pair -- and passed no CAROOT at all. install.mkcert therefore fell
// back to `mkcert -CAROOT`, the machine default, which on a clean machine holds
// nothing. It exits 3 (no CA, no confirmation phrase), this script maps that to
// exit 4 "no mkcert root CA exists on this machine yet", and the whole install
// dies at clusterUp -- naming a prerequisite the install had already satisfied
// one step earlier, under a different root.
//
// AND THE SILENT HALF IS WORSE. On a machine that has run `make up` before, the
// default CAROOT already holds a CA, so the nested call finds one and succeeds
// -- issuing the cluster's front-door certificate from a CA THE INSTALLER DID
// NOT CREATE, does not own, and will not remove on uninstall (that removal is
// gated on the `.memql-created` marker beside the key material). Nothing warns.
// The install reports success. The only visible symptom is on the machine where
// there is no CA to borrow, which is CI -- three legs, all failing identically
// at clusterUp, which is how this was finally seen at all.
//
// So the fix is that the CAROOT TRAVELS, exactly as --repo-root does
// (memql#3570) and for the identical reason: a value resolved by one step and
// re-derived by the next is two sources of truth, and they disagree on the
// machine that matters.
//
// These tests drive the REAL script. The forwarding pair uses the
// --mkcert-setup seam (the issuer stand-in memql#3730 added) to record the argv
// the child is invoked with; the end-to-end case drives the REAL
// install.mkcert against a stub mkcert whose *default* root holds no CA -- the
// CI machine, reproduced.

// carootRecordingIssuer stands in for install.mkcert and records the argv it
// was invoked with, ONE ARGUMENT PER LINE.
//
// Per argument rather than `"$*"`, because word splitting is one of the things
// under test: a CAROOT is a path and may contain a space, and `$*` joins the
// argv back together with spaces -- so a root that arrived split into two
// arguments would render identically to one that arrived intact, which is the
// single failure this recorder must be able to see.
//
// It also creates the pair it was asked for and emits a well-formed envelope,
// because seed-secrets refuses a child that reports neither (memql#3730) -- so
// a recorder that recorded and nothing else would fail the run for a reason
// that has nothing to do with what is under test here.
func carootRecordingIssuer(t *testing.T, dir, log string) string {
	t.Helper()
	path := filepath.Join(dir, "recording-issuer.sh")
	body := `#!/usr/bin/env bash
printf 'ARG %s\n' "$@" >> "` + log + `"
cert=""; key=""
for a in "$@"; do
  case "$a" in
    --cert-file=*) cert="${a#*=}" ;;
    --key-file=*)  key="${a#*=}" ;;
  esac
done
[ -n "$cert" ] && { mkdir -p "$(dirname "$cert")"; printf 'fake-cert\n' > "$cert"; }
[ -n "$key" ]  && { mkdir -p "$(dirname "$key")";  printf 'fake-key\n'  > "$key"; }
printf '{"ok":true,"capability":"install.mkcert","changed":true,"result":{"certIssued":true,"reissued":false,"coverageVerified":true},"error":null}\n'
exit 0
`
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write recording issuer: %v", err)
	}
	return path
}

func readIssuerLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read issuer log: %v", err)
	}
	return string(b)
}

// The contract surface a caller can read WITHOUT running anything, and the
// gate on the flag existing at all: cap_parse_flags refuses an undeclared
// --caroot with exit 2, so k3d.up forwarding one this script had not declared
// would not be a no-op -- it would abort every `make up` at secret seeding.
func TestSeedSecretsDeclaresTheCarootParam(t *testing.T) {
	root := repoRoot(t)
	out, err := exec.Command("bash", filepath.Join(root, "scripts", "k3d", "seed-secrets.sh"), "--print-spec").CombinedOutput()
	if err != nil {
		t.Fatalf("--print-spec failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `"caroot"`) {
		t.Errorf("--print-spec does not declare --caroot, so no caller can pass the CAROOT the install created\n%s", out)
	}
}

// THE THREADING, in both directions, in one test -- because each direction on
// its own is only half a claim. Forwarding a value nobody asked for would break
// `make up` / `make secrets`, which pass no CAROOT and must keep resolving
// mkcert's own default exactly as they do today; forwarding nothing when a
// value WAS given is the bug.
func TestSeedSecretsForwardsTheCarootItWasGivenAndNothingWhenItWasNot(t *testing.T) {
	// Given.
	e := newFrontDoorEnv(t)
	e.seedCA(t)
	log := filepath.Join(e.home, "issuer-argv.log")
	issuer := carootRecordingIssuer(t, e.home, log)
	installerCaroot := filepath.Join(e.home, ".memql", "mkcert")

	// When: the caller names the CAROOT the install created.
	if _, _, code := e.run(t, "--mkcert-setup="+issuer, "--caroot="+installerCaroot); code != 0 {
		t.Fatalf("exit %d with --caroot; stderr:\n%s", code, e.lastStderr)
	}

	// Then: install.mkcert is told which root to look in.
	got := readIssuerLog(t, log)
	if !strings.Contains(got, "--caroot="+installerCaroot) {
		t.Errorf("install.mkcert was invoked without the CAROOT this run was given (%s),\n"+
			"so it would fall back to `mkcert -CAROOT` -- the machine default, which is\n"+
			"either empty (the CI failure) or somebody else's CA (the silent one).\n"+
			"issuer argv:\n%s", installerCaroot, got)
	}

	// When: nothing names one -- `make up`, `make secrets`, every developer run.
	if err := os.Remove(log); err != nil {
		t.Fatalf("reset issuer log: %v", err)
	}
	e2 := newFrontDoorEnv(t)
	e2.seedCA(t)
	log2 := filepath.Join(e2.home, "issuer-argv.log")
	issuer2 := carootRecordingIssuer(t, e2.home, log2)
	if _, _, code := e2.run(t, "--mkcert-setup="+issuer2); code != 0 {
		t.Fatalf("exit %d without --caroot; stderr:\n%s", code, e2.lastStderr)
	}

	// Then: no CAROOT is invented, so install.mkcert resolves the default it
	// always has. Absence is the behaviour-preserving half of this change.
	if got := readIssuerLog(t, log2); strings.Contains(got, "--caroot") {
		t.Errorf("a --caroot appeared with none supplied, which changes what `make secrets`\n"+
			"resolves on every developer machine.\nissuer argv:\n%s", got)
	}
}

// A CAROOT IS A PATH, so it can contain a space -- `/Users/Jane Smith/...` is an
// ordinary macOS home. The forwarding is written as
// `${MKCERT_CAROOT:+--caroot="${MKCERT_CAROOT}"}`, and dropping the inner quotes
// would split it into two arguments: install.mkcert would then resolve a
// truncated root, find no CA there, and refuse with "no mkcert root CA exists"
// -- indistinguishable, from the operator's side, from the bug this change fixes.
func TestSeedSecretsForwardsACarootContainingASpace(t *testing.T) {
	e := newFrontDoorEnv(t)
	e.seedCA(t)
	log := filepath.Join(e.home, "issuer-argv.log")
	issuer := carootRecordingIssuer(t, e.home, log)
	spaced := filepath.Join(e.home, "Application Support", ".memql", "mkcert")

	if _, _, code := e.run(t, "--mkcert-setup="+issuer, "--caroot="+spaced); code != 0 {
		t.Fatalf("exit %d; stderr:\n%s", code, e.lastStderr)
	}

	// One line per argument, so this can only match if the root arrived whole.
	if got := readIssuerLog(t, log); !strings.Contains(got, "ARG --caroot="+spaced+"\n") {
		t.Errorf("the CAROOT did not survive as a single argument.\nwant: ARG --caroot=%s\nissuer argv:\n%s", spaced, got)
	}
}

// carootStubMkcert is the CI machine: `mkcert -CAROOT` answers with a default
// root that holds NO CA, while a second root -- the one the install's localCA
// step created -- does.
//
// It logs the CAROOT of every invocation, not merely the argv, because "which
// CA signed the cluster's front-door certificate" is the fact this whole issue
// is about and it travels in the environment rather than in an argument.
const carootStubMkcert = `#!/usr/bin/env bash
printf 'CAROOT=%s ARGV=%s\n' "${CAROOT:-<default>}" "$*" >> "$STUB_LOG"

case "$1" in
  -CAROOT) printf '%s\n' "$STUB_CAROOT"; exit 0 ;;
  -install)
    root="${CAROOT:-$STUB_CAROOT}"
    mkdir -p "$root"
    printf 'stub-root-ca\n' > "$root/rootCA.pem"
    exit 0 ;;
esac

cert=""; key=""
while [ $# -gt 0 ]; do
  case "$1" in
    -cert-file) cert="$2"; shift 2 ;;
    -key-file)  key="$2";  shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$cert" ] && printf 'stub-cert\n' > "$cert"
[ -n "$key" ]  && printf 'stub-key\n'  > "$key"
exit 0
`

// THE CI CONDITION, END TO END, through the REAL install.mkcert.
//
// The machine has no CA where mkcert looks by default; the install created one
// somewhere else and knows where. Both halves are asserted in one test on
// purpose: the refusal without a CAROOT is what CI hit three times, and it is
// only evidence of anything alongside the run that differs by exactly the flag.
func TestSeedSecretsIssuesFromTheCarootItWasGiven(t *testing.T) {
	// Given: a clean machine (e.caroot is the default root and is deliberately
	// NOT seeded), and the installer's own root, which holds the CA localCA made.
	e := newFrontDoorEnv(t)
	if err := os.WriteFile(e.stubMkcert, []byte(carootStubMkcert), 0o755); err != nil {
		t.Fatalf("write caroot stub: %v", err)
	}
	installerCaroot := filepath.Join(e.home, ".memql", "mkcert")
	if err := os.MkdirAll(installerCaroot, 0o755); err != nil {
		t.Fatalf("mkdir installer caroot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(installerCaroot, "rootCA.pem"), []byte("installer-ca\n"), 0o644); err != nil {
		t.Fatalf("seed installer CA: %v", err)
	}

	// When: nothing tells this script where the CA is. Then: the exact failure
	// the three CI legs died on -- a prerequisite the install had already met.
	res, _, code := e.run(t)
	if code != 4 {
		t.Fatalf("exit %d without --caroot, want 4 (prerequisite missing): the machine default root holds no CA, so install.mkcert must refuse; envelope %+v", code, res)
	}
	if res.Error == nil || !strings.Contains(res.Error.Message, "no mkcert root CA exists") {
		t.Fatalf("the refusal must be the missing-CA one this issue is about, got %+v", res.Error)
	}

	// When: the CAROOT travels.
	e2 := newFrontDoorEnv(t)
	if err := os.WriteFile(e2.stubMkcert, []byte(carootStubMkcert), 0o755); err != nil {
		t.Fatalf("write caroot stub: %v", err)
	}
	installerCaroot2 := filepath.Join(e2.home, ".memql", "mkcert")
	if err := os.MkdirAll(installerCaroot2, 0o755); err != nil {
		t.Fatalf("mkdir installer caroot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(installerCaroot2, "rootCA.pem"), []byte("installer-ca\n"), 0o644); err != nil {
		t.Fatalf("seed installer CA: %v", err)
	}

	res2, calls, code2 := e2.run(t, "--caroot="+installerCaroot2)
	if code2 != 0 {
		t.Fatalf("exit %d with --caroot=%s, want 0: the CA the install created is right there; envelope %+v\nstderr:\n%s",
			code2, installerCaroot2, res2, e2.lastStderr)
	}
	if res2.Result.FrontDoorTLSSource != "issued" {
		t.Errorf("frontDoorTlsSource = %q, want issued", res2.Result.FrontDoorTLSSource)
	}
	if call := tlsSecretCall(calls); call == "" {
		t.Errorf("no front-door TLS secret was seeded; kubectl calls:\n%s", strings.Join(calls, "\n"))
	}

	// Then: the certificate was signed under the INSTALL'S root. Asserted on
	// what mkcert was actually invoked with, because the envelope cannot tell a
	// certificate issued from the right CA from one issued by whichever CA
	// happened to be lying around -- which is the silent failure mode.
	log := e2.mkcertLog(t)
	issued := ""
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "-cert-file") {
			issued = line
		}
	}
	if issued == "" {
		t.Fatalf("mkcert was never asked to issue a certificate; stub log:\n%s", log)
	}
	if !strings.Contains(issued, "CAROOT="+installerCaroot2) {
		t.Errorf("the front-door certificate was issued under %q, not the install's own CA root %q.\n"+
			"On a machine where the default root happens to hold a CA this succeeds and says nothing --\n"+
			"the cluster then serves a certificate from a CA the installer neither created nor removes.\n"+
			"stub log:\n%s", issued, installerCaroot2, log)
	}
}
