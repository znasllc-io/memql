package imageverify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sampleRequest() Request {
	return Request{Context: ".", Dockerfile: "Dockerfile", Target: "verify", Args: map[string]string{"MEMQL_COMMIT": strings.Repeat("a", 40), "MEMQL_RELEASE": ""}, VerificationOnly: true}
}
func TestRequestBindsSourceAndRejectsHostInputs(t *testing.T) {
	q := sampleRequest()
	sha := q.Args["MEMQL_COMMIT"]
	if err := q.Validate(sha, "linux/amd64"); err != nil {
		t.Fatal(err)
	}
	for label, change := range map[string]func(*Request){"mode": func(q *Request) { q.VerificationOnly = false }, "absolute": func(q *Request) { q.Context = "/home" }, "escape": func(q *Request) { q.Dockerfile = "../Dockerfile" }, "target option": func(q *Request) { q.Target = "--allow=security.insecure" }, "source": func(q *Request) { q.Args["MEMQL_COMMIT"] = "wrong" }, "missing provenance": func(q *Request) { delete(q.Args, "MEMQL_RELEASE") }, "unbounded args": func(q *Request) { q.Args["DATA"] = strings.Repeat("x", MaxMetadataBytes) }} {
		t.Run(label, func(t *testing.T) {
			x := sampleRequest()
			change(&x)
			if x.Validate(sha, "linux/amd64") == nil {
				t.Fatal("unsafe request accepted")
			}
		})
	}
	if q.Validate(sha, "darwin/arm64") == nil {
		t.Fatal("unsupported platform accepted")
	}
}
func TestSourcePathRefusesSymlinkAndTraversal(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := sourcePath(root, "Dockerfile", false); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := sourcePath(root, "escape", true); err == nil {
		t.Fatal("host directory symlink accepted")
	}
	if _, err := sourcePath(root, "Dockerfile", true); err == nil {
		t.Fatal("regular file accepted as context")
	}
}
func TestCgroupEvidenceRequiresAggregateEnforcedLimits(t *testing.T) {
	limits := Limits{CPUMilli: 1500, MemoryMiB: 512, MinFreeBytes: 1 << 30, Timeout: time.Minute}
	good := map[string]string{"cpu.max": "150000 100000", "memory.max": "536870912", "memory.swap.max": "0", "pids.max": "512"}
	if err := checkCgroupLimits(good, limits); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"cpu.max": "max 100000", "memory.max": "1073741824", "memory.swap.max": "1", "pids.max": "max"} {
		t.Run(key, func(t *testing.T) {
			bad := map[string]string{}
			for k, v := range good {
				bad[k] = v
			}
			bad[key] = value
			if checkCgroupLimits(bad, limits) == nil {
				t.Fatal("unenforced limit accepted")
			}
		})
	}
	for _, v := range []string{"0::/a\n1:cpu:/a", "1:cpu:/a", "0::relative"} {
		if _, err := parseUnifiedCgroup(v); err == nil {
			t.Fatal("non-v2 membership accepted")
		}
	}
}
func TestReceiptHashesActualExportAndBindsRequest(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "output"), 0700); err != nil {
		t.Fatal(err)
	}
	image := []byte("actual exported archive bytes")
	if err := os.WriteFile(filepath.Join(root, "output/image.oci.tar"), image, 0600); err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(root, "output/metadata.json")
	if err := os.WriteFile(metadata, []byte(`{"containerimage.digest":"sha256:retained"}`), 0600); err != nil {
		t.Fatal(err)
	}
	q := sampleRequest()
	raw, err := buildReceipt(Record{Root: root}, q, q.Args["MEMQL_COMMIT"])
	if err != nil {
		t.Fatal(err)
	}
	var got Receipt
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(image)
	if got.OCISHA256 != hex.EncodeToString(digest[:]) || got.OCIBytes != int64(len(image)) || got.SourceSHA != q.Args["MEMQL_COMMIT"] || !got.SuccessfulBuild {
		t.Fatal("receipt did not bind actual export")
	}
	if err = os.WriteFile(metadata, []byte(`[]`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = buildReceipt(Record{Root: root}, q, q.Args["MEMQL_COMMIT"]); err == nil {
		t.Fatal("invalid metadata accepted")
	}
	if err = os.Remove(filepath.Join(root, "output/image.oci.tar")); err != nil {
		t.Fatal(err)
	}
	if _, err = buildReceipt(Record{Root: root}, q, q.Args["MEMQL_COMMIT"]); err == nil {
		t.Fatal("missing export accepted")
	}
}
func attemptFixture(t *testing.T) Record {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "iv-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return Record{ID: strings.Repeat("a", 32), Root: filepath.Join(root, strings.Repeat("a", 32)), Workspace: "/workspace/step-exact", AttemptSHA256: strings.Repeat("b", 64), SourceSHA: strings.Repeat("c", 40), Identity: HelloIdentity{OwnerUID: os.Geteuid(), RequestSHA256: strings.Repeat("b", 64)}}
}
func TestCleanupRetainsOriginalMarkerAcrossLostReplies(t *testing.T) {
	r := attemptFixture(t)
	if err := createAttempt(ServiceConfig{}, r, serviceRequest{}); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{strings.Repeat("d", 64), strings.Repeat("e", 64)} {
		r.Identity.RequestSHA256 = hash
		if err := prepareCleanupMarker(r); err != nil {
			t.Fatalf("cleanup lost original marker identity: %v", err)
		}
	}
	wrong := r
	wrong.AttemptSHA256 = strings.Repeat("f", 64)
	if prepareCleanupMarker(wrong) == nil {
		t.Fatal("another original request accepted")
	}
}
func TestPartialPreparationIsRecoverableButUnknownStateRefused(t *testing.T) {
	for _, stage := range []string{"empty", "partial", "complete pending", "foreign", "symlink", "unknown"} {
		t.Run(stage, func(t *testing.T) {
			r := attemptFixture(t)
			if err := os.Mkdir(r.Root, 0700); err != nil {
				t.Fatal(err)
			}
			pending := filepath.Join(r.Root, ".attempt.json.pending")
			expected := markerBytes(r)
			switch stage {
			case "partial":
				if err := os.WriteFile(pending, expected[:17], 0600); err != nil {
					t.Fatal(err)
				}
			case "complete pending":
				if err := os.WriteFile(pending, expected, 0600); err != nil {
					t.Fatal(err)
				}
			case "foreign":
				if err := os.WriteFile(pending, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(t.TempDir(), pending); err != nil {
					t.Fatal(err)
				}
			case "unknown":
				if err := os.WriteFile(filepath.Join(r.Root, "foreign"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := prepareCleanupMarker(r)
			safe := stage == "empty" || stage == "partial" || stage == "complete pending"
			if safe && err != nil {
				t.Fatal(err)
			}
			if !safe && err == nil {
				t.Fatal("unknown partial state accepted")
			}
			if safe && checkAttemptMarker(r) != nil {
				t.Fatal("marker was not atomically restored")
			}
		})
	}
}
