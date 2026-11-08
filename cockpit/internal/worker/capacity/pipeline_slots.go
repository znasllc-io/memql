package capacity

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// PIPELINE BUILD SLOTS (policy pipelines.max_concurrent_steps).
//
// Every reader of the host build lock names the slot files the same way: the
// Cockpit's pipeline steps (tools/pipeline_capacity_unix.go), its workbenches
// (workbench/reservation_unix.go) and the local build guard
// (scripts/lib/host_build_guard.py, which globs the same pattern). The naming
// therefore lives here, beside the other records several workloads read.
//
// Slot 0 is pipeline.lock itself: the host build lock, and the attempt record of
// the step in slot 0, byte for byte what a machine with one slot always wrote.
// A machine that names no max_concurrent_steps creates no other file. Slot n > 0
// is pipeline-slot-n.lock: the lock and the attempt record of the step in that
// slot. Like pipeline.lock, a slot file is never unlinked: removing a locked
// inode would let a newcomer lock a different inode beside its owner.

// PipelineSlots is the most pipeline steps a machine's policy may run at once.
// Each step holds one of the worker's HEADLESS slots on the engine's side for
// as long as it runs (eight by default, config.go), and a step beyond them
// would wait in the engine's queue for this machine instead of being refused
// and routed to another one.
const PipelineSlots = 8

// PipelineLockFile is the host build lock and slot 0's attempt record.
const PipelineLockFile = "pipeline.lock"

const pipelineSlotPrefix, pipelineSlotSuffix = "pipeline-slot-", ".lock"

// PipelineSlotFile is the lock and attempt record of slot under root.
func PipelineSlotFile(root string, slot int) string {
	if slot == 0 {
		return filepath.Join(root, PipelineLockFile)
	}
	return filepath.Join(root, fmt.Sprintf("%s%d%s", pipelineSlotPrefix, slot, pipelineSlotSuffix))
}

// PipelineSlot is one file under the capacity root named like an extra slot's.
type PipelineSlot struct {
	// Slot is the slot the name gives, or 0 for a name no Cockpit writes
	// (pipeline-slot-01.lock, pipeline-slot-x.lock): such a file is listed so
	// a reader can refuse to interpret it rather than overlook it.
	Slot int
	Path string
}

// PipelineSlotFiles lists every file under root named like an extra slot's,
// whatever the current policy's slot count: a machine whose owner lowered
// max_concurrent_steps still holds the records of the slots above it. Slot 0
// (pipeline.lock) is not listed.
func PipelineSlotFiles(root string) ([]PipelineSlot, error) {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var slots []PipelineSlot
	for _, entry := range entries {
		digits, ok := strings.CutPrefix(entry.Name(), pipelineSlotPrefix)
		if !ok {
			continue
		}
		if digits, ok = strings.CutSuffix(digits, pipelineSlotSuffix); !ok {
			continue
		}
		slot := 0
		if n, err := strconv.Atoi(digits); err == nil && n > 0 && strconv.Itoa(n) == digits {
			slot = n
		}
		slots = append(slots, PipelineSlot{Slot: slot, Path: filepath.Join(root, entry.Name())})
	}
	sort.Slice(slots, func(i, j int) bool {
		if slots[i].Slot != slots[j].Slot {
			return slots[i].Slot < slots[j].Slot
		}
		return slots[i].Path < slots[j].Path
	})
	return slots, nil
}

// PipelineSlotsPending reports whether an extra slot holds an attempt record: a
// file with content, or one that is not a regular file (never interpreted). It
// is meaningful only to a caller holding pipeline.lock, shared or exclusive:
// the Cockpit holds it exclusively while any of its steps runs, so under that
// lock no step is writing one, and a record found is an attempt whose process
// ended before its cleanup did.
func PipelineSlotsPending(root string) (bool, error) {
	slots, err := PipelineSlotFiles(root)
	if err != nil {
		return false, err
	}
	for _, slot := range slots {
		info, err := os.Lstat(slot.Path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if !info.Mode().IsRegular() || info.Size() != 0 {
			return true, nil
		}
	}
	return false, nil
}
