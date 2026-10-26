//go:build linux

package capacity

import (
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
)

func ReadSystemHeadroom() (SystemHeadroom, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return SystemHeadroom{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(data) > 65536 {
		return SystemHeadroom{}, errors.New("host memory measurement unavailable")
	}
	return parseSystemMeminfo(string(data))
}

func parseSystemMeminfo(raw string) (SystemHeadroom, error) {
	values := map[string]uint64{}
	for _, line := range strings.Split(raw, "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok || (key != "MemTotal" && key != "MemAvailable") {
			continue
		}
		parts := strings.Fields(rest)
		if len(parts) != 2 || parts[1] != "kB" {
			return SystemHeadroom{}, errors.New("invalid host memory units")
		}
		n, err := strconv.ParseUint(parts[0], 10, 64)
		if _, duplicate := values[key]; duplicate || err != nil || n > ^uint64(0)/1024 {
			return SystemHeadroom{}, errors.New("invalid host memory count")
		}
		values[key] = n * 1024
	}
	total, haveTotal := values["MemTotal"]
	available, haveAvailable := values["MemAvailable"]
	if !haveTotal || !haveAvailable || total == 0 || available > total {
		return SystemHeadroom{}, errors.New("incomplete host memory measurement")
	}
	return SystemHeadroom{Total: total, Available: available}, nil
}
