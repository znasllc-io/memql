package calls

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

var errPortraitCursor = errors.New("portrait maintenance continuation invalid")

const portraitUnusedGrace = 24 * time.Hour
const portraitRetiredMetadataGrace = 24 * time.Hour

// PortraitRetirementReceipt pins canonical bytes to their exact provider version.
type PortraitRetirementReceipt struct {
	Size int64
	ETag string
}

// PortraitLifecycleStorage identifies one native-composed owned object; neither this port nor
// the public upload/read routes accept an arbitrary URL to retire.
type PortraitLifecycleStorage interface {
	PortraitStorage
	InspectRetirement(context.Context, string, string, string, int64) (PortraitRetirementReceipt, error)
	Retire(context.Context, string, string, string, PortraitRetirementReceipt, string) error
}
type portraitMaintenanceState struct {
	mu      sync.Mutex
	cursor  string
	recent  []string
	storage PortraitLifecycleStorage
}

func (i *Integration) ConfigurePortraitMaintenance(storage PortraitLifecycleStorage) {
	if i == nil || i.service == nil || i.service.portraitMaintenance == nil {
		return
	}
	state := i.service.portraitMaintenance
	state.mu.Lock()
	defer state.mu.Unlock()
	state.storage = storage
}

// ReconcilePortraits scans one two-row native keyset page; both objects may perform
// provider I/O in a pass. Moving the cursor past every attempted row prevents a
// persistently unavailable object from starving later assets. The retiring barrier
// survives restarts and prevents approval, read or upload adoption during lost replies.
func (s *Service) ReconcilePortraits(ctx context.Context) error {
	state := s.portraitMaintenance
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	storage := state.storage
	if local, ok := s.portraitStorage.(PortraitLifecycleStorage); ok {
		storage = local
	}
	if storage == nil {
		return nil
	}
	rows, next, err := s.store.portraitMaintenanceCandidates(ctx, state.cursor)
	if err != nil {
		if errors.Is(err, errPortraitCursor) || errors.Is(err, memql.ErrCursorMalformed) || errors.Is(err, memql.ErrCursorSortMismatch) {
			state.cursor = ""
			state.recent = nil
		}
		return err
	}
	if next != "" && slices.Contains(state.recent, next) {
		state.cursor = ""
		state.recent = nil
		return errPortraitCursor
	}
	var first error
	attempts := 0
	for _, row := range rows {
		if err = ctx.Err(); err != nil {
			return err
		}
		p, e := portraitFromRow(row)
		owner := memql.BareShortId(rowString(row, "ownerUserId"))
		if e != nil || owner == "" {
			if first == nil {
				first = accountRefusal("portrait_maintenance_invalid")
			}
			continue
		}
		retiring, e := s.preparePortraitRetirement(ctx, owner, p)
		if e != nil {
			if first == nil {
				first = e
			}
			continue
		}
		if retiring != nil && attempts < 2 {
			attempts++
			if e = s.finishPortraitRetirement(ctx, owner, retiring, storage); e != nil && first == nil {
				first = e
			}
		}
	}
	state.cursor = next
	if next == "" {
		state.recent = nil
	} else {
		if len(state.recent) == 64 {
			state.recent = state.recent[1:]
		}
		state.recent = append(state.recent, next)
	}
	return first
}
func (s *Store) portraitMaintenanceCandidates(ctx context.Context, cursor string) ([]map[string]any, string, error) {
	actor, e := asCluster(ctx)
	if e != nil {
		return nil, "", e
	}
	call, e := langparser.RenderCall("callsPortraitMaintenanceCandidates", nil)
	if e != nil {
		return nil, "", e
	}
	raw, e := s.engine.Execute(memql.ContextWithCursor(memql.ContextWithRootsOnlyRead(memql.ContextWithFreshRead(actor)), cursor), "query "+call)
	if e != nil {
		return nil, "", fmt.Errorf("calls: portrait maintenance scan: %w", scrubbed(e))
	}
	out, rows, e := accountingInventoryPage(raw)
	if e != nil || !uniquePortraitInventoryRows(rows) {
		return nil, "", errPortraitCursor
	}
	next := ""
	if out.GetMeta() != nil {
		next = out.GetMeta().Cursor
		if out.GetMeta().HasMore && next == "" {
			return nil, "", errPortraitCursor
		}
	}
	if next != "" && (next == cursor || len(next) > 8192) {
		return nil, "", errPortraitCursor
	}
	if len(rows) > 2 {
		return nil, "", errPortraitCursor
	}
	return rows, next, nil
}
func (s *Service) preparePortraitRetirement(ctx context.Context, owner string, candidate *portraitRow) (*portraitRow, error) {
	grants, e := s.store.delegations(memql.ContextWithFreshRead(ctx), owner, candidate.SiteID, candidate.KeyID)
	if e != nil {
		return nil, e
	}
	if len(grants) > 100 {
		return nil, accountRefusal("portrait_reference_capacity")
	}
	locks := []string{accountGate(candidate.KeyID), keyGate(candidate.KeyID)}
	ids := []string{}
	for _, d := range grants {
		locks = append(locks, delegationGate(d.ID))
		ids = append(ids, d.ID)
	}
	release, e := s.gates(ctx, locks...)
	if e != nil {
		return nil, e
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	p, e := s.store.portrait(fresh, owner, candidate.ID)
	if e != nil || p == nil {
		return nil, e
	}
	if p.SiteID != candidate.SiteID || p.KeyID != candidate.KeyID || p.Digest != candidate.Digest {
		return nil, accountRefusal("portrait_reference_changed")
	}
	if p.Status == "retired" {
		return nil, nil
	}
	if p.Status == "retiring" {
		return p, nil
	}
	grants, e = s.store.delegations(fresh, owner, p.SiteID, p.KeyID)
	if e != nil {
		return nil, e
	}
	if len(grants) > 100 {
		return nil, accountRefusal("portrait_reference_capacity")
	}
	// A new grant created between inventory and the key gate needs its own gate;
	// retry on a later pass rather than acquiring a second gate-pool connection.
	for _, d := range grants {
		if !slices.Contains(ids, d.ID) {
			return nil, accountRefusal("portrait_reference_changed")
		}
	}
	referenced, e := s.portraitReferenced(fresh, owner, p, grants)
	if e != nil {
		return nil, e
	}
	if referenced {
		if !p.UnusedSince.IsZero() {
			p.UnusedSince = time.Time{}
			e = s.store.savePortrait(fresh, owner, p, s.now())
		}
		return nil, e
	}
	if p.UnusedSince.IsZero() {
		p.UnusedSince = s.now()
		if p.UnusedSince.Before(p.IssuedAt) {
			p.UnusedSince = p.IssuedAt
		}
		return nil, s.store.savePortrait(fresh, owner, p, s.now())
	}
	if s.now().Before(p.UnusedSince.Add(portraitUnusedGrace)) {
		return nil, nil
	}
	p.Status = "retiring"
	p.RetirementID = id.NewShortId()
	if e = s.store.savePortrait(fresh, owner, p, s.now()); e != nil {
		return nil, e
	}
	return p, nil
}
func (s *Service) portraitReferenced(ctx context.Context, owner string, p *portraitRow, grants []*delegationRow) (bool, error) {
	for _, d := range grants {
		if !d.State.RevokedAt.IsZero() || !s.now().Before(d.State.Request.ExpiresAt) {
			continue
		}
		if d.State.Request.PortraitID == p.ID && d.State.Request.PortraitDigest == p.Digest {
			return true, nil
		}
		for _, entry := range d.State.Proposals {
			q := entry.Proposal
			if q.Status == "pending" && s.now().Before(q.ExpiresAt) && q.Grant.PortraitID == p.ID && q.Grant.PortraitDigest == p.Digest {
				return true, nil
			}
		}
	}
	call, e := langparser.RenderCall("callsPortraitOccupiedSeats", map[string]any{"siteId": p.SiteID, "keyId": p.KeyID})
	rows, e := s.store.readCompleteOwnerPage(auth.ContextWithInternalOrigin(ctx), owner, "portrait seat references", call, e)
	if e != nil {
		return false, e
	}
	if len(rows) > 100 {
		return false, accountRefusal("portrait_reference_capacity")
	}
	for _, row := range rows {
		seat := seatFromRow(row)
		if seat.Kind != string(callrules.KindAgent) {
			continue
		}
		presentation := parseAgentPresentation(seat.AgentData)
		if presentation.Purpose == "" {
			return false, accountRefusal("portrait_reference_unavailable")
		}
		if presentation.PortraitID == p.ID && presentation.PortraitDigest == p.Digest {
			return true, nil
		}
	}
	return false, nil
}
func (s *Service) finishPortraitRetirement(ctx context.Context, owner string, p *portraitRow, storage PortraitLifecycleStorage) error {
	// The persisted retirement ID and receipt, once present, are immutable.
	if !p.RetirementReady {
		bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
		receipt, e := storage.InspectRetirement(bounded, owner, portraitObject(owner, p), p.Digest, p.Size)
		cancel()
		if e != nil {
			return accountRefusal("portrait_retirement_unavailable")
		}
		if receipt.Size < 0 || receipt.Size > callrules.PortraitCanonicalBytes || len(receipt.ETag) > 128 {
			return accountRefusal("portrait_retirement_unavailable")
		}
		release, e := s.gates(ctx, accountGate(p.KeyID), keyGate(p.KeyID))
		if e != nil {
			return e
		}
		current, e := s.store.portrait(memql.ContextWithFreshRead(ctx), owner, p.ID)
		if e == nil && (current == nil || current.Status != "retiring" || current.RetirementID != p.RetirementID) {
			e = accountRefusal("portrait_reference_changed")
		}
		if e == nil {
			if !current.RetirementReady {
				current.Size = receipt.Size
				current.RetirementETag = receipt.ETag
				current.RetirementReady = true
				e = s.store.savePortrait(ctx, owner, current, s.now())
			}
			p = current
		}
		release()
		if e != nil {
			return e
		}
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	e := storage.Retire(bounded, owner, portraitObject(owner, p), p.Digest, PortraitRetirementReceipt{Size: p.Size, ETag: p.RetirementETag}, p.RetirementID)
	cancel()
	if e != nil {
		return accountRefusal("portrait_retirement_unavailable")
	}
	release, e := s.gates(ctx, accountGate(p.KeyID), keyGate(p.KeyID))
	if e != nil {
		return e
	}
	defer release()
	current, e := s.store.portrait(memql.ContextWithFreshRead(ctx), owner, p.ID)
	if e != nil {
		return e
	}
	if current == nil || current.RetirementID != p.RetirementID || current.Size != p.Size || current.RetirementETag != p.RetirementETag {
		return accountRefusal("portrait_reference_changed")
	}
	if current.Status == "retired" {
		return nil
	}
	if current.Status != "retiring" {
		return accountRefusal("portrait_reference_changed")
	}
	current.Status = "retired"
	current.RetiredAt = s.now()
	current.PurgeAt = current.RetiredAt.Add(portraitRetiredMetadataGrace)
	return s.store.savePortrait(ctx, owner, current, s.now())
}
