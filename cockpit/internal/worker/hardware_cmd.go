package worker

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/znasllc-io/memql/cockpit/internal/worker/hardware"
	"github.com/znasllc-io/memql/cockpit/internal/worker/inference"
)

// `memql worker hardware` -- what this machine tells the cluster it is.
//
// IT EXISTS BECAUSE THE PAYLOAD IS OTHERWISE INVISIBLE. Everything in
// it leaves on Register and lands on a row somebody reads in a browser,
// and the two questions it raises are asked from this side: "is that
// actually my machine?" (a VM reporting host memory is a documented
// failure mode) and "does that say anything about me?" A payload nobody
// can print is one nobody can check.
//
// It reads the machine and NOTHING ELSE -- no cluster, no policy, no
// credential -- so it answers on a box that has never been paired,
// which is exactly when somebody is deciding whether to pair it.

// hardwareLabelWidth aligns the field column. Wider than the inference
// preamble's because these labels are longer ("Disk free", "CPU
// cores"), and the two commands are read minutes apart rather than side
// by side, so matching the number matters less than the column being
// straight in each.
const hardwareLabelWidth = 13

type hardwareReport struct {
	out  io.Writer
	scan func(context.Context) hardware.Inventory
}

func newHardwareReport() *hardwareReport {
	return &hardwareReport{out: os.Stdout, scan: hardware.Local}
}

func handleHardware(args []string) {
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "memql worker hardware takes no arguments (got %q)\n", strings.Join(args, " "))
		os.Exit(SetupExitUsage)
	}
	newHardwareReport().run(context.Background())
}

func (r *hardwareReport) run(ctx context.Context) {
	inv := r.scan(ctx)

	r.line("This machine reports the following to the cluster.")
	r.line("")
	r.field("Chip", orUnknown(inv.Chip))
	r.field("Memory", memoryLine(inv))
	r.field("GPU", gpuLine(inv))
	r.field("CPU cores", countOrUnknown(inv.CPUCores))
	r.field("OS", orUnknown(inv.OSVersion))
	r.field("Disk free", bytesOrUnknown(inv.DiskFreeBytes))
	r.field("Runtimes", runtimesLine(inv))
	r.line("")
	r.field("Class", classLine(inv))
	r.line("")

	// The recommended set is the whole reason a person reads the class,
	// so it is not left as an inference from a number.
	r.line("Recommended for this class:")
	for _, id := range inference.RecommendedSet(hardware.Class(inv)) {
		r.line("  " + id)
	}
	r.line("")
	r.line("  memql worker setup --inference    pulls that set")
	r.line("")

	// The privacy claim, stated rather than assumed. Somebody deciding
	// whether to pair a work machine is deciding exactly this, and the
	// answer is a test in internal/worker/hardware -- so it can be said
	// plainly instead of hedged.
	r.line("Nothing above names you, this machine, or any path on it.")
}

// memoryLine says which KIND of memory this is. "64 GB" alone is
// ambiguous between a Mac's unified pool and a PC's system RAM, and the
// class is computed from three quarters of the first and none of the
// second -- so a person checking the arithmetic needs the word.
func memoryLine(inv hardware.Inventory) string {
	if inv.MemoryBytes == 0 {
		return "not established"
	}
	if inv.GPU != nil && inv.GPU.Backend == hardware.BackendMetal {
		return humanGB(inv.MemoryBytes) + " unified"
	}
	return humanGB(inv.MemoryBytes) + " system"
}

// gpuLine names the card, its memory and how it is reached. An ABSENT
// GPU says so in words rather than printing an empty column: "no GPU
// found" and a blank line after "GPU" are the same pixels and
// completely different confidence.
func gpuLine(inv hardware.Inventory) string {
	if inv.GPU == nil {
		return "none found"
	}
	name := orUnknown(inv.GPU.Name)
	if inv.GPU.VRAMBytes == 0 {
		return fmt.Sprintf("%s, memory not established, %s", name, inv.GPU.Backend)
	}
	return fmt.Sprintf("%s, %s, %s", name, humanGB(inv.GPU.VRAMBytes), inv.GPU.Backend)
}

func runtimesLine(inv hardware.Inventory) string {
	if len(inv.Runtimes) == 0 {
		return "none found"
	}
	parts := make([]string, 0, len(inv.Runtimes))
	for _, rt := range inv.Runtimes {
		if rt.Version == "" {
			// Present, version unknown. Said in words, because "ollama"
			// beside "docker 27.3.1" reads as a missing value rather
			// than as a runtime that declines to state one.
			parts = append(parts, rt.Name+" (version not reported)")
			continue
		}
		parts = append(parts, rt.Name+" "+rt.Version)
	}
	return strings.Join(parts, ", ")
}

// classLine shows the class AND the arithmetic behind it, because the
// class is the one number here a person is likely to disagree with --
// "this is a 64 GB machine, why does it say 32" is answered on the same
// line rather than in a doc.
func classLine(inv hardware.Inventory) string {
	class := hardware.Class(inv)
	usable := hardware.UsableBytes(inv)

	// A GAP IS NOT A VERDICT. An inventory nobody could fill in classes
	// `unsupported` exactly like a machine that is genuinely too small,
	// and printing the second sentence for the first contradicts the
	// Hardware line directly above it.
	if !hardware.Scanned(inv) {
		return "not established -- this machine's memory could not be read, so the smallest set is recommended"
	}

	if class == hardware.ClassUnsupported {
		if usable == 0 {
			return fmt.Sprintf("unsupported -- no GPU memory found, so no set is recommended above the smallest (%d GB)", hardware.SmallestClass())
		}
		return fmt.Sprintf("unsupported -- %s usable, below the smallest class (%d GB)",
			preciseGB(usable), hardware.SmallestClass())
	}
	if inv.GPU != nil && inv.GPU.Backend == hardware.BackendMetal {
		return fmt.Sprintf("%s -- %s usable, 75%% of %s unified", class, preciseGB(usable), humanGB(inv.MemoryBytes))
	}
	return fmt.Sprintf("%s -- %s usable, the whole of the GPU's memory", class, preciseGB(usable))
}

// preciseGB renders the usable figure to one decimal place, and it is
// the ONE place in this output that does not round to whole gigabytes.
//
// The reason is the class beside it. A card sold as 24 GB reports 23.99
// after the driver's reservation, and "23 GB usable" next to "Class 24"
// is a line a person has to reconcile before they can trust either
// number -- they will assume one of the two is a bug, and half the time
// they will be right to check. "23.9 GB usable" next to "Class 24" is
// arithmetic anybody accepts at a glance.
//
// It stays out of the fields above, where whole gigabytes are what an
// operator compares against a spec sheet.
func preciseGB(b uint64) string {
	const gb = float64(uint64(1) << 30)
	return fmt.Sprintf("%.1f GB", float64(b)/gb)
}

func (r *hardwareReport) line(text string) { fmt.Fprintln(r.out, text) }

func (r *hardwareReport) field(label, value string) {
	fmt.Fprintf(r.out, "  %-*s%s\n", hardwareLabelWidth, label, value)
}

// orUnknown renders a fact the probe could not establish. "not
// established" rather than "unknown": it says the probe LOOKED, which
// is what distinguishes this from a field that was never filled in.
func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "not established"
	}
	return s
}

func countOrUnknown(n int) string {
	if n <= 0 {
		return "not established"
	}
	return fmt.Sprintf("%d", n)
}

func bytesOrUnknown(b uint64) string {
	if b == 0 {
		return "not established"
	}
	return humanGB(b)
}

// humanGB renders whole gigabytes, ROUNDED TO NEAREST -- the same
// conversion hardware.Class uses, so the fields and the class can never
// disagree about how many gigabytes something is.
//
// Rounding rather than flooring for the reason the class does it: a
// card sold as 24 GB reports 23.99 after the driver's reservation, and
// flooring prints "23 GB" for a card whose box, its vendor's control
// panel and its spec sheet all say 24. That is the number an operator
// is checking this line against.
func humanGB(b uint64) string {
	const gb = uint64(1) << 30
	if b >= gb {
		return fmt.Sprintf("%d GB", (b+gb/2)/gb)
	}
	const mb = uint64(1) << 20
	return fmt.Sprintf("%d MB", (b+mb/2)/mb)
}
