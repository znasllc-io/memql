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

// Reserve the full two-workspace memory budget plus twenty percent for the
// machine and its other work. The Docker VM and the host are separate limits
// on macOS; both must have room before creating another workspace.
func (d Docker) Headroom(ctx context.Context, runtime Runtime) error {
	host, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil || host.Total == 0 {
		return fmt.Errorf("cannot measure machine memory headroom")
	}
	budget := uint64(Slots*MemoryMiB) << 20
	if host.Available < budget+host.Total/5 {
		return ErrBusy
	}
	d.endpoint = runtime.Endpoint
	if err := d.verifyDaemon(ctx, runtime); err != nil {
		return err
	}
	out, err := d.command(ctx, nil, "stats", "--no-stream", "--format", "{{json .}}")
	if err != nil {
		return fmt.Errorf("cannot measure Docker memory headroom")
	}
	var other uint64
	for _, line := range bytes.Split(out, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var row struct{ MemUsage string }
		if json.Unmarshal(line, &row) != nil {
			return fmt.Errorf("Docker returned unreadable memory usage")
		}
		used, err := memoryQuantity(strings.TrimSpace(strings.SplitN(row.MemUsage, "/", 2)[0]))
		if err != nil {
			return err
		}
		// Count all existing containers, including workbenches from another OS
		// account on the same daemon. Reserving the full budget again is
		// conservative and avoids trusting a forgeable container name prefix.
		other += used
	}
	if runtime.MemoryBytes <= 0 || other+budget > uint64(runtime.MemoryBytes)-uint64(runtime.MemoryBytes)/5 {
		return ErrBusy
	}
	return nil
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
