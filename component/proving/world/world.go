// Package world is the fake external world a proving scenario runs against.
//
// PURE -- standard library only. Three facets and no network: a benchmark that
// reached the internet would measure the internet.
//
// EVERY facet counts what it was asked to do, and counts DUPLICATES
// separately. That is not bookkeeping, it is the durability family's entire
// assertion: "zero duplicated side effects after a mid-run kill" is a claim
// about a counter somebody kept, and a world that merely refused a duplicate
// would make the claim true by construction and prove nothing.
//
// So the rule here is the opposite of a production outbox's: A DUPLICATE IS
// ACCEPTED AND RECORDED. The world's job is to notice, not to prevent.
package world

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Delivery is one thing the world was asked to do.
type Delivery struct {
	// Facet is machine, mailbox or http.
	Facet string
	// Key is what makes two deliveries "the same delivery". For a mailbox it
	// is the address plus the body digest; for a machine, the script hash;
	// for HTTP, the method, path and body digest. It is NOT the idempotency
	// key the platform supplies -- using that would make the world agree with
	// the platform by construction, and the whole question is whether the
	// platform's key did its job.
	Key string
	// Detail is what a failure message quotes back.
	Detail string
	// Duplicate records that this delivery repeated an earlier one.
	Duplicate bool
	// IdempotencyKey is what the caller supplied, recorded so a failure can
	// say whether the platform even tried.
	IdempotencyKey string
}

// World is the fake external world for one scenario run.
type World struct {
	mu sync.Mutex

	// scripts maps a script name to the stdout the machine returns.
	scripts map[string]string
	// addresses the mailbox accepts.
	addresses map[string]bool
	// routes maps "METHOD /path" to a canned body.
	routes map[string]string

	deliveries []Delivery
	seen       map[string]int
	// scriptHashes records the digest of every script the machine was asked
	// to run, which is what the fleet scenario verifies.
	scriptHashes []string
}

// Config is the scenario's declared world.
type Config struct {
	Scripts   map[string]string
	Addresses []string
	Routes    map[string]string
}

// New builds a world from a scenario's declaration.
func New(c Config) *World {
	w := &World{
		scripts:   map[string]string{},
		addresses: map[string]bool{},
		routes:    map[string]string{},
		seen:      map[string]int{},
	}
	for k, v := range c.Scripts {
		w.scripts[k] = v
	}
	for _, a := range c.Addresses {
		w.addresses[a] = true
	}
	for k, v := range c.Routes {
		w.routes[k] = v
	}
	return w
}

// RunScript asks the fake machine to run a script. It returns the recorded
// stdout, or an error naming the scripts it does know -- an unknown script is
// a scenario defect, and answering "" would let it pass.
func (w *World) RunScript(name, body, idempotencyKey string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out, ok := w.scripts[name]
	if !ok {
		return "", fmt.Errorf("proving/world: the fake machine has no script %q (it has: %s)", name, strings.Join(sortedKeys(w.scripts), ", "))
	}
	h := digest(body)
	w.scriptHashes = append(w.scriptHashes, h)
	w.record("machine", h, "script "+name, idempotencyKey)
	return out, nil
}

// Send asks the fake mailbox to deliver. An unknown address is an ERROR rather
// than a silent drop: a scenario that mails somewhere the world does not know
// about would otherwise report a clean run having delivered nothing.
func (w *World) Send(to, body, idempotencyKey string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.addresses[to] {
		return fmt.Errorf("proving/world: the fake mailbox does not accept %q (it accepts: %s)", to, strings.Join(sortedBoolKeys(w.addresses), ", "))
	}
	w.record("mailbox", to+"|"+digest(body), "to "+to, idempotencyKey)
	return nil
}

// Fetch asks the fake HTTP table.
func (w *World) Fetch(method, path, body, idempotencyKey string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	route := method + " " + path
	reply, ok := w.routes[route]
	if !ok {
		return "", fmt.Errorf("proving/world: the fake HTTP table has no route %q (it has: %s)", route, strings.Join(sortedKeys(w.routes), ", "))
	}
	w.record("http", route+"|"+digest(body), route, idempotencyKey)
	return reply, nil
}

// record appends a delivery and marks it duplicate if its key repeats. Callers
// hold the lock.
func (w *World) record(facet, key, detail, idem string) {
	w.seen[key]++
	w.deliveries = append(w.deliveries, Delivery{
		Facet: facet, Key: key, Detail: detail,
		Duplicate:      w.seen[key] > 1,
		IdempotencyKey: idem,
	})
}

// Count returns how many deliveries a facet saw, duplicates INCLUDED. A count
// that quietly excluded duplicates would make `count: 1` pass on a world that
// delivered twice.
func (w *World) Count(facet string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, d := range w.deliveries {
		if d.Facet == facet {
			n++
		}
	}
	return n
}

// Duplicates returns how many deliveries repeated an earlier one. This is the
// durability family's headline figure and it must be zero.
func (w *World) Duplicates() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, d := range w.deliveries {
		if d.Duplicate {
			n++
		}
	}
	return n
}

// DuplicateDetail describes the duplicates, for a failure message. A count
// with no detail leaves the reader to go and find which delivery repeated.
func (w *World) DuplicateDetail() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var parts []string
	for _, d := range w.deliveries {
		if !d.Duplicate {
			continue
		}
		idem := d.IdempotencyKey
		if idem == "" {
			idem = "(no idempotency key was supplied)"
		}
		parts = append(parts, fmt.Sprintf("%s %s under %s", d.Facet, d.Detail, idem))
	}
	return strings.Join(parts, "; ")
}

// Deliveries returns a copy of the log.
func (w *World) Deliveries() []Delivery {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]Delivery, len(w.deliveries))
	copy(out, w.deliveries)
	return out
}

// ScriptHashes returns the digests of every script the machine was asked to
// run, in order.
func (w *World) ScriptHashes() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, len(w.scriptHashes))
	copy(out, w.scriptHashes)
	return out
}

// FirstAddress is the address deliveries go to when a scenario declares no
// recipient of its own. Empty when the world has no mailbox, which Send then
// refuses loudly rather than dropping.
func (w *World) FirstAddress() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	keys := sortedBoolKeys(w.addresses)
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

// Counter reads "<facet>.<name>" as the scenario's effect assertions spell it.
// ok is false for an unknown counter, which the verifier reports rather than
// treating as zero -- a typo'd counter that read zero would make
// `count: 0` pass forever.
func (w *World) Counter(spec string) (int, bool) {
	facet, name, ok := strings.Cut(spec, ".")
	if !ok {
		return 0, false
	}
	switch name {
	case "sent", "run", "fetched", "delivered":
		return w.Count(facet), true
	case "duplicates":
		return w.Duplicates(), true
	}
	return 0, false
}

// KnownCounters lists the spellings Counter accepts, for an error message.
func KnownCounters() []string {
	return []string{
		"machine.run", "machine.duplicates",
		"mailbox.sent", "mailbox.delivered", "mailbox.duplicates",
		"http.fetched", "http.duplicates",
	}
}

func digest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:16]
}

// Digest is the world's own content digest, exported so the fleet scenario's
// named check can compare the hash the step computed against the hash the
// machine recorded.
func Digest(s string) string { return digest(s) }

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedBoolKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
