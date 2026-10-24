package worker

import (
	"bytes"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/consent"
)

// The sentences below are the whole product for a person at a terminal,
// so each is asserted on its exact words -- changing one is then a thing
// somebody does on purpose.

func TestUnpairSentences(t *testing.T) {
	on := true
	survivor := Home{ID: "local", ClusterURL: "https://api.local.example", Token: "mql_wkr_local_aaaaaaaaaaaa", Enabled: &on}
	gone := Home{ID: "production", ClusterURL: "https://api.prod.example", Token: "mql_wkr_prod_aaaaaaaaaaaa"}

	got := unpairSentences(RemoveHomeResult{
		Workers: WorkersFile{Homes: []Home{survivor}}, Removed: gone, Mirror: MirrorRewritten, MirrorHome: "local",
	}, false, "/home/me/.memql/worker.yaml", "darwin")
	want := []string{
		`Removed home "production" (1 home left, 1 enabled).`,
		`The legacy mirror /home/me/.memql/worker.yaml now names "local".`,
		"The running worker keeps its stream to production until it restarts:",
		"  launchctl kickstart -k gui/$(id -u)/com.znasllc.memql-worker",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	got = unpairSentences(RemoveHomeResult{Removed: gone, Mirror: MirrorDeleted}, false, "/home/me/.memql/worker.yaml", "linux")
	want = []string{
		`Removed home "production". It was the last one.`,
		"Deleted the legacy mirror /home/me/.memql/worker.yaml: no enabled home is left for it to name, so no token stays behind in it.",
		"The running worker keeps its stream to production until it restarts:",
		"  systemctl --user restart memql-worker",
		"With no cluster enabled it then waits, connected to nothing. After you pair one (memql worker pair <code>), restart it the same way.",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	off := false
	gone.Enabled = &off
	got = unpairSentences(RemoveHomeResult{Workers: WorkersFile{Homes: []Home{gone, survivor}}, Removed: gone, Mirror: MirrorUntouched},
		true, "/home/me/.memql/worker.yaml", "linux")
	if got[0] != `Disabled home "production" (2 homes left, 1 enabled).` || len(got) != 3 {
		t.Fatalf("disable:\n%s", strings.Join(got, "\n"))
	}
}

func TestNoEnabledHomeSentence(t *testing.T) {
	path := "/home/me/.memql/workers.yaml"
	if got, want := noEnabledHomeSentence(path, WorkersFile{}),
		"No cluster is paired with this machine (/home/me/.memql/workers.yaml has no homes). Pair one with a code from the portal: memql worker pair <code>"; got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	off := false
	w := WorkersFile{Homes: []Home{{ID: "prod", Enabled: &off}, {ID: "local", Enabled: &off}}}
	if got, want := noEnabledHomeSentence(path, w),
		"Every cluster in /home/me/.memql/workers.yaml is disabled (prod, local). Set enabled: true on one, or pair another with memql worker pair <code>, then restart the worker."; got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

// fixedStatus is a window opened for an hour, twenty minutes ago.
func fixedStatus(now time.Time, strict bool) consent.Status {
	return consent.Status{Granted: true, ExpiresAt: now.Add(40 * time.Minute), Window: time.Hour, Strict: strict}
}

func TestRenderConsentStatus_OneCluster(t *testing.T) {
	now := time.Date(2026, 9, 13, 14, 0, 0, 0, time.Local)
	st := fixedStatus(now, true)
	got := renderConsentStatus(consent.Response{OK: true, Home: "production", Status: st,
		Homes: []consent.HomeStatus{{Home: "production", Status: st}}}, now)
	want := "Approved directory reads for production: available with a matching Nexus approval\n" +
		"Consent for production: GRANTED\n" +
		"  Expires at:   " + st.ExpiresAt.Format(time.RFC3339) + "\n" +
		"  Remaining:    40m0s\n" +
		"  Window:       1h0m0s\n" +
		"  Strict mode:  ON\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}

	got = renderConsentStatus(consent.Response{OK: true, Homes: []consent.HomeStatus{{Home: "production"}}}, now)
	want = "Approved directory reads for production: available with a matching Nexus approval\nConsent for production: NOT GRANTED\nRun `memql worker consent grant --window=<duration>` to open a window.\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderConsentStatus_SeveralClusters(t *testing.T) {
	now := time.Date(2026, 9, 13, 14, 0, 0, 0, time.Local)
	st := fixedStatus(now, false)
	got := renderConsentStatus(consent.Response{OK: true, Homes: []consent.HomeStatus{
		{Home: "local"},
		{Home: "production", Status: st},
	}}, now)
	want := "Consent is per cluster. This machine serves 2:\n\n" +
		"  CLUSTER      CONSENT       EXPIRES    REMAINING   STRICT\n" +
		"  local        not granted\n" +
		"  production   GRANTED       " + st.ExpiresAt.Format("15:04:05") + "   40m         off\n" +
		"\n" +
		"Approved directory reads for local: available with a matching Nexus approval\n" +
		"Approved directory reads for production: available with a matching Nexus approval\n" +
		"Open a window:  memql worker consent grant --window=<duration> --cluster <cluster>\n" +
		"Close them all: memql worker consent revoke\n"
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}

	// Naming one of several keeps the single-cluster block, with the
	// command that works on a machine that serves several.
	got = renderConsentStatus(consent.Response{OK: true, Home: "local", Homes: []consent.HomeStatus{
		{Home: "local"}, {Home: "production", Status: st},
	}}, now)
	if !strings.Contains(got, "Consent for local: NOT GRANTED") ||
		!strings.Contains(got, "`memql worker consent grant --window=<duration> --cluster local`") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestRenderConsentStatusShowsLocalDirectoryStop(t *testing.T) {
	resp := consent.Response{OK: true, Home: "cluster-a", Status: consent.Status{DirectoryReadsPaused: true}}
	got := renderConsentStatus(resp, time.Now())
	if !strings.Contains(got, "Approved directory reads for cluster-a: paused on this machine") || strings.Contains(got, "available with") {
		t.Fatalf("local stop hidden: %s", got)
	}
}

// A grant that needed a cluster answers with the commands that would have
// worked -- one per cluster, with the window and flags the person typed.
func TestConsentRefusalListsACommandPerCluster(t *testing.T) {
	got := consentRefusal(consent.Response{Code: consent.CodeHomeRequired, Homes: []consent.HomeStatus{
		{Home: "local"}, {Home: "production"},
	}}, time.Hour, true)
	want := "This machine serves 2 clusters: local, production.\n" +
		"Consent is per cluster -- say which one this window is for:\n\n" +
		"  memql worker consent grant --window=1h --strict --cluster local\n" +
		"  memql worker consent grant --window=1h --strict --cluster production\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	got = consentRefusal(consent.Response{Code: consent.CodeUnknownHome, Homes: []consent.HomeStatus{{Home: "local"}}}, time.Hour, false)
	if got != "ERROR: no cluster by that id on this machine. It serves: local.\n" {
		t.Fatalf("unknown home: %q", got)
	}
}

// Revoking one cluster says which others are still open: "revoked" with
// no name reads as "this machine is locked", and it may not be.
func TestRevokeSentence(t *testing.T) {
	now := time.Date(2026, 9, 13, 14, 0, 0, 0, time.Local)
	st := fixedStatus(now, false)
	got := revokeSentence(consent.Response{OK: true, Home: "local", Homes: []consent.HomeStatus{
		{Home: "local"}, {Home: "production", Status: st},
	}}, now)
	want := "Consent revoked for local. Its tool calls will be denied until you grant a new window.\n" +
		"production still has a window open (40m left); memql worker consent revoke closes every window.\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	got = revokeSentence(consent.Response{OK: true, Homes: []consent.HomeStatus{{Home: "local"}, {Home: "production"}}}, now)
	if got != "Consent revoked on every cluster (local, production). Future worker tool calls will be denied until you grant a new window.\n" {
		t.Fatalf("revoke all: %q", got)
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Hour: "1h", 30 * time.Minute: "30m", 90 * time.Minute: "1h30m",
		45 * time.Second: "45s", 90 * time.Second: "1m30s", 24 * time.Hour: "24h",
		time.Hour + time.Second: "1h0m1s",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestDuplicateHomeLines(t *testing.T) {
	got := strings.Join(duplicateHomeLines("local", "api.example.com"), "\n")
	want := `NOT CONNECTED -- "api.example.com" points at the same cluster, and it connects instead.` + "\n" +
		"One machine holds one stream per cluster. Remove this home, or set enabled: false on it:\n" +
		"  memql worker unpair --cluster local"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// At a terminal, a worker with no enabled home says so and exits 1.
func TestWaitWithoutHomesAtATerminalExits(t *testing.T) {
	var stderr bytes.Buffer
	code := waitWithoutHomes(quietLogger(), &stderr, "/home/me/.memql/workers.yaml", WorkersFile{}, true, nil)
	if code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	if got, want := stderr.String(), "ERROR: "+noEnabledHomeSentence("/home/me/.memql/workers.yaml", WorkersFile{})+"\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

// Under a service it waits -- and a SIGHUP, which the model commands send
// every running worker to reload its policy, does not end the wait:
// systemd counts death by SIGHUP as a clean exit and would not restart it.
func TestWaitWithoutHomesUnderAServiceSurvivesSIGHUP(t *testing.T) {
	logs := &logBuffer{}
	logger := slogJSON(logs)
	signals := make(chan os.Signal, 2)
	signals <- syscall.SIGHUP
	signals <- syscall.SIGTERM
	code := waitWithoutHomes(logger, &bytes.Buffer{}, "/w.yaml", WorkersFile{}, false, signals)
	if code != 0 {
		t.Fatalf("exit code %d, want 0 after SIGTERM", code)
	}
	for _, want := range []string{
		"no cluster is enabled on this machine; this worker waits, connected to nothing, until it is restarted",
		"policy reload requested; nothing to reload while no cluster is enabled",
		`"signal":"terminated"`,
	} {
		if logs.count(want) != 1 {
			t.Errorf("log missing %q:\n%s", want, logs)
		}
	}
}

// Only the worker.yaml beside workers.yaml is the mirror anything here
// may rewrite or delete; a --config anywhere else is a person's file.
func TestIsMirrorOf(t *testing.T) {
	for _, tc := range []struct {
		config, workers string
		want            bool
	}{
		{"/home/me/.memql/worker.yaml", "/home/me/.memql/workers.yaml", true},
		{"/home/me/.memql/./worker.yaml", "/home/me/.memql/workers.yaml", true},
		{"/home/me/elsewhere/worker.yaml", "/home/me/.memql/workers.yaml", false},
		{"/home/me/.memql/handmade.yaml", "/home/me/.memql/workers.yaml", false},
		{"/home/me/.memql/worker.yaml", "/srv/custom/workers.yaml", false},
	} {
		if got := isMirrorOf(tc.config, tc.workers); got != tc.want {
			t.Errorf("isMirrorOf(%q, %q) = %v, want %v", tc.config, tc.workers, got, tc.want)
		}
	}
	if got := mirrorPathFor("/srv/custom/workers.yaml"); got != "/srv/custom/worker.yaml" {
		t.Errorf("mirrorPathFor = %q", got)
	}
}
