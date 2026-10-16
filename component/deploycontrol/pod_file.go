package deploycontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const (
	podFileProtocol           = "v5.channel.k8s.io"
	MaxPodFileBytes     int64 = 2 << 30
	podFileControlBytes       = 16 << 10
)

var podFileName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// Only transport interruptions can be retried. A failed remote command, a
// protocol violation or a writer failure must remain a failure.
var errPodFileInterrupted = errors.New("pod file transport")

// ReadPodFile copies one file through Kubernetes' authenticated exec protocol.
// It executes only cat with a separate literal path argument: no shell, stdin,
// terminal, uploaded executable, or caller-selected command. The caller must
// authorize and bind the pod/container identity before and after this read;
// Kubernetes exec itself offers no UID precondition. Bytes remain untrusted.
//
// Output is streamed with bounded memory and a 2 GiB maximum. Success requires
// the remote command's Success status AND normal transport closure. Partial
// bytes, a lost status, cancellation, stderr overflow and protocol errors are
// failures; a caller must discard partial output. This call never retries.
// A supplied writer must return from Write (and honor cancellation if it can
// block). The caller owns it. The operation has a maximum 30-minute lifetime.
func (e *ClusterAPI) ReadPodFile(ctx context.Context, namespace, pod, container, file string, dst io.Writer, maxBytes int64) (int64, error) {
	if err := validatePodFile(namespace, pod, container, file, dst, maxBytes); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	return e.readPodFileCommand(ctx, namespace, pod, container, []string{"cat", "--", file}, dst, maxBytes)
}

func validatePodFile(namespace, pod, container, file string, dst io.Writer, maxBytes int64) error {
	for _, name := range []string{namespace, pod, container} {
		if len(name) > 253 || !podFileName.MatchString(name) || name == "." || name == ".." {
			return errors.New("pod file read requires literal Kubernetes names")
		}
	}
	if dst == nil || maxBytes < 0 || maxBytes > MaxPodFileBytes || len(file) > 4096 ||
		!strings.HasPrefix(file, "/") || path.Clean(file) != file || strings.ContainsAny(file, "\x00\r\n") {
		return errors.New("pod file read requires an absolute clean path, writer and 0 through 2 GiB bound")
	}
	return nil
}

// The command is constructed only by the two file-read primitives. Never
// expose caller-selected commands through this helper.
func (e *ClusterAPI) readPodFileCommand(ctx context.Context, namespace, pod, container string, command []string, dst io.Writer, maxBytes int64) (int64, error) {
	q := url.Values{"container": {container}, "stdout": {"true"}, "stderr": {"true"},
		"stdin": {"false"}, "tty": {"false"}, "command": command}
	req, err := e.newRequest(ctx, http.MethodGet, "api/v1/namespaces/"+namespace+"/pods/"+pod+"/exec?"+q.Encode(), "", nil)
	if err != nil {
		return 0, err
	}
	// Never forward the projected bearer through an API-server redirect.
	client := *e.stream
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	conn, response, err := websocket.Dial(ctx, req.URL.String(), &websocket.DialOptions{
		HTTPClient: &client, HTTPHeader: req.Header, Subprotocols: []string{podFileProtocol},
	})
	if err != nil {
		if response != nil {
			return 0, fmt.Errorf("pod file transport refused: HTTP %d", response.StatusCode)
		}
		return 0, fmt.Errorf("%w: %w", errPodFileInterrupted, err)
	}
	defer conn.CloseNow()
	if conn.Subprotocol() != podFileProtocol {
		return 0, errors.New("pod file transport did not negotiate the required exec protocol")
	}
	conn.SetReadLimit(1 << 20)
	var status bytes.Buffer
	var written, stderrBytes int64
	buffer := make([]byte, 64<<10)
	for {
		kind, reader, err := conn.Reader(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return written, ctx.Err()
			}
			if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
				return written, podFileStreamFailure("closure", written, err)
			}
			if status.Len() == 0 {
				return written, fmt.Errorf("%w: command status was lost after %d bytes", errPodFileInterrupted, written)
			}
			var result struct {
				Status string `json:"status"`
			}
			if json.Unmarshal(status.Bytes(), &result) != nil || result.Status != "Success" {
				return written, errors.New("pod file command did not report success")
			}
			return written, nil
		}
		if kind != websocket.MessageBinary {
			return written, errors.New("pod file transport received a non-binary frame")
		}
		var channel [1]byte
		if _, err := io.ReadFull(reader, channel[:]); err != nil {
			return written, errors.New("pod file transport received an empty frame")
		}
		switch channel[0] {
		case 1: // stdout
			for {
				n, readErr := reader.Read(buffer)
				if int64(n) > maxBytes-written {
					return written, errors.New("pod file exceeds its byte bound")
				}
				if n > 0 {
					m, writeErr := dst.Write(buffer[:n])
					written += int64(m)
					if writeErr != nil {
						return written, writeErr
					}
					if m != n {
						return written, io.ErrShortWrite
					}
				}
				if errors.Is(readErr, io.EOF) {
					break
				}
				if readErr != nil {
					return written, podFileStreamFailure("stdout", written, readErr)
				}
			}
		case 2, 3: // stderr / Kubernetes Status, bounded across every frame
			limit := int64(podFileControlBytes) - stderrBytes
			var target io.Writer = io.Discard
			if channel[0] == 3 {
				limit = int64(podFileControlBytes - status.Len())
				target = &status
			}
			n, err := io.CopyBuffer(target, io.LimitReader(reader, limit+1), buffer)
			if err != nil {
				return written, podFileStreamFailure("control", written, err)
			}
			if n > limit {
				return written, errors.New("pod file control stream exceeds its bound")
			}
			if channel[0] == 2 {
				stderrBytes += n
			}
		default:
			return written, errors.New("pod file transport received an unexpected channel")
		}
	}
}

func podFileStreamFailure(phase string, written int64, err error) error {
	switch websocket.CloseStatus(err) {
	case websocket.StatusProtocolError, websocket.StatusUnsupportedData,
		websocket.StatusInvalidFramePayloadData, websocket.StatusPolicyViolation, websocket.StatusMessageTooBig:
		return fmt.Errorf("pod file %s rejected after %d bytes: %w", phase, written, err)
	default:
		return fmt.Errorf("%w: %s interrupted after %d bytes: %w", errPodFileInterrupted, phase, written, err)
	}
}
