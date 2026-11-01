// Package fleetmemory is the one sum for whether a machine has room for a model.
//
// Two places ask. The engine asks before it chooses a machine, from the machine's
// capacity report (component/memql's AdmitFleetMachine). The machine asks again
// before it hands the call to a runtime (the Cockpit's hostResources.acquire).
// While each had a sum of its own the engine kept sending calls the machine then
// declined, each one costing the caller up to 30 seconds (memql#6414). So there
// is one sum, here, and both sides read free memory through one reader (the
// Cockpit's capacity.ReadMemory).
//
// cockpit/internal/worker/modelcall/admission_agreement_test.go runs both real
// callers on the same figures, for every model in the catalog.
package fleetmemory

import "math"

const (
	gib = uint64(1) << 30

	// workingContextTokens is the context window WorkingSet already covers.
	workingContextTokens = 32768
)

// WorkingSet estimates what a model of this file size takes resident. It is a
// conservative admission estimate, never reported as usage. The file gets an
// eighth of itself, and at least 2 GiB, of context and compute headroom: the
// figure that reproduces the catalog's residency at a 32K working context
// (dsl/models/seeds.memql), which is what a machine class is chosen by. A larger
// allowance refuses the catalog's own pick on the class it is picked for: 25%
// plus 2 GiB put the 24 GB class's 27B past every 24 GB card, permanently.
//
// Zero file bytes is no estimate at all.
func WorkingSet(fileBytes uint64) uint64 {
	if fileBytes == 0 {
		return 0
	}
	return saturatingAdd(fileBytes, max(fileBytes/8, 2*gib))
}

// ForContext adds what a context window above the working window costs: 2 GiB a
// step. A runtime allocates a window whole, so asking for one token past a step
// costs the whole of it.
func ForContext(need uint64, contextTokens int64) uint64 {
	if contextTokens <= workingContextTokens {
		return need
	}
	steps := uint64((contextTokens - 1) / workingContextTokens)
	if steps > math.MaxUint64/(2*gib) {
		return math.MaxUint64
	}
	return saturatingAdd(need, steps*2*gib)
}

// Reserve is what admission leaves free on a pool of this size, for the operating
// system and whatever else the machine is doing. It belongs to the pool rather
// than to the model: 10% of a 20 GB model is no use to a machine with 4 GB of
// other work on it, and is too much to ask of a 24 GB card.
func Reserve(total uint64) uint64 {
	return min(4*gib, max(gib, total/16))
}

// CanFit reports whether a need could ever be admitted on a pool of this size,
// however free the pool is. What cannot is refused before anything is done for it.
func CanFit(need, total uint64) bool {
	return need <= total && Reserve(total) <= total-need
}

// RoomWanted is the free memory a call needs: what it still has to load, with the
// reserve left over. Resident is what the runtime already holds of this model.
//
// A warm model still keeps the reserve. A call that allocates nothing could run
// without it, and letting it would serve some warm calls that are declined today,
// but that is a change to what machines admit, not a detail of this sum.
func RoomWanted(need, resident, total uint64) uint64 {
	return saturatingAdd(Reserve(total), need-min(need, resident))
}

func saturatingAdd(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}
	return a + b
}
