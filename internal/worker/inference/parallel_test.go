package inference

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/hardware"
)

// parallel_test.go pins the one number the managed Ollama and the cockpit
// have to agree on: how many requests a model takes at once. Ollama
// decides it from OLLAMA_NUM_PARALLEL in the unit; the worker advertises it
// as the `max=` on the model label, and the engine rations by that label.

func TestDefaultParallel(t *testing.T) {
	for class, want := range map[string]int{
		// Measured on a 24 GB card: two slots at a 16K window leave about
		// 2.7 GB free (spec, section 5). Higher classes have more room but
		// no measurement behind them.
		"24": 2, "32": 2,
		"unsupported": 1, "16": 1, "64": 1, "128": 1, "": 1, "nonsense": 1,
	} {
		if got := DefaultParallel(class); got != want {
			t.Errorf("DefaultParallel(%q) = %d, want %d", class, got, want)
		}
	}
}

func TestTheNativeUnitCarriesTheParallelSlots(t *testing.T) {
	s := stageIn("/home/op")
	if unit := s.Unit(); strings.Contains(unit, "OLLAMA_NUM_PARALLEL") {
		t.Errorf("a stage with no parallel setting must write the unit it always wrote:\n%s", unit)
	}
	s.Parallel = 1
	if unit := s.Unit(); strings.Contains(unit, "OLLAMA_NUM_PARALLEL") {
		t.Errorf("one slot is Ollama's own default and needs no line:\n%s", unit)
	}
	s.Parallel = 2
	unit := s.Unit()
	if !strings.Contains(unit, "\nEnvironment=OLLAMA_NUM_PARALLEL=2\n") {
		t.Errorf("the unit does not set the slots:\n%s", unit)
	}
	// Beside the other Environment lines, inside [Service].
	if strings.Index(unit, "OLLAMA_NUM_PARALLEL") < strings.Index(unit, "[Service]") ||
		strings.Index(unit, "OLLAMA_NUM_PARALLEL") > strings.Index(unit, "[Install]") {
		t.Errorf("the setting is outside [Service]:\n%s", unit)
	}
}

// The consent covers the stage, so the stage has to say what it will set.
func TestStageLinesNameTheParallelSlots(t *testing.T) {
	s := stageIn("/home/op")
	if got := strings.Join(s.Lines(), "\n"); strings.Contains(got, "at a time") {
		t.Errorf("one slot needs no mention:\n%s", got)
	}
	s.Parallel = 2
	if got := strings.Join(s.Lines(), "\n"); !strings.Contains(got, "2 requests at a time per model") {
		t.Errorf("the stage must say how many requests it lets Ollama take:\n%s", got)
	}
}

// A fresh native install on a 24 GB card gets the class default; on any
// other class it writes the unit it always wrote. A machine whose runtime
// is already serving is never touched (decideLinuxNative returns first),
// which is why the owner's laptop is changed by hand and documented.
func TestDecideGivesTheStageTheClassDefault(t *testing.T) {
	host := func(vramMiB uint64) Host {
		return Host{
			GOOS: "linux", GOARCH: "amd64",
			Floor:    metFloor("NVIDIA GeForce RTX 5090 Laptop GPU"),
			Native:   nativeFacts(GPUVendorNVIDIA),
			LookPath: lookPath("systemctl", "nvidia-smi"),
			Hardware: hardware.Inventory{
				GPU: &hardware.GPU{Name: "NVIDIA GeForce RTX 5090 Laptop GPU", VRAMBytes: vramMiB << 20, Backend: hardware.BackendCUDA},
			},
		}
	}
	for name, tc := range map[string]struct {
		vramMiB   uint64
		wantClass string
		want      int
	}{
		"the reference laptop":   {24463, "24", 2},
		"a 32 GB card":           {32607, "32", 2},
		"a 16 GB card":           {16303, "16", 1},
		"an 8 GB card":           {8188, "unsupported", 1},
		"a card with no reading": {0, "unsupported", 1},
	} {
		t.Run(name, func(t *testing.T) {
			p := Decide(host(tc.vramMiB))
			if p.Stage == nil {
				t.Fatalf("no stage: %q", p.Refusal)
			}
			if p.MachineClass != tc.wantClass {
				t.Fatalf("class = %q, want %q", p.MachineClass, tc.wantClass)
			}
			if p.Stage.Parallel != tc.want {
				t.Errorf("Stage.Parallel = %d, want %d", p.Stage.Parallel, tc.want)
			}
		})
	}
}

func TestUnitParallel(t *testing.T) {
	tests := []struct {
		name  string
		texts []string
		want  int
	}{
		{"a plain line", []string{"[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=2\n"}, 2},
		{"quoted whole", []string{"[Service]\nEnvironment=\"OLLAMA_NUM_PARALLEL=3\"\n"}, 3},
		{"quoted single", []string{"[Service]\nEnvironment='OLLAMA_NUM_PARALLEL=3'\n"}, 3},
		{"several assignments on one line", []string{"[Service]\nEnvironment=OLLAMA_HOST=127.0.0.1:11434 OLLAMA_NUM_PARALLEL=4 OLLAMA_KV_CACHE_TYPE=q8_0\n"}, 4},
		{"the last assignment wins", []string{"[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=2\nEnvironment=OLLAMA_NUM_PARALLEL=3\n"}, 3},
		{"a drop-in overrides the unit", []string{"[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=2\n", "[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=3\n"}, 3},
		{"an empty Environment resets the list", []string{"[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=2\n", "[Service]\nEnvironment=\n"}, 0},
		{"another section is not the service", []string{"[Unit]\nEnvironment=OLLAMA_NUM_PARALLEL=9\n"}, 0},
		{"a comment is not a setting", []string{"[Service]\n# Environment=OLLAMA_NUM_PARALLEL=2\n; Environment=OLLAMA_NUM_PARALLEL=2\n"}, 0},
		{"a similar name is not the variable", []string{"[Service]\nEnvironment=OLLAMA_NUM_PARALLEL_X=5\n"}, 0},
		// systemd would hand Ollama the text, and Ollama ignores what it
		// cannot parse, so an unusable last word leaves nothing to claim.
		{"a word is not a number", []string{"[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=two\n"}, 0},
		{"zero is not a ceiling", []string{"[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=0\n"}, 0},
		{"negative is not a ceiling", []string{"[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=-2\n"}, 0},
		// Ollama reads the number with strconv.ParseUint after trimming spaces
		// and quotes. It logs anything else as invalid and runs one slot, so the
		// unit must not claim more than that.
		{"a plus sign is not a number to Ollama", []string{"[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=+2\n"}, 0},
		{"a decimal is not a number to Ollama", []string{"[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=2.0\n"}, 0},
		{"a hex prefix is not a number to Ollama", []string{"[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=0x2\n"}, 0},
		{"spaces inside the quoted word are trimmed as Ollama trims them", []string{"[Service]\nEnvironment=\"OLLAMA_NUM_PARALLEL= 3 \"\n"}, 3},
		{"a count too big to hold is not a ceiling", []string{"[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=4294967296\n"}, 0},
		{"an unusable later value does not keep the earlier one", []string{"[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=2\nEnvironment=OLLAMA_NUM_PARALLEL=lots\n"}, 0},
		{"no unit at all", nil, 0},
		{"a unit that says nothing", []string{"[Service]\nExecStart=/usr/bin/env ollama serve\n"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := UnitParallel(tt.texts...); got != tt.want {
				t.Errorf("UnitParallel = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestUnitParallelReadsWhatTheStageWrote. The cockpit reads back the line
// it wrote, whatever else the operator's paths put in the unit.
func TestUnitParallelReadsWhatTheStageWrote(t *testing.T) {
	s := stageIn("/home/A Person%u")
	s.ModelsDir = "/mnt/a \"quoted\" path%u/line\nnext"
	s.Parallel = 2
	if got := UnitParallel(s.Unit()); got != 2 {
		t.Errorf("UnitParallel(Stage.Unit()) = %d, want 2:\n%s", got, s.Unit())
	}
}

// TestManagedParallelAtReadsTheUnitAndItsDropIns. `systemctl --user edit`
// writes a drop-in, which is the way systemd documents changing a unit, so
// a cockpit that read only the unit would ignore the owner's edit.
func TestManagedParallelAtReadsTheUnitAndItsDropIns(t *testing.T) {
	dir := t.TempDir()
	unit := filepath.Join(dir, "memql-ollama.service")

	if got := managedParallelAt(unit); got != 0 {
		t.Errorf("no unit file -> %d, want 0 (no managed runtime)", got)
	}
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(unit, "[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=2\n")
	if got := managedParallelAt(unit); got != 2 {
		t.Errorf("the unit alone -> %d, want 2", got)
	}
	write(unit+".d/10-first.conf", "[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=3\n")
	write(unit+".d/20-second.conf", "[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=4\n")
	if got := managedParallelAt(unit); got != 4 {
		t.Errorf("drop-ins apply in lexical order, the last one winning -> %d, want 4", got)
	}
	// Not a .conf: systemd ignores it, so does the cockpit.
	write(unit+".d/30-notes.txt", "[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=9\n")
	if got := managedParallelAt(unit); got != 4 {
		t.Errorf("a non-.conf file in the drop-in directory changed the answer to %d", got)
	}

	// A home directory is a path, not a pattern. The reader used to glob the
	// drop-in directory, and brackets, a backslash or a star in the path made
	// the glob miss the drop-ins without a sound.
	for _, name := range []string{"h[1]", "h[1", `h\x`, "h*?"} {
		t.Run("a home directory named "+name, func(t *testing.T) {
			unit := filepath.Join(t.TempDir(), name, "memql-ollama.service")
			mustWriteFile(t, unit, "[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=2\n")
			mustWriteFile(t, unit+".d/10-raise.conf", "[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=4\n")
			if got := managedParallelAt(unit); got != 4 {
				t.Errorf("the drop-in under %q was not read: got %d, want 4", name, got)
			}
		})
	}
	// systemd ignores hidden files in a drop-in directory, so does the cockpit.
	t.Run("a hidden drop-in is ignored", func(t *testing.T) {
		unit := filepath.Join(t.TempDir(), "memql-ollama.service")
		mustWriteFile(t, unit, "[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=2\n")
		mustWriteFile(t, unit+".d/.disabled.conf", "[Service]\nEnvironment=OLLAMA_NUM_PARALLEL=7\n")
		if got := managedParallelAt(unit); got != 2 {
			t.Errorf("a hidden .conf file changed the answer to %d, want the unit's 2", got)
		}
	})
}

// mustWriteFile writes a test fixture and its directories, failing the test
// on any error.
func mustWriteFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}
