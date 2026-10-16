package deploycontrol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"
)

const (
	podFileRangeBytes    = 8 << 20
	podFileBlockBytes    = 64 << 10
	podFileRangeAttempts = 3
	podFileRangeTimeout  = 2 * time.Minute
)

// ReadPodFileResumable reads an immutable file in bounded ranges. Each range
// must finish with a successful command status and normal transport closure
// before any of its bytes reach dst. An interrupted range is discarded and
// retried, at most three attempts; earlier verified ranges are never replayed.
//
// The caller must establish immutability and bind the pod/container identity
// before and after the complete read, as for ReadPodFile. Partial overall output
// must be discarded on failure. The fixed read-only dd command uses literal
// arguments, no shell, stdin, output file or caller-selected options. Memory is
// bounded by one 8 MiB range, output by maxBytes, and total time by 30 minutes
// (or the caller's earlier deadline). No credentials are sent to the container.
func (e *ClusterAPI) ReadPodFileResumable(ctx context.Context, namespace, pod, container, file string, dst io.Writer, maxBytes int64) (int64, error) {
	if err := validatePodFile(namespace, pod, container, file, dst, maxBytes); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	var total int64
	var chunk bytes.Buffer
	chunk.Grow(podFileRangeBytes)
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		// Full ranges are block-aligned. A short successful range is EOF.
		command := []string{"dd", "if=" + file, "bs=" + strconv.Itoa(podFileBlockBytes),
			"skip=" + strconv.FormatInt(total/podFileBlockBytes, 10),
			"count=" + strconv.Itoa(podFileRangeBytes/podFileBlockBytes), "iflag=fullblock"}
		var err error
		for attempt := 1; attempt <= podFileRangeAttempts; attempt++ {
			chunk.Reset()
			rangeCtx, stop := context.WithTimeout(ctx, podFileRangeTimeout)
			_, err = e.readPodFileCommand(rangeCtx, namespace, pod, container, command, &chunk, min(podFileRangeBytes, maxBytes-total+1))
			stop()
			if err == nil || ctx.Err() != nil || (!errors.Is(err, errPodFileInterrupted) && !errors.Is(err, context.DeadlineExceeded)) || attempt == podFileRangeAttempts {
				break
			}
			timer := time.NewTimer(time.Duration(attempt) * 200 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return total, ctx.Err()
			case <-timer.C:
			}
		}
		if err != nil {
			return total, fmt.Errorf("pod file range at byte %d failed: %w", total, err)
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
		if int64(chunk.Len()) > maxBytes-total {
			return total, errors.New("pod file exceeds its byte bound")
		}
		size := chunk.Len()
		for chunk.Len() > 0 {
			data := chunk.Next(min(chunk.Len(), podFileBlockBytes))
			n, err := dst.Write(data)
			total += int64(n)
			if err != nil {
				return total, err
			}
			if n != len(data) {
				return total, io.ErrShortWrite
			}
		}
		if size < podFileRangeBytes {
			return total, nil
		}
	}
}
