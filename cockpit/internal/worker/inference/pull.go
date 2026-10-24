package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
)

// pull.go pulls a model into the runtime this machine serves from.
//
// IT IS AN HTTP CALL, NOT `ollama pull`, AND THE PLAN SAYS OTHERWISE.
// The implementation plan for this epic says "`ollama pull <model>` under
// the supervisor; parses the progress lines". That would work on macOS and
// SILENTLY CANNOT WORK ON LINUX, which is the platform design D1 picked
// Docker for: there the runtime is the ollama/ollama container, and a
// machine that installed it through this command's own plan has no
// `ollama` binary on PATH at all. The subprocess route would have to
// become `docker exec ollama ollama pull`, which hard-codes a container
// name this package does not own and breaks on podman, on a container
// somebody named differently, and on a remote OLLAMA_HOST. The signature
// the plan itself specifies takes `ollamaBase string` -- a URL -- which is
// the shape of the right answer.
//
// The HTTP API is also strictly better at the job. `ollama pull` prints a
// TTY progress bar with carriage returns and ANSI escapes when stdout is a
// terminal and something different when it is not, so "parses the progress
// lines" means parsing a human display that the vendor may restyle in any
// release. POST /api/pull streams NDJSON objects carrying `status`,
// `digest`, `total` and `completed` -- exact byte counts, from the same
// base URL the discoverer already probes, identically for both runtimes.
//
// Verified against Ollama's docs/api.md "Pull a Model" and against
// server/routes.go PullHandler / server/images.go PullModel, all read
// 2026-09-07.
//
// A 200 IS NOT SUCCESS. PullModel emits "pulling manifest" before it
// fetches anything, so the response headers are already sent by the time
// anything can go wrong, and PullHandler then pushes the failure into the
// same body as `{"error": "..."}`. This is the trap CLAUDE.md names for
// the engine's /memql/query, which answers HTTP 200 with the refusal in an
// `errors` array. Every object is read, and the absence of the final
// `{"status":"success"}` is itself a failure -- a runtime killed halfway
// closes the body cleanly, and reading that as a completed pull writes the
// model into models.allow, advertises it to the cluster, and fails on
// somebody else's prompt.

// Progress is one line of a pull, as the runtime reported it.
//
// Completed and Total belong to the LAYER named in Status, not to the
// model: Ollama pulls a model as several blobs and restarts the count for
// each one. A caller rendering a single bar keys it on Status changing
// rather than summing, and one that treats Total as the download size will
// show a bar that reaches the end and starts again. There is no separate
// digest field because the status line already names it ("pulling
// 8934d96d3f08"), and a second spelling of the same fact is one more thing
// for two surfaces to disagree about.
type Progress struct {
	// Model is the id exactly as it was asked for, so a caller rendering
	// several pulls can tell them apart without keeping its own map.
	Model string
	// Status is the runtime's own line. It is what an operator reads when
	// a pull fails, so it is passed through unedited.
	Status string
	// Completed and Total are bytes of the current layer. Zero means the
	// runtime did not say -- the first object for a layer carries a total
	// and no completed, and the manifest and verify phases carry neither.
	Completed, Total uint64
}

var (
	// ErrPullFailed is the runtime reporting a failure of its own. The
	// wrapped message is its words, which is the only thing that
	// distinguishes "no such model" from "no space left on device" from a
	// registry that stopped answering -- none of which this side can
	// diagnose and all of which the operator can act on.
	ErrPullFailed = errors.New("the model runtime could not pull the model")

	// ErrPullIncomplete is a stream that ended without the final success
	// object. Distinct from ErrPullFailed because the runtime said
	// nothing at all: the process was killed, the container stopped, or
	// the connection died. A retry is the reasonable next step, where for
	// ErrPullFailed it usually is not.
	ErrPullIncomplete = errors.New("the model runtime stopped before the pull finished")
)

// pullEvent is one NDJSON object from POST /api/pull.
//
// Total and Completed are int64 to match api.ProgressResponse exactly; a
// uint64 here would turn a negative the runtime should never send into an
// enormous positive one, and a progress bar showing 16 exabytes is a
// harder bug to place than a zero.
type pullEvent struct {
	Status    string `json:"status"`
	Digest    string `json:"digest"`
	Total     int64  `json:"total"`
	Completed int64  `json:"completed"`
	Error     string `json:"error"`
}

// Pull streams a model into the local runtime, reporting progress.
//
// model is passed to the runtime BYTE FOR BYTE. Hugging Face ids --
// `hf.co/<owner>/<repo>` and `hf.co/<owner>/<repo>:<quant>` -- are
// resolved by Ollama itself, and normalising one here (lowercasing it, or
// reading the colon as a tag separator and keeping the left half) pulls a
// different quantization from the one that was asked for and leaves the
// machine advertising an id that does not match what is on disk.
//
// ollamaBase is where the runtime answers; empty takes the same default
// the models package probes. A machine whose operator moved Ollama with
// OLLAMA_HOST must be given the base the discoverer resolved -- this
// function deliberately does not resolve that variable a second time,
// because two resolvers can disagree and then `memql worker models` and a
// pull would be looking in different places.
//
// onProgress may be nil. It is called from this goroutine, in order, and a
// slow one slows the read of the stream.
func Pull(ctx context.Context, ollamaBase, model string, onProgress func(Progress)) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return errors.New("no model was named, so there is nothing to pull")
	}

	body, err := json.Marshal(map[string]any{"model": model, "stream": true})
	if err != nil {
		return fmt.Errorf("pulling %s: %w", model, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pullURL(ollamaBase), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("pulling %s: %w", model, err)
	}
	req.Header.Set("Content-Type", "application/json")

	// NO CLIENT TIMEOUT, and that is the deliberate part. A 40 GB model on
	// a domestic line legitimately takes an hour, and a wall-clock deadline
	// on the client would end it with a network error naming nothing --
	// after which the operator retries and hits the same wall. The caller's
	// context is the only deadline, because the caller is the one that
	// knows whether a person is watching.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("pulling %s was cancelled: %w", model, ctxErr)
		}
		return fmt.Errorf("pulling %s: the model runtime at %s did not answer: %w", model, pullURL(ollamaBase), err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("pulling %s: %w: the runtime answered HTTP %d: %s",
			model, ErrPullFailed, resp.StatusCode, errorFromBody(resp.Body))
	}

	dec := json.NewDecoder(resp.Body)
	var last string
	var succeeded bool
	for {
		var ev pullEvent
		if err := dec.Decode(&ev); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("pulling %s was cancelled after %s: %w", model, quoteStatus(last), ctxErr)
			}
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("pulling %s: %w: its progress stream broke off after %s: %v",
				model, ErrPullIncomplete, quoteStatus(last), err)
		}
		// The error object is checked BEFORE the status, because a failure
		// carries no status and reporting it as progress would leave the
		// last status line -- the one an operator needs -- pointing at
		// whatever succeeded before it.
		if strings.TrimSpace(ev.Error) != "" {
			return fmt.Errorf("pulling %s: %w: %s (its last status was %s)",
				model, ErrPullFailed, strings.TrimSpace(ev.Error), quoteStatus(last))
		}
		if ev.Status == "" {
			continue
		}
		last = ev.Status
		if ev.Status == ollamaPullSuccess {
			succeeded = true
		}
		if onProgress != nil {
			onProgress(Progress{
				Model:     model,
				Status:    ev.Status,
				Completed: clampBytes(ev.Completed),
				Total:     clampBytes(ev.Total),
			})
		}
	}
	if !succeeded {
		return fmt.Errorf("pulling %s: %w: its progress stream ended after %s without reporting success",
			model, ErrPullIncomplete, quoteStatus(last))
	}
	return nil
}

// ollamaPullSuccess is the final status of a completed pull, from
// server/images.go's `fn(api.ProgressResponse{Status: "success"})`.
//
// REQUIRING IT IS FAIL-CLOSED, and that is the trade being made: if a
// future Ollama ends a pull with a different word, this reports a
// successful pull as a failure, the operator runs the command again, and
// the second pull is a fast no-op. The other direction -- accepting any
// clean end as success -- writes a half-pulled model into models.allow and
// advertises it to a fleet that will route real work to it. This package
// takes the same direction the models package takes on every capability.
const ollamaPullSuccess = "success"

// pullURL is where the runtime takes a pull.
func pullURL(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = models.DefaultOllamaBaseURL
	}
	return strings.TrimRight(base, "/") + "/api/pull"
}

// clampBytes keeps a nonsensical count out of a progress bar. See
// pullEvent for why the wire type is signed.
func clampBytes(n int64) uint64 {
	if n < 0 {
		return 0
	}
	return uint64(n)
}

// quoteStatus renders the last status for an error sentence, naming the
// absence when there was none -- "ended after """ reads as a truncated
// message and sends a person looking at the wrong thing.
func quoteStatus(status string) string {
	if strings.TrimSpace(status) == "" {
		return "no status at all"
	}
	return fmt.Sprintf("%q", status)
}

// errorFromBody digs the runtime's own message out of a non-2xx answer.
//
// Bounded, because this is an error path and an endpoint that is not
// Ollama at all -- a proxy, a login page, something else on port 11434 --
// can answer with a megabyte of HTML, and pasting that into a terminal
// buries the one line worth reading.
func errorFromBody(r io.Reader) string {
	raw, _ := io.ReadAll(io.LimitReader(r, 8<<10))
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &payload); err == nil && strings.TrimSpace(payload.Error) != "" {
		return strings.TrimSpace(payload.Error)
	}
	if line := firstLine(string(raw)); line != "" {
		return line
	}
	return "it said nothing"
}
