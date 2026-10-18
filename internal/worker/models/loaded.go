package models

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Loaded is one model Ollama holds in memory right now (GET /api/ps).
//
// It exists for `memql worker models`, because the question a tuned
// machine raises is not "what is configured" but "did it take": whether
// the model is loaded, whether all of it is on the GPU, at which window,
// and until when. Discovery answers what is INSTALLED; only the runtime can
// answer what is LOADED.
type Loaded struct {
	ID string
	// SizeBytes is the loaded model's total size and VRAMBytes the part of
	// it on the GPU. A model loaded while the card was busy can be mostly
	// on the CPU, which is slow and stays that way until it reloads -- the
	// risk of keeping a model resident on a card other things share.
	SizeBytes, VRAMBytes int64
	// ContextLength is the window the model was loaded with (per slot).
	ContextLength int
	// ExpiresAt is when Ollama will unload it; zero when it did not say.
	ExpiresAt time.Time
}

// foreverHorizon is how far out an expiry must be to be Ollama's spelling
// of keep_alive -1: it reports a date centuries away rather than a flag.
const foreverHorizon = 100 * 365 * 24 * time.Hour

// Forever reports an expiry so far away that it is keep_alive -1.
func (l Loaded) Forever(now time.Time) bool {
	return !l.ExpiresAt.IsZero() && l.ExpiresAt.After(now.Add(foreverHorizon))
}

// GPUPercent is the share of the model on the GPU, 0 to 100. A model whose
// size Ollama did not state reads 0 rather than a guess.
func (l Loaded) GPUPercent() int {
	if l.SizeBytes <= 0 {
		return 0
	}
	p := int(l.VRAMBytes * 100 / l.SizeBytes)
	if p > 100 {
		p = 100
	}
	return p
}

type ollamaPSResponse struct {
	Models []struct {
		Name          string    `json:"name"`
		Model         string    `json:"model"`
		Size          int64     `json:"size"`
		SizeVRAM      int64     `json:"size_vram"`
		ContextLength int       `json:"context_length"`
		ExpiresAt     time.Time `json:"expires_at"`
	} `json:"models"`
}

// Loaded asks the native Ollama what it has in memory. An error is the
// reason it could not be asked, as a sentence the caller can print; an
// empty list with no error is a runtime with nothing loaded, which is the
// ordinary state of a machine nobody has tuned.
func (d *Discoverer) Loaded(ctx context.Context) ([]Loaded, error) {
	var ps ollamaPSResponse
	base := d.ollamaBaseURL()
	if err := d.getJSON(ctx, base+"/api/ps", "", &ps); err != nil {
		return nil, fmt.Errorf("no answer from Ollama's /api/ps at %s (%v)", base, unwrapURLError(err))
	}
	out := make([]Loaded, 0, len(ps.Models))
	for _, m := range ps.Models {
		id := strings.TrimSpace(m.Model)
		if id == "" {
			id = strings.TrimSpace(m.Name)
		}
		if id == "" {
			continue
		}
		l := Loaded{ID: id, SizeBytes: m.Size, VRAMBytes: m.SizeVRAM, ContextLength: m.ContextLength}
		// Go's zero time is what an absent expires_at decodes to; Ollama
		// also spells "unknown" as year 1.
		if m.ExpiresAt.Year() > 1 {
			l.ExpiresAt = m.ExpiresAt
		}
		out = append(out, l)
	}
	return out, nil
}

// unwrapURLError is the cause inside a *url.Error. The sentence already names
// the URL the question went to, and the url.Error's own text repeats it.
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}
