// Command memql-call-worker-bridge is a private stdio adapter for a trusted
// media processor. It exposes three typed SDK operations, never a query proxy.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"
)

const maxRequestBytes = 64 * 1024
const maxResponseBytes = 512 * 1024
const operationTimeout = 10 * time.Second

var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type request struct {
	ID        string         `json:"id"`
	Action    string         `json:"action"`
	SiteID    string         `json:"siteId"`
	RoomID    string         `json:"roomId"`
	LeaseID   string         `json:"leaseId,omitempty"`
	Reset     *bool          `json:"reset,omitempty"`
	MaxTracks *int           `json:"maxTracks,omitempty"`
	Revision  *int64         `json:"revision,omitempty"`
	Event     map[string]any `json:"event,omitempty"`
}

type response struct {
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

type operation func(context.Context, request) (any, error)

// A closed envelope rejects duplicates and unknown members before any SDK call.
// Event fields remain native-owned: this adapter cannot grant policy authority.
func parseRequest(line []byte) (request, error) {
	var out request
	if len(line) == 0 || len(line) > maxRequestBytes {
		return out, errors.New("invalid_request")
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return out, errors.New("invalid_request")
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return out, errors.New("invalid_request")
		}
		seen[name] = true
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil {
			return out, errors.New("invalid_request")
		}
	}
	if _, err = decoder.Token(); err != nil {
		return out, errors.New("invalid_request")
	}
	decoder = json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err = decoder.Decode(&out); err != nil {
		return request{}, errors.New("invalid_request")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || !identifier.MatchString(out.ID) || !identifier.MatchString(out.SiteID) || out.LeaseID != "" && !identifier.MatchString(out.LeaseID) {
		return request{}, errors.New("invalid_request")
	}
	if out.Action != "rooms" && !identifier.MatchString(out.RoomID) {
		return request{}, errors.New("invalid_request")
	}
	switch out.Action {
	case "rooms":
		if seen["roomId"] || seen["leaseId"] || seen["reset"] || seen["maxTracks"] || seen["revision"] || seen["event"] {
			return request{}, errors.New("invalid_request")
		}
	case "lease":
		if seen["revision"] || seen["event"] || seen["reset"] && out.Reset == nil || seen["maxTracks"] && (out.MaxTracks == nil || *out.MaxTracks < 0 || *out.MaxTracks > 300) {
			return request{}, errors.New("invalid_request")
		}
	case "publish":
		if out.LeaseID == "" || out.Revision == nil || *out.Revision < 1 || *out.Revision > 9007199254740991 || len(out.Event) == 0 || seen["reset"] || seen["maxTracks"] {
			return request{}, errors.New("invalid_request")
		}
	default:
		return request{}, errors.New("invalid_request")
	}
	return out, nil
}

func writeResponse(w io.Writer, reply response) error {
	data, err := json.Marshal(reply)
	if err != nil || len(data) > maxResponseBytes {
		data, _ = json.Marshal(response{ID: reply.ID, Error: "response_too_large"})
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

func serve(ctx context.Context, in io.Reader, out io.Writer, invoke operation) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), maxRequestBytes+2)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		req, err := parseRequest(scanner.Bytes())
		if err != nil {
			if err = writeResponse(out, response{Error: "invalid_request"}); err != nil {
				return err
			}
			continue
		}
		bounded, cancel := context.WithTimeout(ctx, operationTimeout)
		result, err := invoke(bounded, req)
		deadline := errors.Is(bounded.Err(), context.DeadlineExceeded)
		cancel()
		reply := response{ID: req.ID, OK: err == nil && !deadline, Result: result}
		if err != nil || deadline {
			reply.Result = nil
			reply.Error = "request_failed"
			if deadline || errors.Is(err, context.DeadlineExceeded) {
				reply.Error = "deadline_exceeded"
			}
		}
		if err = writeResponse(out, reply); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		_ = writeResponse(out, response{Error: "invalid_request"})
		return errors.New("input_closed")
	}
	return nil
}

func run() int {
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "This adapter accepts configuration through environment variables and requests through stdin.")
		return 2
	}
	cfg, err := configuration(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Invalid worker bridge configuration; check endpoint, operator credential and certificate settings.")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	invoke, closeConnection, err := connect(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Worker bridge connection failed; verify the configured service and credential.")
		return 1
	}
	defer closeConnection()
	// Cancellation also interrupts an idle pipe; the SDK owns cancellation of an
	// in-flight RPC. The caller restarts after failure, without replaying results.
	interruptRead := context.AfterFunc(ctx, func() { _ = os.Stdin.Close() })
	defer interruptRead()
	if err = serve(ctx, os.Stdin, os.Stdout, invoke); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "Worker bridge input or output closed unexpectedly.")
		return 1
	}
	return 0
}

func main() { os.Exit(run()) }
