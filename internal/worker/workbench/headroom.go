package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v4/mem"
)

// Charge one new workspace and the unused allowance of existing reservations.
// Their current usage is already in the host/Docker measurements. Keep a bounded
// host cushion and twenty percent of Docker free; empty slots consume no budget.
// The caller holds the cross-process admission lock through Create.
func (d Docker) Headroom(ctx context.Context, runtime Runtime, active []record) error {
	d.endpoint = runtime.Endpoint
	if err := d.verifyDaemon(ctx, runtime); err != nil {
		return err
	}
	out, err := d.command(ctx, nil, "stats", "--no-stream", "--format", "{{json .}}")
	if err != nil {
		return fmt.Errorf("cannot measure Docker memory headroom")
	}
	usage := map[string]uint64{}
	for _, line := range bytes.Split(out, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var row struct{ Name, MemUsage string }
		if json.Unmarshal(line, &row) != nil || row.Name == "" {
			return fmt.Errorf("Docker returned unreadable memory usage")
		}
		used, err := memoryQuantity(strings.TrimSpace(strings.SplitN(row.MemUsage, "/", 2)[0]))
		if err != nil {
			return err
		}
		if _, duplicate := usage[row.Name]; duplicate {
			return fmt.Errorf("Docker returned duplicate memory usage")
		}
		usage[row.Name] = used
	}
	host, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil || host.Total == 0 {
		return fmt.Errorf("cannot measure machine memory headroom")
	}
	return checkHeadroom(host.Total, host.Available, runtime, active, usage)
}

func checkHeadroom(total, available uint64, runtime Runtime, active []record, usage map[string]uint64) error {
	if total == 0 || available > total || runtime.MemoryBytes <= 0 {
		return fmt.Errorf("memory headroom is unavailable")
	}
	const workspaceBytes = uint64(MemoryMiB) << 20
	hostGrowth, dockerGrowth := workspaceBytes, workspaceBytes
	seen := map[string]bool{}
	for _, existing := range active {
		key := existing.DockerID + "/" + existing.Name
		if seen[key] || existing.Name == "" || existing.DockerID == "" || len(active) >= Slots {
			return ErrInterrupted
		}
		seen[key] = true
		if existing.DockerID != runtime.ID {
			// Its host memory can still grow, but it does not consume this daemon.
			hostGrowth += workspaceBytes
			continue
		}
		// Only exact names from the local reservation journal get usage credit.
		// Unknown containers are counted below, never excluded by name/label.
		growth := workspaceBytes - min(workspaceBytes, usage[existing.Name])
		hostGrowth += growth
		dockerGrowth += growth
	}
	cushion := hostMemoryCushion(total)
	if available < cushion || hostGrowth > available-cushion {
		return ErrBusy
	}
	remaining := uint64(runtime.MemoryBytes) - uint64(runtime.MemoryBytes)/5
	for _, used := range usage {
		if used > remaining {
			return ErrBusy
		}
		remaining -= used
	}
	if dockerGrowth > remaining {
		return ErrBusy
	}
	return nil
}

// Leave room for the OS and unrelated applications after every bounded workspace
// reaches its limit. Scaling this reserve without a ceiling unnecessarily blocks
// a 1.5 GiB task on larger hosts; the workspaces' own growth is charged separately.
func hostMemoryCushion(total uint64) uint64 {
	const gib = uint64(1 << 30)
	return min(4*gib, max(gib, total/5))
}

func memoryQuantity(text string) (uint64, error) {
	for _, unit := range []struct {
		name  string
		scale float64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"GB", 1e9}, {"MB", 1e6}, {"kB", 1e3}, {"B", 1}} {
		if strings.HasSuffix(text, unit.name) {
			n, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(text, unit.name)), 64)
			if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > float64(1<<60)/unit.scale {
				return 0, fmt.Errorf("Docker memory measurement is invalid")
			}
			return uint64(n * unit.scale), nil
		}
	}
	return 0, fmt.Errorf("Docker memory measurement is unavailable")
}
