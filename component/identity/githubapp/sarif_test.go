package githubapp

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const sarifPath = "/repos/acme/widget/code-scanning/sarifs"
const sarifID = "47177e22-5596-11eb-80a1-c1e54ef945c6"
const sarifDocument = `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"fixture"}},"results":[]}]}`

func testAnalysis() SARIFAnalysis {
	return SARIFAnalysis{CommitSHA: headSHA, Ref: "refs/heads/main", CheckoutURI: "file:///workspace/", ToolName: "fixture", StartedAt: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC), SARIF: []byte(sarifDocument)}
}

func TestSARIFUploadUsesPinnedInputsAndRetainsAcceptedIdentity(t *testing.T) {
	now := time.Now()
	hub := newHub().json(sarifPath, 202, `{"id":"`+sarifID+`","url":"https://untrusted.invalid/do-not-follow"}`)
	c := testClient(t, hub, &now)
	analysis := testAnalysis()
	upload, err := c.UploadSARIF(t.Context(), installToken, "acme", "widget", analysis)
	if err != nil || upload.ID != sarifID {
		t.Fatalf("upload: %+v %v", upload, err)
	}
	body := sentJSON(t, hub, 0)
	if body["commit_sha"] != headSHA || body["ref"] != analysis.Ref || body["checkout_uri"] != analysis.CheckoutURI || body["tool_name"] != analysis.ToolName || body["started_at"] != "2026-10-06T12:00:00Z" || body["validate"] != true {
		t.Fatalf("incorrect analysis metadata: %+v", body)
	}
	assertSARIFWire(t, body["sarif"].(string), analysis.SARIF, upload.SARIFSHA256)
	if len(hub.seen()) != 1 || hub.seen()[0].Method != http.MethodPost || hub.seen()[0].Header.Get("Authorization") != "Bearer "+installToken {
		t.Fatal("upload retried, followed a response URL, or lost its scoped credential")
	}
}

func assertSARIFWire(t *testing.T, encoded string, original []byte, digest string) {
	t.Helper()
	compressed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	r, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	actual, err := io.ReadAll(io.LimitReader(r, MaxSARIFBytes+1))
	if err != nil || !bytes.Equal(actual, original) {
		t.Fatal("submitted SARIF differs from evidence")
	}
	sum := sha256.Sum256(actual)
	if hex.EncodeToString(sum[:]) != digest {
		t.Fatal("receipt digest differs from submitted evidence")
	}
}

func TestSARIFUploadDoesNotRetryAmbiguousOrRejectedWrites(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		uncertain bool
	}{
		{"server failure", 503, `{}`, true},
		{"timeout", 408, `{}`, true},
		{"missing accepted id", 202, `{}`, true},
		{"invalid accepted id", 202, `{"id":"../other"}`, true},
		{"unreadable accepted reply", 202, `{`, true},
		{"unexpected success", 200, `{"id":"` + sarifID + `"}`, true},
		{"permission refused", 403, `{"message":"private response text"}`, false},
		{"rate limited", 429, `{}`, false},
		{"too large", 413, `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			hub := newHub().json(sarifPath, tc.status, tc.body)
			c := testClient(t, hub, &now)
			upload, err := c.UploadSARIF(t.Context(), installToken, "acme", "widget", testAnalysis())
			if err == nil || errors.Is(err, ErrSARIFUploadUncertain) != tc.uncertain || upload.ID != "" || upload.SARIFSHA256 == "" || len(hub.seen()) != 1 {
				t.Fatalf("upload=%+v err=%v calls=%d", upload, err, len(hub.seen()))
			}
			if strings.Contains(err.Error(), "private response text") || strings.Contains(err.Error(), installToken) {
				t.Fatal("error exposed response body or credential")
			}
		})
	}
}

func TestSARIFUploadRefusesInvalidEvidenceBeforeSending(t *testing.T) {
	for _, invalid := range []string{"sha", "short ref", "ref traversal", "empty branch", "ref control", "url", "version", "empty runs", "null run", "invalid json", "raw limit", "compressed limit"} {
		t.Run(invalid, func(t *testing.T) {
			analysis := testAnalysis()
			switch invalid {
			case "sha":
				analysis.CommitSHA = "main"
			case "short ref":
				analysis.Ref = "main"
			case "ref traversal":
				analysis.Ref = "refs/heads/../main"
			case "empty branch":
				analysis.Ref = "refs/heads/"
			case "ref control":
				analysis.Ref = "refs/heads/a\x01b"
			case "url":
				analysis.CheckoutURI = "https://untrusted.invalid/"
			case "version":
				analysis.SARIF = []byte(`{"version":"1.0.0","runs":[{}]}`)
			case "empty runs":
				analysis.SARIF = []byte(`{"version":"2.1.0","runs":[]}`)
			case "null run":
				analysis.SARIF = []byte(`{"version":"2.1.0","runs":[null]}`)
			case "invalid json":
				analysis.SARIF = []byte(sarifDocument + "{}")
			case "raw limit":
				analysis.SARIF = make([]byte, MaxSARIFBytes+1)
			case "compressed limit":
				data := make([]byte, 11<<20)
				r := rand.NewChaCha8([32]byte{42})
				if _, err := r.Read(data); err != nil {
					t.Fatal(err)
				}
				analysis.SARIF = []byte(`{"version":"2.1.0","runs":[{"properties":{"fixture":"` + base64.StdEncoding.EncodeToString(data) + `"}}]}`)
			}
			now := time.Now()
			hub := newHub()
			c := testClient(t, hub, &now)
			if _, err := c.UploadSARIF(t.Context(), installToken, "acme", "widget", analysis); err == nil || len(hub.seen()) != 0 {
				t.Fatalf("invalid evidence reached upload: %v", err)
			}
		})
	}
}

func TestSARIFProcessingNeverConfusesPendingAndComplete(t *testing.T) {
	for _, tc := range []struct {
		body, want string
		invalid    bool
	}{
		{`{"processing_status":"pending"}`, "pending", false},
		{`{"processing_status":"complete"}`, "complete", false},
		{`{"processing_status":"failed","errors":["validation failed"]}`, "failed", false},
		{`{"processing_status":"complete","errors":["contradiction"]}`, "", true},
		{`{}`, "", true},
		{`{"processing_status":"unknown"}`, "", true},
	} {
		now := time.Now()
		hub := newHub().json(sarifPath+"/"+sarifID, 200, tc.body)
		c := testClient(t, hub, &now)
		state, err := c.SARIFUploadStatus(t.Context(), installToken, "acme", "widget", sarifID)
		if (err != nil) != tc.invalid || state.Status != tc.want || len(hub.seen()) != 1 || hub.seen()[0].Method != http.MethodGet {
			t.Fatalf("body=%s state=%+v err=%v", tc.body, state, err)
		}
	}
}

func TestSARIFNeverFollowsHTTPRedirects(t *testing.T) {
	now := time.Now()
	hub := newHub().redirect(sarifPath, 307, "https://other.github.test/steal")
	c := testClient(t, hub, &now)
	if _, err := c.UploadSARIF(t.Context(), installToken, "acme", "widget", testAnalysis()); !errors.Is(err, ErrSARIFUploadUncertain) {
		t.Fatalf("redirected upload: %v", err)
	}
	if len(hub.seen()) != 1 {
		t.Fatal("upload followed a redirect with its analysis or bearer")
	}
	hub.redirect(sarifPath+"/"+sarifID, 302, "https://other.github.test/steal")
	if _, err := c.SARIFUploadStatus(t.Context(), installToken, "acme", "widget", sarifID); err == nil || len(hub.seen()) != 2 {
		t.Fatal("status read followed a redirect")
	}
}

func TestSARIFInputsCannotChangeTheRepositoryEndpoint(t *testing.T) {
	now := time.Now()
	hub := newHub()
	c := testClient(t, hub, &now)
	for _, pair := range [][2]string{{"..", "widget"}, {"acme", "../private"}, {"", "widget"}, {"acme?token=x", "widget"}} {
		if _, err := c.UploadSARIF(t.Context(), installToken, pair[0], pair[1], testAnalysis()); err == nil {
			t.Fatal("invalid repository accepted")
		}
	}
	for _, id := range []string{"", "../other", "id?token=x"} {
		if _, err := c.SARIFUploadStatus(t.Context(), installToken, "acme", "widget", id); err == nil {
			t.Fatal("invalid upload id accepted")
		}
	}
	if len(hub.seen()) != 0 {
		t.Fatal("invalid endpoint reached network")
	}
	for _, ref := range []string{"refs/heads/a/b", "refs/tags/v1.0.0", "refs/pull/42/head", "refs/pull/42/merge"} {
		if !validSARIFRef(ref) {
			t.Fatalf("valid ref refused: %s", ref)
		}
	}
}

// Optional wire rehearsal uses a real local scanner report, but sends it only
// to the in-process fake. It never uploads local evidence to GitHub.
func TestSARIFRealReportWireRehearsal(t *testing.T) {
	path := os.Getenv("MEMQL_SARIF_REHEARSAL_FILE")
	if path == "" {
		t.Skip("set MEMQL_SARIF_REHEARSAL_FILE for a local scanner report")
	}
	report, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	hub := newHub().json(sarifPath, 202, `{"id":"`+sarifID+`"}`)
	c := testClient(t, hub, &now)
	analysis := testAnalysis()
	analysis.SARIF = report
	upload, err := c.UploadSARIF(context.Background(), installToken, "acme", "widget", analysis)
	if err != nil {
		t.Fatal(err)
	}
	assertSARIFWire(t, sentJSON(t, hub, 0)["sarif"].(string), report, upload.SARIFSHA256)
}
