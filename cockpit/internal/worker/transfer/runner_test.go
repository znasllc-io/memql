package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

type controlFixture struct {
	mu             sync.Mutex
	job            Job
	chunks         map[int][]byte
	hashes         map[int]string
	server         *httptest.Server
	dropCheckpoint bool
	puts           map[int]int
	data           []byte
	failDownload   int
	corrupt        bool
	pause          bool
	afterReport    func()
}

func fixtureRunner(t *testing.T, data []byte) (*Runner, *controlFixture) {
	t.Helper()
	h := sha256.Sum256(data)
	c := &controlFixture{job: Job{ID: "backup-001", WorkerID: "worker-001", Kind: "archive", State: "checking", Desired: "running", Size: int64(len(data)), SHA256: hex.EncodeToString(h[:]), ETag: `"v1"`}, chunks: map[int][]byte{}, hashes: map[int]string{}, puts: map[int]int{}, data: data, failDownload: -1}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		n, _ := strconv.Atoi(r.URL.Query().Get("block"))
		if r.Method == http.MethodPut {
			b, e := io.ReadAll(r.Body)
			if e != nil {
				t.Error(e)
			}
			if checksum(b).SHA256 != c.hashes[n] {
				http.Error(w, "checksum", 400)
				return
			}
			c.chunks[n] = b
			c.puts[n]++
			w.WriteHeader(201)
			return
		}
		if n == c.failDownload {
			c.failDownload = -1
			http.Error(w, "interrupted", 503)
			return
		}
		start := n * 4
		end := min(start+4, len(c.data))
		b := append([]byte(nil), c.data[start:end]...)
		if c.corrupt && len(b) > 0 {
			b[0] ^= 1
		}
		w.Header().Set("ETag", c.job.ETag)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(c.data)))
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.WriteHeader(206)
		w.Write(b)
	}))
	t.Cleanup(c.server.Close)
	r := &Runner{Control: c, StateDir: t.TempDir(), HTTP: c.server.Client(), CheckRead: func(string) error { return nil }, CheckWrite: func(string) error { return nil }, testBlockBytes: 4}
	return r, c
}
func (c *controlFixture) Call(_ context.Context, op string, a map[string]any, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var result any = c.job
	switch op {
	case "Read":
	case "Create":
	case "Prepare":
		c.job.State = "uploading"
		result = c.job
	case "Control":
		if a["action"] == "resume" {
			c.job.Desired = "running"
			if c.job.Kind == "download" {
				c.job.State = "downloading"
			} else {
				c.job.State = "preparing"
			}
		}
		result = c.job
	case "Block":
		if c.job.Desired != "running" {
			return ErrPaused
		}
		n := a["ordinal"].(int)
		c.hashes[n] = a["sha256"].(string)
		result = Grant{Method: "PUT", URL: fmt.Sprintf("%s?block=%d", c.server.URL, n), Headers: map[string]string{}, ExpiresAt: time.Now().Add(time.Minute)}
	case "Checkpoint":
		old := c.job.Committed
		for n := 0; ; n++ {
			b, ok := c.chunks[n]
			if !ok {
				break
			}
			if int64(n*4) < c.job.Committed {
				continue
			}
			c.job.Committed += int64(len(b))
		}
		if c.dropCheckpoint && c.job.Committed > old {
			c.dropCheckpoint = false
			return errors.New("reply lost after durable commit")
		}
		if final, _ := a["final"].(bool); final {
			c.job.State = "complete"
		}
		if c.pause && c.job.Committed > 0 {
			c.job.Desired = "paused"
			c.job.State = "paused"
			c.pause = false
		}
		result = c.job
	case "DownloadBlock":
		n := int(a["ordinal"].(int64))
		start := n * 4
		end := min(start+4, len(c.data))
		sum := checksum(c.data[start:end])
		result = DownloadBlock{Request: Grant{Method: "GET", URL: fmt.Sprintf("%s?block=%d", c.server.URL, n), ExpiresAt: time.Now().Add(time.Minute)}, SHA256: sum.SHA256, Offset: int64(start), Size: int64(end - start)}
	case "Report":
		c.job.State = a["state"].(string)
		c.job.Committed = a["offset"].(int64)
		result = c.job
		if c.afterReport != nil {
			c.afterReport()
			c.afterReport = nil
			return errors.New("report reply lost")
		}
	default:
		return fmt.Errorf("unexpected operation %s", op)
	}
	if out == nil {
		return nil
	}
	b, e := json.Marshal(result)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, out)
}
func sourceFixture(t *testing.T, c *controlFixture, data []byte) {
	t.Helper()
	c.job.Path = filepath.Join(t.TempDir(), "backup.zip")
	if e := os.WriteFile(c.job.Path, data, 0600); e != nil {
		t.Fatal(e)
	}
}
func TestUploadLostCommitReplyAndProcessRestart(t *testing.T) {
	data := []byte("small-fixture-data")
	r, c := fixtureRunner(t, data)
	sourceFixture(t, c, data)
	c.dropCheckpoint = true
	if _, e := r.Run(context.Background(), c.job); e == nil {
		t.Fatal("expected simulated lost reply")
	}
	restarted := *r
	j, e := restarted.Run(context.Background(), c.job)
	if e != nil {
		t.Fatal(e)
	}
	if j.State != "complete" {
		t.Fatal(j.State)
	}
	if c.puts[0] != 1 {
		t.Fatalf("durable prefix resent %d times", c.puts[0])
	}
	var got []byte
	for n := 0; n < len(c.chunks); n++ {
		got = append(got, c.chunks[n]...)
	}
	if string(got) != string(data) {
		t.Fatalf("recovered bytes %q", got)
	}
}
func TestUploadPauseResumeAndChangedSource(t *testing.T) {
	data := []byte("abcdefghijkl")
	r, c := fixtureRunner(t, data)
	sourceFixture(t, c, data)
	c.pause = true
	_, e := r.Run(context.Background(), c.job)
	if e == nil {
		t.Fatal("paused upload continued")
	}
	if len(c.puts) != 1 {
		t.Fatal("pause allowed another block")
	}
	j, e := r.Action(context.Background(), c.job.ID, "resume")
	if e != nil {
		t.Fatal(e)
	}
	j, e = r.Run(context.Background(), j)
	if e != nil {
		t.Fatal(e)
	}
	if j.State != "complete" {
		t.Fatal(j)
	}
	// A cached manifest cannot make changed bytes look like the old source.
	r, c = fixtureRunner(t, data)
	sourceFixture(t, c, data)
	f, _, e := r.source(context.Background(), c.job)
	if e != nil {
		t.Fatal(e)
	}
	f.Close()
	info, _ := os.Stat(c.job.Path)
	if e = os.WriteFile(c.job.Path, []byte("XXXXXXXXXXXX"), 0600); e != nil {
		t.Fatal(e)
	}
	os.Chtimes(c.job.Path, info.ModTime(), info.ModTime())
	if _, e = r.Run(context.Background(), c.job); e == nil {
		t.Fatal("changed source accepted")
	}
	if len(c.puts) != 0 {
		t.Fatal("changed bytes uploaded")
	}
}
func TestDownloadInterruptedCorruptPrefixRecoversAndNeverOverwrites(t *testing.T) {
	data := []byte("tiny-download-fixture")
	r, c := fixtureRunner(t, data)
	c.job.Kind = "download"
	c.job.State = "downloading"
	c.job.Path = filepath.Join(t.TempDir(), "restored.zip")
	c.failDownload = 1
	if _, e := r.Run(context.Background(), c.job); e == nil {
		t.Fatal("expected interruption")
	}
	if _, e := os.Stat(c.job.Path); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("incomplete file exposed")
	}
	journal, e := readCheckpoint(filepath.Join(r.StateDir, "transfers", c.job.ID+".download.json"))
	if e != nil {
		t.Fatal(e)
	}
	var checkpoint downloadState
	if e = json.Unmarshal(journal, &checkpoint); e != nil {
		t.Fatal(e)
	}
	partial := filepath.Join(filepath.Dir(c.job.Path), checkpoint.Partial)
	f, e := os.OpenFile(partial, os.O_WRONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	f.WriteAt([]byte("bad!"), 0)
	f.Close()
	restarted := *r
	j, e := restarted.Run(context.Background(), c.job)
	if e != nil {
		t.Fatal(e)
	}
	if j.State != "complete" {
		t.Fatal(j)
	}
	got, e := os.ReadFile(c.job.Path)
	if e != nil || string(got) != string(data) {
		t.Fatalf("download=%q %v", got, e)
	}
	r2, c2 := fixtureRunner(t, data)
	c2.job.Kind = "download"
	c2.job.State = "downloading"
	c2.job.Path = c.job.Path
	if _, e = r2.Run(context.Background(), c2.job); e == nil {
		t.Fatal("existing destination overwritten")
	}
	got, _ = os.ReadFile(c.job.Path)
	if string(got) != string(data) {
		t.Fatal("destination changed")
	}
}
func TestDownloadRejectsChecksumAndRecoversLostCompletion(t *testing.T) {
	data := []byte("eight888")
	r, c := fixtureRunner(t, data)
	c.job.Kind = "download"
	c.job.State = "downloading"
	c.job.Path = filepath.Join(t.TempDir(), "copy.zip")
	c.corrupt = true
	if _, e := r.Run(context.Background(), c.job); e == nil {
		t.Fatal("corrupt response accepted")
	}
	if _, e := os.Stat(c.job.Path); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("corrupt final exposed")
	}
	c.corrupt = false
	j, e := r.Run(context.Background(), c.job)
	if e != nil {
		t.Fatal(e)
	}
	c.job.State = "downloading"
	j, e = r.Run(context.Background(), c.job)
	if e != nil || j.State != "complete" {
		t.Fatalf("lost completion: %+v %v", j, e)
	}
}
func TestPathsRequireSeparateLocalConsent(t *testing.T) {
	r, c := fixtureRunner(t, []byte("data"))
	sourceFixture(t, c, []byte("data"))
	r.CheckRead = nil
	if _, e := r.Run(context.Background(), c.job); e == nil {
		t.Fatal("unconsented read")
	}
	c.job.Kind = "download"
	c.job.State = "downloading"
	r.CheckWrite = nil
	if _, e := r.Run(context.Background(), c.job); e == nil {
		t.Fatal("read permission became write permission")
	}
}

func TestLocalCheckpointCannotHaveTwoWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.lock")
	release, err := LockLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := LockLocal(path); err == nil {
		second()
		t.Fatal("two writers entered the same journal")
	}
	release()
	recovered, err := LockLocal(path)
	if err != nil {
		t.Fatal("lock survived its writer", err)
	}
	recovered()
}

func TestDownloadRecoversBeforeIdentityReceiptAndAfterMissingPartial(t *testing.T) {
	for _, stage := range []string{"intent-only", "missing-partial"} {
		t.Run(stage, func(t *testing.T) {
			data := []byte("tiny-recovery")
			r, c := fixtureRunner(t, data)
			c.job.Kind = "download"
			c.job.State = "downloading"
			c.job.Path = filepath.Join(t.TempDir(), "restored.zip")
			c.failDownload = 1
			if _, err := r.Run(context.Background(), c.job); err == nil {
				t.Fatal("expected network interruption")
			}
			journal := filepath.Join(r.StateDir, "transfers", c.job.ID+".download.json")
			raw, err := readCheckpoint(journal)
			if err != nil {
				t.Fatal(err)
			}
			var state downloadState
			if err = json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			partial := filepath.Join(filepath.Dir(c.job.Path), state.Partial)
			if stage == "intent-only" {
				state.Identity = ""
				state.Chunks = nil
				raw, _ = json.Marshal(state)
				if err = atomicFile(journal, raw); err != nil {
					t.Fatal(err)
				}
			} else if err = os.Remove(partial); err != nil {
				t.Fatal(err)
			}
			restarted := *r
			j, err := restarted.Run(context.Background(), c.job)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(c.job.Path)
			if err != nil || string(got) != string(data) || j.State != "complete" {
				t.Fatal("recovery did not produce exact bytes", err)
			}
		})
	}
}

func TestBatchedUploadRecoversAllStagedBlocksAfterLostCheckpoint(t *testing.T) {
	data := []byte("sixteen-byte-fixture")
	r, c := fixtureRunner(t, data)
	sourceFixture(t, c, data)
	c.job.CheckpointBlocks = 2
	c.dropCheckpoint = true
	if _, err := r.Run(context.Background(), c.job); err == nil {
		t.Fatal("expected lost checkpoint reply")
	}
	if len(c.puts) != 2 {
		t.Fatalf("batch did not stop at its checkpoint window: %d", len(c.puts))
	}
	restarted := *r
	j, err := restarted.Run(context.Background(), c.job)
	if err != nil || j.State != "complete" {
		t.Fatal("batch recovery failed", err)
	}
	for n, count := range c.puts {
		if count != 1 {
			t.Fatalf("block %d was resent %d times", n, count)
		}
	}
}
