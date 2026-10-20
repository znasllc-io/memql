package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/mem"
)

// Reserve the full two-workspace memory budget plus twenty percent for the
// machine and its other work. The Docker VM and the host are separate limits
// on macOS; both must have room before creating another workspace.
func (d Docker) Headroom(ctx context.Context, runtime Runtime) error {
	other, err := d.memoryUsage(ctx, runtime)
	if err != nil {
		return err
	}
	// Measure the host after Docker's bounded sampling, so a delayed sample
	// cannot admit against an older host-memory observation.
	host, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil || host.Total == 0 {
		return fmt.Errorf("cannot measure machine memory headroom")
	}
	budget := uint64(Slots*MemoryMiB) << 20
	if host.Available < budget+host.Total/5 {
		return ErrBusy
	}
	if runtime.MemoryBytes <= 0 {
		return ErrBusy
	}
	usable := uint64(runtime.MemoryBytes) - uint64(runtime.MemoryBytes)/5
	if other > usable || budget > usable-other {
		return ErrBusy
	}
	return nil
}

var errDockerStatsUnavailable = errors.New("Docker memory measurement is unavailable")

// Docker can report "-- / --" when a container's statistics sample is not
// ready. Retry only that observation, never a workbench command or container
// creation. A persistent missing sample still refuses admission.
func (d Docker) memoryUsage(ctx context.Context, runtime Runtime) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	d.endpoint = runtime.Endpoint
	for attempt := 0; attempt < 3; attempt++ {
		if err := d.verifyDaemon(ctx, runtime); err != nil {
			return 0, err
		}
		out, err := d.command(ctx, nil, "stats", "--no-stream", "--format", "{{json .}}")
		if err != nil {
			return 0, fmt.Errorf("cannot measure Docker memory headroom: %w", err)
		}
		used, err := dockerMemoryUsage(out)
		if !errors.Is(err, errDockerStatsUnavailable) || attempt == 2 {
			return used, err
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, ctx.Err()
		case <-timer.C:
		}
	}
	return 0, errDockerStatsUnavailable
}

func dockerMemoryUsage(out []byte) (uint64, error) {
	var other uint64
	for _, line := range bytes.Split(out, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var row struct{ MemUsage string }
		if json.Unmarshal(line, &row) != nil {
			return 0, fmt.Errorf("Docker returned unreadable memory usage")
		}
		if strings.TrimSpace(row.MemUsage) == "-- / --" {
			return 0, errDockerStatsUnavailable
		}
		used, err := memoryQuantity(strings.TrimSpace(strings.SplitN(row.MemUsage, "/", 2)[0]))
		if err != nil {
			return 0, err
		}
		// Count all existing containers, including workbenches from another OS
		// account on the same daemon. Reserving the full budget again is
		// conservative and avoids trusting a forgeable container name prefix.
		if used > math.MaxUint64-other {
			return 0, fmt.Errorf("Docker memory usage exceeds its numeric bound")
		}
		other += used
	}
	return other, nil
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
