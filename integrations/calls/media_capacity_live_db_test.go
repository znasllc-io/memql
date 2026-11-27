//go:build darwin || linux

package calls

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/server"
)

// This is an isolated native-service fixture, not the served BFF. A temporary
// loopback handler uses the real native admission service; only its disposable
// native passes enter the Python generator's private stdin. It never installs a
// provider bypass or changes the served account/tester-code configuration.
type capacitySignalService struct {
	service *Service
	mu      sync.Mutex
	begin   []float64
	forward int
	denied  int
}

func (s *capacitySignalService) CheckMediaSignal(ctx context.Context, token string) error {
	return s.service.CheckMediaSignal(ctx, token)
}
func (s *capacitySignalService) BeginMediaSignal(ctx context.Context, token string) (*callrules.MediaSignalAdmission, error) {
	started := time.Now()
	lease, err := s.service.BeginMediaSignal(ctx, token)
	s.mu.Lock()
	s.begin = append(s.begin, time.Since(started).Seconds())
	if err != nil {
		s.denied++
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	forward := lease.Forward
	lease.Forward = func(ctx context.Context, run func(context.Context) error) error {
		err := forward(ctx, run)
		s.mu.Lock()
		s.forward++
		if err != nil {
			s.denied++
		}
		s.mu.Unlock()
		return err
	}
	return lease, nil
}
func (s *capacitySignalService) receipt() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	values := slices.Clone(s.begin)
	slices.Sort(values)
	var max, p95 float64
	if len(values) > 0 {
		max = values[len(values)-1]
		p95 = values[(len(values)*95-1)/100]
	}
	return map[string]any{"begin_count": len(values), "begin_max_seconds": max, "begin_p95_seconds": p95, "forward_count": s.forward, "denied_callback_count": s.denied}
}

type capacityPeer struct {
	Identity string `json:"identity"`
	Token    string `json:"token"`
}
type capacityRoom struct {
	Room  string         `json:"room"`
	Peers []capacityPeer `json:"peers"`
}
type capacityManifest struct {
	Case    string         `json:"case"`
	Origin  string         `json:"origin"`
	Rooms   []capacityRoom `json:"rooms"`
	Seconds int            `json:"seconds"`
}

func capacityUsage() (float64, float64) {
	var usage syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) != nil {
		return 0, 0
	}
	cpu := float64(usage.Utime.Sec+usage.Stime.Sec) + float64(usage.Utime.Usec+usage.Stime.Usec)/1e6
	rss := float64(usage.Maxrss) / 1024
	if runtime.GOOS == "darwin" {
		rss /= 1024
	}
	return cpu, rss
}

func capacityRead(ctx context.Context, scanner *bufio.Scanner) (map[string]any, error) {
	type response struct {
		value map[string]any
		err   error
	}
	done := make(chan response, 1)
	go func() {
		if !scanner.Scan() {
			done <- response{err: fmt.Errorf("capacity child closed")}
			return
		}
		var value map[string]any
		err := json.Unmarshal(scanner.Bytes(), &value)
		done <- response{value, err}
	}()
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("capacity child deadline")
	case out := <-done:
		return out.value, out.err
	}
}

func capacityNativeClear(ctx context.Context, cw *callWorld, rooms []capacityRoom) bool {
	deadline, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		clear := true
		for _, room := range rooms {
			status, err := cw.svc.ReconcileMediaSignals(deadline, room.Room)
			clear = clear && err == nil && status.State == "clear"
		}
		if clear {
			return true
		}
		select {
		case <-deadline.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func TestMediaSignalLiveNativeCapacity(t *testing.T) {
	if os.Getenv("MEMQL_TEST_MEDIA_CAPACITY_LOCAL") != "1" {
		t.Skip("opt in to bounded native admission capacity fixture")
	}
	credential, control := nativeLocalSignalOptIn(t)
	python, script, output := os.Getenv("MEMQL_TEST_MEDIA_CAPACITY_PYTHON"), os.Getenv("MEMQL_TEST_MEDIA_CAPACITY_SCRIPT"), os.Getenv("MEMQL_TEST_MEDIA_CAPACITY_OUTPUT")
	if !filepath.IsAbs(python) || filepath.Base(script) != "native_measure.py" || !filepath.IsAbs(script) || filepath.Clean(output) != output || filepath.Dir(output) != "/private/tmp" || !strings.HasPrefix(filepath.Base(output), "callkey-native-capacity-") || filepath.Ext(output) != ".json" {
		t.Fatal("explicit local fixture interpreter, script and temporary receipt required")
	}
	source, err := os.ReadFile(script)
	require.NoError(t, err)
	digest := sha256.Sum256(source)
	selected := strings.Split(os.Getenv("MEMQL_TEST_MEDIA_CAPACITY_CASES"), ",")
	if len(selected) == 1 && selected[0] == "" {
		selected = []string{"1x2", "1x4", "1x8", "2x4", "4x2", "1x16", "2x8"}
	}
	matrix := map[string][2]int{"1x2": {1, 2}, "1x4": {1, 4}, "1x8": {1, 8}, "2x4": {2, 4}, "4x2": {4, 2}, "1x16": {1, 16}, "2x8": {2, 8}}
	seen := map[string]bool{}
	for _, name := range selected {
		if _, ok := matrix[name]; !ok || seen[name] {
			t.Fatal("invalid fixed capacity matrix")
		}
		seen[name] = true
	}
	if len(selected) > len(matrix) {
		t.Fatal("capacity matrix exceeds bound")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()
	existing, err := control.ListRooms(ctx, &livekit.ListRoomsRequest{})
	require.NoError(t, err)
	for _, room := range existing.Rooms {
		if room.NumParticipants != 0 {
			t.Fatal("active local room refuses capacity fixture")
		}
	}
	gitSource, _ := exec.Command("git", "rev-parse", "HEAD").Output()
	sourceDigests := map[string]string{}
	for _, path := range []string{"media_capacity_live_db_test.go", "media_signal_live_db_test.go", "media_signal.go", "media_signal_auth.go", "media_signal_store.go", "presentation_state.go", "pass.go", "../../component/server/media_signal_handler.go"} {
		content, e := os.ReadFile(path)
		require.NoError(t, e)
		digest := sha256.Sum256(content)
		sourceDigests[path] = hex.EncodeToString(digest[:])
	}
	report := map[string]any{"schema": 1, "started_at": time.Now().UTC().Format(time.RFC3339Nano), "scope": "isolated native service and actual pinned local provider; not served BFF/browser acceptance or a supported capacity ceiling", "engine_commit": strings.TrimSpace(string(gitSource)), "native_measure_sha256": hex.EncodeToString(digest[:]), "fixed_local_seat_ceiling": 50, "stop_criteria": map[string]any{"setup_seconds": 45, "peer_rss_mib": 6144, "media_cpu_percent_for_10_seconds": 200, "total_seconds": 540, "all_source_data_stats_and_cleanup_checks_required": true}, "profile": "two 10-second synthetic phases with one Python process per peer and native cleanup readback before simultaneous reentry; camera 640x360 5fps plus one screen per room; 48kHz mono tone; server data 2 packets/s/room", "limitations": []string{"Temporary in-process native handler, not deployed BFF/ingress capacity.", "Synthetic static media, one development machine, no WAN/TURN or provider charge.", "Mock account funding and adult verification are isolated test fixtures; actual tester-code journey is not exercised.", "No processing, ASR, device access, Skills entitlement or 100-person qualification."}}
	cases := []map[string]any{}
	report["source_sha256"] = sourceDigests
	report["go_version"] = runtime.Version()
	headroom, sawEight := false, false
	writeReceipt := func() {
		report["cases"] = cases
		raw, e := json.MarshalIndent(report, "", "  ")
		require.NoError(t, e)
		require.NoError(t, os.WriteFile(output, append(raw, '\n'), 0600))
	}
	defer func() {
		report["finished_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		report["passed"] = !t.Failed() && len(cases) == len(selected)
		writeReceipt()
	}()
	for _, name := range selected {
		shape := matrix[name]
		if shape[0]*shape[1] > 8 && !headroom {
			report["progression_stopped"] = "eight-peer profile did not establish required resource headroom"
			break
		}
		passed := t.Run(name, func(t *testing.T) {
			started, cpuStart := time.Now(), 0.0
			cpuStart, _ = capacityUsage()
			row := map[string]any{"case": name, "rooms": shape[0], "participants_per_room": shape[1], "cleanup_verified": false}
			cases = append(cases, row)
			defer func() {
				cpu, rss := capacityUsage()
				row["native_process_cpu_seconds"] = cpu - cpuStart
				row["native_process_peak_rss_mib"] = rss
				row["wall_seconds"] = time.Since(started).Seconds()
				row["passed"] = !t.Failed()
				writeReceipt()
			}()
			cw, first, _ := commerceFixture(t, "native-capacity-"+name)
			if cw == nil {
				t.Fatal("required isolated native database unavailable")
			}
			nativeLocalSignalConfigure(t, cw, credential)
			service := &capacitySignalService{service: cw.svc}
			handler := httptest.NewServer(server.NewMediaSignalHandler(service))
			defer handler.Close()
			manifest := capacityManifest{Case: name, Origin: handler.URL + "/media", Seconds: 10}
			for roomIndex := 0; roomIndex < shape[0]; roomIndex++ {
				account := first
				if roomIndex > 0 {
					account = accountRegister(t, cw, newSoftwareAuthenticator(t))
				}
				commerceVerified(t, cw, account)
				quoted := commerceDo(t, cw, account, callrules.AccountRequest{Action: "quote_call", ParticipantLimit: shape[1], DurationMinutes: 10})
				start, e := cw.svc.Start(ctx, *account.result.Session, callrules.StartBody{Name: "Synthetic capacity host", Confirm: confirmed, QuoteID: quoted.Quote.QuoteID, IdempotencyKey: fmt.Sprintf("native-capacity-start-%d", roomIndex)})
				require.NoError(t, e)
				nativeLocalSignalRoomCleanup(t, control, start.Call.ID)
				room := capacityRoom{Room: start.Call.ID, Peers: []capacityPeer{{start.Join.Identity, start.Join.Pass}}}
				for index := 1; index < shape[1]; index++ {
					join, _, e := cw.join(start.Call.GuestToken, "Synthetic capacity guest")
					require.NoError(t, e)
					require.Equal(t, callrules.OutcomeAdmit, join.Outcome)
					room.Peers = append(room.Peers, capacityPeer{join.Identity, join.Pass})
				}
				manifest.Rooms = append(manifest.Rooms, room)
			}
			childCtx, childCancel := context.WithTimeout(ctx, 150*time.Second)
			defer childCancel()
			child := exec.CommandContext(childCtx, python, script)
			// A failed native fixture must also stop every SDK child. No orphan
			// participant process may survive a coordinator deadline or failure.
			child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			child.Cancel = func() error { return syscall.Kill(-child.Process.Pid, syscall.SIGKILL) }
			child.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + os.Getenv("TMPDIR"), "PYTHONDONTWRITEBYTECODE=1", "RUST_LOG=off"}
			child.Stderr = io.Discard
			input, e := child.StdinPipe()
			require.NoError(t, e)
			out, e := child.StdoutPipe()
			require.NoError(t, e)
			require.NoError(t, child.Start())
			waited := false
			defer func() {
				input.Close()
				childCancel()
				if !waited {
					_ = child.Wait()
				}
			}()
			encoder := json.NewEncoder(input)
			require.NoError(t, encoder.Encode(manifest))
			scanner := bufio.NewScanner(out)
			scanner.Buffer(make([]byte, 65536), 1024*1024)
			phases := []map[string]any{}
			row["phases"] = phases
			maxCPU, maxRSS := 0.0, 0.0
			for phase := 0; phase < 2; phase++ {
				message, e := capacityRead(childCtx, scanner)
				require.NoError(t, e)
				require.Equal(t, "phase", message["event"], "capacity child did not complete a measured phase")
				result, ok := message["result"].(map[string]any)
				require.True(t, ok)
				phases = append(phases, result)
				row["phases"] = phases
				row["configuration"] = message["configuration"]
				clear := capacityNativeClear(childCtx, cw, manifest.Rooms)
				result["native_cleanup_clear"] = clear
				row["native_admission"] = service.receipt()
				writeReceipt()
				require.True(t, clear, "native pending cleanup prevents progression")
				require.Equal(t, true, result["passed"], "capacity coverage/resource threshold failed")
				for _, sample := range result["resource_samples"].([]any) {
					sample := sample.(map[string]any)
					maxRSS = max(maxRSS, sample["simultaneous_peer_rss_mib"].(float64))
					media := sample["media"].(map[string]any)
					var cpu float64
					_, e := fmt.Sscanf(media["CPUPerc"].(string), "%f%%", &cpu)
					require.NoError(t, e)
					maxCPU = max(maxCPU, cpu)
				}
				require.NoError(t, encoder.Encode(map[string]string{"action": "continue"}))
			}
			message, e := capacityRead(childCtx, scanner)
			require.NoError(t, e)
			require.Equal(t, "complete", message["event"])
			input.Close()
			e = child.Wait()
			waited = true
			require.NoError(t, e)
			for _, room := range manifest.Rooms {
				_, e := control.DeleteRoom(ctx, &livekit.DeleteRoomRequest{Room: room.Room})
				require.NoError(t, e)
				left, e := control.ListRooms(ctx, &livekit.ListRoomsRequest{Names: []string{room.Room}})
				require.NoError(t, e)
				require.Empty(t, left.Rooms)
			}
			row["cleanup_verified"] = true
			row["maximum_sampled_media_cpu_percent"], row["maximum_sampled_peer_rss_mib"] = maxCPU, maxRSS
			if shape[0]*shape[1] == 8 {
				headroom = (!sawEight || headroom) && maxCPU < 100 && maxRSS < 3072
				sawEight = true
			}
			row["extension_headroom"] = headroom
			t.Logf("%s passed both phases and native/provider cleanup; max sampled media CPU %.2f%%, peer RSS %.1f MiB", name, maxCPU, maxRSS)
		})
		if !passed {
			break
		}
	}
}
