package hardware

import "strconv"

// The machine class (design record D2).
//
// It answers exactly one question -- WHICH SET OF MODELS IS RECOMMENDED
// FOR THIS MACHINE -- and it is worth being precise about what it does
// not answer, because the two get confused every time.
//
// THE CLASS IS NOT THE HARDWARE FLOOR. The floor (internal/worker/models)
// decides whether this machine serves models AT ALL, and a machine below
// it advertises nothing. The class decides only which set a person is
// pointed at, and a machine below the smallest class still serves
// perfectly well -- a Linux box with 8 GB of VRAM clears the floor and
// holds a 9B model comfortably, and it classes `unsupported`. Reading
// this verdict as a gate would refuse a machine that works.
//
// The rule is the engine's, copied verbatim so the two halves cannot
// drift: the largest of 16, 24, 32, 64, 128 not exceeding usable memory.
// The engine computes it too, in component/memql/fleet_class.go, and
// that is the authority -- this copy exists because `setup --inference`
// has to choose a set BEFORE any engine round trip, on a machine that
// may not even be paired yet. When the engine names a class for this
// machine, that name wins; this is the answer for a machine nobody has
// told anything.
const ClassUnsupported = "unsupported"

// classes, ascending. Fixed by the record.
var classes = []uint64{16, 24, 32, 64, 128}

// UsableBytes is the memory a model actually gets.
//
// Seventy-five percent of the unified pool on Metal, because the OS,
// the window server and everything else the person has open live in the
// same memory and macOS will not hand a model the rest. The WHOLE of
// VRAM on a discrete card, because nothing else is in it -- the display
// on such a machine is usually driven by the same card, but the
// framebuffer is megabytes against gigabytes and pretending otherwise
// would drop a 24 GB card a class for no reason anyone could measure.
//
// A machine with no GPU has no usable memory for this purpose. That is
// the honest answer rather than a pessimistic one: CPU inference is not
// something this cockpit offers, so system memory is not a number any
// recommendation here could spend.
func UsableBytes(inv Inventory) uint64 {
	if inv.GPU == nil {
		return 0
	}
	if inv.GPU.Backend == BackendMetal {
		return inv.GPU.VRAMBytes / 4 * 3
	}
	return inv.GPU.VRAMBytes
}

// Class is the largest class not exceeding usable memory, or
// ClassUnsupported below the smallest.
func Class(inv Inventory) string {
	usableGB := usableGigabytes(UsableBytes(inv))
	out := ClassUnsupported
	for _, c := range classes {
		if usableGB >= c {
			out = strconv.FormatUint(c, 10)
		}
	}
	return out
}

// usableGigabytes converts to whole gigabytes, ROUNDED TO NEAREST
// rather than floored.
//
// Flooring is the obvious reading of "not exceeding", and it
// misclassifies almost every discrete GPU by a whole class. A card sold
// as 24 GB does not report 24 GiB: nvidia-smi reports TOTAL memory
// minus what the driver has already reserved, so a 24 GB RTX reports
// something like 24564 MiB -- 23.988 GiB, which floors to 23 and lands
// a 24 GB machine in class 16. A 16 GB card floors to 15 and classes
// `unsupported` while clearing the hardware floor comfortably. The
// effect is systematic and always in the same direction, so it would
// have shipped as "the fleet mysteriously under-recommends".
//
// Rounding absorbs the reserved-memory slop and nothing else: it moves
// a figure by at most half a gigabyte, and every threshold here is a
// multiple of eight. A card that genuinely has 15 GB still rounds to 15
// and stays unsupported. The unified-memory path is unaffected either
// way -- 75 percent of a power-of-two pool is exact.
func usableGigabytes(b uint64) uint64 {
	const gb = uint64(1) << 30
	return (b + gb/2) / gb
}

// SmallestClass is the floor of the class scale, for the copy that has
// to name it ("below the smallest class (16 GB)").
func SmallestClass() uint64 { return classes[0] }

// Scanned reports whether the inventory established anything at all
// about this machine's memory.
//
// IT IS THE DIFFERENCE BETWEEN A VERDICT AND A GAP, and without it the
// two are the same string. `Class` returns ClassUnsupported for a
// machine genuinely below 16 GB AND for an inventory nobody filled in --
// but "this machine is too small" and "nothing was measured" are
// different facts, and a caller that printed the first when the second
// was true would contradict the floor verdict sitting on the line above
// it. A person reading "32 GB, meets the floor" over "unsupported -- no
// GPU memory found" does not learn that a scan failed; they conclude
// the tool is broken, and they are half right.
func Scanned(inv Inventory) bool {
	return inv.MemoryBytes > 0 || inv.GPU != nil
}
