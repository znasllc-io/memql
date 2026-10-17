package workbench

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Engine interface {
	Ready(context.Context) (Runtime, error)
	Create(context.Context, Runtime, string, string) error
	Remove(context.Context, Runtime, string) error
	Execute(context.Context, Runtime, string, Request) (Result, error)
}
type workspace struct {
	mu               sync.Mutex
	key, name, home  string
	reservation      *reservation
	runtime          Runtime
	created, touched time.Time
	deadline         time.Time
	closed           bool
}
type Manager struct {
	mu     sync.Mutex
	engine Engine
	root   string
	spaces map[string]*workspace
	now    func() time.Time
}

func New(engine Engine, root string) *Manager {
	return &Manager{engine: engine, root: root, spaces: map[string]*workspace{}, now: time.Now}
}

// No operation waits on this machine. Busy is a refusal BEFORE execution; the
// cluster's bounded queue retries only after reading current consent again.
func (m *Manager) Execute(ctx context.Context, id Identity, request Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if id.ExpiresAt.IsZero() || !m.now().Before(id.ExpiresAt) || id.ExpiresAt.Sub(m.now()) > Lifetime+time.Second {
		return Result{}, fmt.Errorf("workbench lease has expired or is invalid")
	}
	if err := Validate(request); err != nil {
		return Result{}, err
	}
	key, err := id.Key()
	if err != nil {
		return Result{}, err
	}
	m.mu.Lock()
	if m.root == "" {
		m.root, err = capacityRoot()
		if err != nil {
			m.mu.Unlock()
			return Result{}, err
		}
	}
	w := m.spaces[key]
	if w == nil {
		if _, err = os.Stat(filepath.Join(m.root, "workbench-ended-"+key)); err == nil {
			m.mu.Unlock()
			return Result{}, fmt.Errorf("workbench expired or interrupted; start a new goal")
		}
		// Reserve under the map lock. Docker creation happens with only this
		// workspace locked; unrelated work remains responsive.
		lease, err := reserve(m.root)
		if err != nil {
			m.mu.Unlock()
			return Result{}, err
		}
		w = &workspace{key: key, home: id.Home, reservation: lease, created: m.now(), touched: m.now(), deadline: id.ExpiresAt}
		w.mu.Lock()
		m.spaces[key] = w
		m.mu.Unlock()
		if err = m.create(ctx, w); err != nil {
			m.retire(w)
			w.mu.Unlock()
			return Result{}, err
		}
	} else {
		if !w.mu.TryLock() {
			m.mu.Unlock()
			return Result{}, ErrBusy
		}
		m.mu.Unlock()
	}
	defer w.mu.Unlock()
	if w.closed {
		return Result{}, fmt.Errorf("workbench has ended; start a new goal")
	}
	if !m.now().Before(w.deadline) || m.now().Sub(w.created) >= Lifetime {
		m.retire(w)
		return Result{}, fmt.Errorf("workbench lifetime reached; start a new goal")
	}
	w.touched = m.now()
	result, err := m.engine.Execute(ctx, w.runtime, w.name, request)
	if err != nil {
		// An uncertain command result may have side effects. Destroy the workspace
		// and retain a tombstone; never label this as a retryable capacity refusal.
		m.retire(w)
		return Result{}, fmt.Errorf("workbench execution was interrupted; nothing was replayed: %w", err)
	}
	w.touched = m.now()
	return result, nil
}
func (m *Manager) create(ctx context.Context, w *workspace) error {
	runtime, err := m.engine.Ready(ctx)
	if err != nil {
		return err
	}
	previous, err := w.reservation.previous()
	if err != nil {
		return err
	}
	if previous.Name != "" {
		// A process died without finishing cleanup. Only the recorded Docker
		// daemon and exact container may reconcile it; no broad container sweep.
		if previous.DockerID != runtime.ID {
			return ErrInterrupted
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err = m.engine.Remove(cleanup, runtime, previous.Name); err != nil {
			return ErrInterrupted
		}
		if err = os.WriteFile(filepath.Join(m.root, "workbench-ended-"+previous.Key), []byte("interrupted\n"), 0600); err != nil {
			return err
		}
		if err = w.reservation.clear(); err != nil {
			return err
		}
		return ErrInterrupted
	}
	if capacity, ok := m.engine.(interface {
		Headroom(context.Context, Runtime) error
	}); ok {
		if err := capacity.Headroom(ctx, runtime); err != nil {
			return err
		}
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	w.name = "memql-workbench-" + hex.EncodeToString(nonce[:])
	// Journal BEFORE create. A lost Docker reply still leaves an exact cleanup
	// target and cannot create an unaccounted third workspace.
	if err = w.reservation.save(record{Name: w.name, Key: w.key, DockerID: runtime.ID}); err != nil {
		return err
	}
	w.runtime = runtime
	return m.engine.Create(ctx, runtime, w.name, w.key)
}
func (m *Manager) retire(w *workspace) {
	if w.closed {
		return
	}
	w.closed = true
	var endedErr error
	if w.name != "" {
		endedErr = os.WriteFile(filepath.Join(m.root, "workbench-ended-"+w.key), []byte("ended\n"), 0600)
	}
	if w.name != "" {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := m.engine.Remove(cleanup, w.runtime, w.name)
		cancel()
		if err == nil && endedErr == nil {
			_ = w.reservation.clear()
		}
	}
	// On uncertain cleanup the nonempty record fences reuse across restart.
	w.reservation.close()
	m.mu.Lock()
	delete(m.spaces, w.key)
	m.mu.Unlock()
}

// Sweep is called by the worker lifecycle. An active call owns its workspace
// lock; a sweep never cancels that call just because another workspace idled.
func (m *Manager) Sweep() {
	m.mu.Lock()
	root := m.root
	spaces := make([]*workspace, 0, len(m.spaces))
	for _, w := range m.spaces {
		spaces = append(spaces, w)
	}
	m.mu.Unlock()
	if root != "" {
		entries, _ := os.ReadDir(root)
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "workbench-ended-") && len(strings.TrimPrefix(entry.Name(), "workbench-ended-")) == 64 {
				if info, err := entry.Info(); err == nil && info.Mode().IsRegular() && m.now().Sub(info.ModTime()) > 2*Lifetime {
					_ = os.Remove(filepath.Join(root, entry.Name()))
				}
			}
		}
	}
	for _, w := range spaces {
		if !w.mu.TryLock() {
			continue
		}
		if m.now().Sub(w.touched) >= IdleTTL || !m.now().Before(w.deadline) {
			m.retire(w)
		}
		w.mu.Unlock()
	}
}
func (m *Manager) CloseHome(home string) error {
	m.mu.Lock()
	spaces := make([]*workspace, 0, len(m.spaces))
	for _, w := range m.spaces {
		if w.home == home {
			spaces = append(spaces, w)
		}
	}
	m.mu.Unlock()
	for _, w := range spaces {
		if !w.mu.TryLock() {
			return errors.New("workbench still has an active operation")
		}
		m.retire(w)
		w.mu.Unlock()
	}
	return nil
}

// Reconcile visits only the two recorded slots and skips every live lock.
// No request is replayed and no unrecorded container is ever removed.
func (m *Manager) Reconcile(ctx context.Context) error {
	m.mu.Lock()
	root := m.root
	var err error
	if root == "" {
		root, err = capacityRoot()
		if err == nil {
			m.root = root
		}
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	var failures []error
	for slot := 0; slot < Slots; slot++ {
		lease, err := reserveAt(root, slot)
		if errors.Is(err, ErrBusy) {
			continue
		}
		if err != nil {
			failures = append(failures, err)
			continue
		}
		func() {
			defer lease.close()
			previous, err := lease.previous()
			if err != nil {
				failures = append(failures, err)
				return
			}
			if previous.Name == "" {
				return
			}
			runtime, err := m.engine.Ready(ctx)
			if err != nil || runtime.ID != previous.DockerID {
				failures = append(failures, ErrInterrupted)
				return
			}
			if err = m.engine.Remove(ctx, runtime, previous.Name); err != nil {
				failures = append(failures, err)
				return
			}
			if err = os.WriteFile(filepath.Join(root, "workbench-ended-"+previous.Key), []byte("interrupted\n"), 0600); err != nil {
				failures = append(failures, err)
				return
			}
			if err = lease.clear(); err != nil {
				failures = append(failures, err)
			}
		}()
	}
	return errors.Join(failures...)
}
