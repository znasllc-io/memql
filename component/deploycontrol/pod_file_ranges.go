package deploycontrol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
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
// and contain its full expected length before any bytes reach dst. The file
// length is obtained with a fixed read-only stat command. An incomplete range is discarded and
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
	// A clean exec status does not prove that stdout contained the entire
	// requested range. Bind the immutable file's length before transferring;
	// a short successful stream must be retried, never mistaken for file EOF.
	var metadata bytes.Buffer
	metadataCtx, stopMetadata := context.WithTimeout(ctx, podFileRangeTimeout)
	_, err := e.readPodFileCommand(metadataCtx, namespace, pod, container,
		[]string{"stat", "-c", "MEMQL_SIZE:%s:END", "--", file}, &metadata, 64)
	stopMetadata()
	if err != nil {
		return 0, fmt.Errorf("pod file size could not be verified: %w", err)
	}
	value := metadata.String()
	if !strings.HasPrefix(value, "MEMQL_SIZE:") || !strings.HasSuffix(value, ":END\n") {
		return 0, errors.New("pod file size response is incomplete")
	}
	digits := strings.TrimSuffix(strings.TrimPrefix(value, "MEMQL_SIZE:"), ":END\n")
	if strings.Trim(digits, "0123456789") != "" {
		return 0, errors.New("pod file size is not an unsigned decimal integer")
	}
	fileSize, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || fileSize < 0 || fileSize > maxBytes {
		return 0, errors.New("pod file size is invalid or exceeds its byte bound")
	}
	var total int64
	var chunk bytes.Buffer
	chunk.Grow(podFileRangeBytes)
	for total < fileSize {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		// Full ranges are block-aligned; only the final range may be shorter.
		want := min(int64(podFileRangeBytes), fileSize-total)
		command := []string{"dd", "if=" + file, "bs=" + strconv.Itoa(podFileBlockBytes),
			"skip=" + strconv.FormatInt(total/podFileBlockBytes, 10),
			"count=" + strconv.Itoa(podFileRangeBytes/podFileBlockBytes), "iflag=fullblock"}
		var err error
		for attempt := 1; attempt <= podFileRangeAttempts; attempt++ {
			chunk.Reset()
			rangeCtx, stop := context.WithTimeout(ctx, podFileRangeTimeout)
			_, err = e.readPodFileCommand(rangeCtx, namespace, pod, container, command, &chunk, want+1)
			stop()
			if err == nil && int64(chunk.Len()) != want {
				err = fmt.Errorf("%w: received %d of %d expected bytes", errPodFileInterrupted, chunk.Len(), want)
			}
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
	}
	return total, nil
}
