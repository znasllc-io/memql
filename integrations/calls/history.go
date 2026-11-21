package calls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

const historyArchiveWindow = 30 * 24 * time.Hour
const historyPreviewWindow = 5 * time.Minute

func historyGate(id string) string { return "calls.history." + id }
func historyID(pepper []byte, site, key, kind, id string) string {
	return callrules.HistoryDigest(pepper, "entry", site, key, kind, id)
}
func historyStamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
func historyOrder(t time.Time, id string) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z") + "|" + id
}

// A projection is per authenticated key, including an anonymous join's unclaimed
// guest key. Legacy seats without a key have no personal history. Source rows remain operational authority; no
// participant name, message, note, credential or media pass is copied here.
func (s *Service) roomHistory(ctx context.Context, site callrules.SiteRef, key string, room roomRow, pepper []byte) (historyRow, error) {
	if room.SiteID != site.SiteID || room.OwnerUserID != site.OwnerUserID {
		return historyRow{}, callrules.ErrNotFound
	}
	kind, source := "room", room.ID
	if room.ScheduledCallID != "" {
		kind, source = "booking", room.ScheduledCallID
	}
	role := "guest"
	if room.KeyID == key {
		role = "host"
	}
	row := historyRow{SiteID: site.SiteID, OwnerUserID: site.OwnerUserID, KeyID: key, SourceKind: kind, SourceID: source, Title: callTitle(room.Title, room.OpenedAt), CanRename: room.KeyID == key, Role: role, Status: "active", StartedAt: room.OpenedAt, EndedAt: room.EndedAt}
	if room.KeyID == key {
		if room.FundingMode == usageMeteredMode {
			config, err := decodeUsageConfig(room.UsageConfig)
			if err != nil {
				return historyRow{}, err
			}
			row.ConfiguredInsights, row.ConfiguredResources = &config.Insights, &config.Resources
		}
		if room.OriginalHostKind == string(callrules.KindPerson) || room.OriginalHostKind == string(callrules.KindAgent) {
			agent := room.OriginalHostKind == string(callrules.KindAgent)
			row.AgentHosted = &agent
		}
	}
	if !room.open() {
		if room.KeyID == key {
			row.CostCents = room.UsageCostCents
		}
		row.Status = "ended"
	}
	row.ID = historyID(pepper, site.SiteID, key, kind, source)
	row.OrderKey = historyOrder(room.OpenedAt, row.ID)
	return row, nil
}
func (s *Service) bookingHistory(ctx context.Context, site callrules.SiteRef, key string, b bookingRow, pepper []byte) (historyRow, error) {
	if b.SiteID != site.SiteID || b.KeyID != key {
		return historyRow{}, callrules.ErrNotFound
	}
	if b.RoomID != "" {
		room, err := s.store.roomByID(ctx, site.OwnerUserID, b.RoomID)
		if err != nil {
			return historyRow{}, err
		}
		if room != nil {
			return s.roomHistory(ctx, site, key, *room, pepper)
		}
	}
	row := historyRow{SiteID: site.SiteID, OwnerUserID: site.OwnerUserID, KeyID: key, SourceKind: "booking", SourceID: b.ID, Title: callTitle(b.Title, b.Start), CanRename: true, Role: "host", Status: "scheduled", StartedAt: b.Start}
	if b.Status == callrules.BookingCancelled {
		row.Status = "cancelled"
		row.EndedAt = b.Version
	} else if b.Status == callrules.BookingEnded {
		row.Status = "ended"
		// No room ever opened: there is no elapsed call duration to count.
	}
	row.ID = historyID(pepper, site.SiteID, key, "booking", b.ID)
	row.OrderKey = historyOrder(b.Start, row.ID)
	return row, nil
}

// upsertHistory never recreates a purged entry. The opaque receipt has no source
// or key fields; retaining it is the minimal durable evidence that a backfill must
// skip this projection. All writes and physical purge share this exact gate.
func (s *Service) upsertHistory(ctx context.Context, row historyRow) (*historyRow, error) {
	release, err := s.gates(ctx, historyGate(row.ID))
	if err != nil {
		return nil, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	now := s.now()
	prior, err := s.store.historyByID(fresh, row.OwnerUserID, row.ID)
	if err != nil {
		return nil, err
	}
	receipt, err := s.store.historyReceiptByID(fresh, row.OwnerUserID, row.ID)
	if err != nil {
		return nil, err
	}
	if prior == nil && receipt != nil && !receipt.PurgeAt.IsZero() && !now.Before(receipt.PurgeAt) {
		return nil, nil
	}
	if prior == nil {
		if err = s.store.createHistory(fresh, row.OwnerUserID, row, now); err != nil {
			return nil, err
		}
		return &row, nil
	}
	if prior.SiteID != row.SiteID || prior.KeyID != row.KeyID || prior.SourceKind != row.SourceKind || prior.SourceID != row.SourceID {
		return nil, callrules.ErrForbidden
	}
	row.OrderKey, row.ArchivedAt, row.PurgeAt, row.Version = prior.OrderKey, prior.ArchivedAt, prior.PurgeAt, prior.Version
	// Once archived, the confirmed snapshot is immutable until restored. Renaming
	// another participant's operational room cannot silently change this archive.
	if !prior.ArchivedAt.IsZero() {
		return prior, nil
	}
	// These are immutable facts about this same source. Operational cleanup may
	// remove the original seat; losing that source must not erase a known fact or
	// repeatedly rewrite the projection. Purge receipts are still checked above.
	if row.ConfiguredInsights == nil {
		row.ConfiguredInsights = prior.ConfiguredInsights
	}
	if row.ConfiguredResources == nil {
		row.ConfiguredResources = prior.ConfiguredResources
	}
	if row.AgentHosted == nil {
		row.AgentHosted = prior.AgentHosted
	}
	if equalOptionalCents(prior.CostCents, row.CostCents) && equalHistoryBool(prior.ConfiguredInsights, row.ConfiguredInsights) && equalHistoryBool(prior.ConfiguredResources, row.ConfiguredResources) && equalHistoryBool(prior.AgentHosted, row.AgentHosted) && prior.Title == row.Title && prior.Role == row.Role && prior.Status == row.Status && prior.StartedAt.Equal(row.StartedAt) && prior.EndedAt.Equal(row.EndedAt) {
		prior.CanRename = row.CanRename
		return prior, nil
	}
	if err = s.store.updateHistory(fresh, row.OwnerUserID, *prior, historyFields(row), now); err != nil {
		return nil, err
	}
	return &row, nil
}

// repairHistory does at most twenty source rows in one invocation. Its durable
// per-key cursor runs rooms, bookings and seats; after the first full cycle it
// continues repairing missed event writes without claiming a partial total.
func (s *Service) repairHistory(ctx context.Context, h keyHolder) (bool, error) {
	syncID := callrules.HistoryDigest(h.pepper, "repair", h.site.SiteID, h.row.KeyID)
	fresh := memql.ContextWithFreshRead(ctx)
	state, err := s.store.historySyncByID(fresh, h.site.OwnerUserID, syncID)
	if err != nil {
		return false, err
	}
	if state == nil {
		state = &historySync{ID: syncID, SiteID: h.site.SiteID, KeyID: h.row.KeyID, Phase: "rooms"}
	}
	if state.SiteID != h.site.SiteID || state.KeyID != h.row.KeyID {
		return false, callrules.ErrForbidden
	}
	priorVersion := state.Version
	rows, next, err := s.store.historySources(fresh, h.site.OwnerUserID, *state)
	if err != nil {
		return false, err
	}
	for _, raw := range rows {
		var row historyRow
		var source any
		switch state.Phase {
		case "rooms":
			room := roomFromRow(raw)
			source = room
			row, err = s.roomHistory(fresh, h.site, h.row.KeyID, room, h.pepper)
		case "bookings":
			booking := bookingFromRow(raw)
			source = booking
			row, err = s.bookingHistory(fresh, h.site, h.row.KeyID, booking, h.pepper)
		case "seats":
			seat := seatFromRow(raw)
			source = seat
			if !seat.JoinedAt.IsZero() {
				room, e := s.store.roomByID(fresh, h.site.OwnerUserID, seat.RoomID)
				err = e
				if room != nil {
					row, err = s.roomHistory(fresh, h.site, h.row.KeyID, *room, h.pepper)
				}
			}
		}
		if err != nil && !errors.Is(err, callrules.ErrNotFound) {
			return false, err
		}
		if row.ID != "" {
			if _, err = s.upsertHistory(fresh, row); err != nil {
				return false, err
			}
			s.acknowledgeHistorySource(fresh, h.site.OwnerUserID, h.row.KeyID, source)
		}
	}
	state.AfterID = next
	if next == "" {
		state.AfterID = ""
		switch state.Phase {
		case "rooms":
			state.Phase = "bookings"
		case "bookings":
			state.Phase = "seats"
		case "seats":
			state.Phase = "rooms"
			state.Complete = true
		}
	}
	// Do not hold the repair gate while taking entry gates: the bounded gate
	// pool must remain usable by concurrent account reads. Entry upserts are
	// idempotent; only cursor advancement needs this short final serialization.
	release, err := s.gates(ctx, "calls.history-sync."+syncID)
	if err != nil {
		return false, err
	}
	defer release()
	current, err := s.store.historySyncByID(fresh, h.site.OwnerUserID, syncID)
	if err != nil {
		return false, err
	}
	if current != nil && !current.Version.Equal(priorVersion) {
		return !current.Complete, nil
	}
	if err = s.store.writeHistorySync(fresh, h.site.OwnerUserID, *state, s.now()); err != nil {
		return false, err
	}
	return !state.Complete, nil
}

type historyCursor struct {
	Scope, After string
	Until        int64
}

func historyScope(h keyHolder, r callrules.HistoryRequest) string {
	return callrules.HistoryDigest(h.pepper, "page-scope", h.site.SiteID, h.row.KeyID, fmt.Sprint(r.Archive), r.Search, r.Role, r.Status, r.From, r.To)
}
func validateHistoryRequest(r *callrules.HistoryRequest) (time.Time, time.Time, error) {
	if r.Limit == 0 {
		r.Limit = 20
	}
	if r.Period == 0 {
		r.Period = 30
	}
	if r.Limit < 1 || r.Limit > 50 || (r.Period != 7 && r.Period != 30 && r.Period != 90) || len(r.Cursor) > 4096 || len(r.Search) > 256 || !utf8.ValidString(r.Search) {
		return time.Time{}, time.Time{}, callrules.ErrInvalid
	}
	if r.Role != "" && r.Role != "host" && r.Role != "guest" {
		return time.Time{}, time.Time{}, callrules.ErrInvalid
	}
	if r.Status != "" && r.Status != "active" && r.Status != "ended" && r.Status != "scheduled" && r.Status != "cancelled" {
		return time.Time{}, time.Time{}, callrules.ErrInvalid
	}
	var from, to time.Time
	var err error
	if r.From != "" {
		from, err = time.Parse(time.RFC3339Nano, r.From)
		if err != nil {
			return from, to, callrules.ErrInvalid
		}
	}
	if r.To != "" {
		to, err = time.Parse(time.RFC3339Nano, r.To)
		if err != nil {
			return from, to, callrules.ErrInvalid
		}
	}
	if !from.IsZero() && !to.IsZero() && !from.Before(to) {
		return from, to, callrules.ErrInvalid
	}
	r.Search = strings.ToLower(strings.TrimSpace(r.Search))
	return from, to, nil
}

func (s *Service) History(ctx context.Context, sess callrules.Session, req callrules.HistoryRequest) (callrules.HistoryPage, error) {
	none := callrules.HistoryPage{}
	from, to, err := validateHistoryRequest(&req)
	if err != nil {
		return none, err
	}
	h, err := s.authenticatedKey(ctx, sess)
	if err != nil {
		return none, err
	}
	scope := historyScope(h, req)
	after := ""
	if req.Cursor != "" {
		payload, ok := callrules.OpenHistoryToken(h.pepper, "cursor", req.Cursor)
		var c historyCursor
		if !ok || json.Unmarshal(payload, &c) != nil || c.Scope != scope || s.now().Unix() >= c.Until {
			return none, callrules.Refuse(callrules.ErrInvalid, "cursor_invalid", "refresh the history list")
		}
		after = c.After
	}
	if err := s.observeCurrentHistory(ctx, h); err != nil {
		return none, err
	}
	collecting, err := s.repairHistory(ctx, h)
	if err != nil {
		return none, err
	}
	out := callrules.HistoryPage{AccountID: h.row.KeyID, Calls: []callrules.HistoryCall{}, Collecting: collecting, Overview: callrules.HistoryOverview{Currency: "USD", PeriodDays: req.Period, Series: []callrules.HistorySeries{}}}
	out.CurrentCall, err = s.currentHistoryCall(ctx, h)
	if err != nil {
		return none, err
	}
	rows, more, err := s.store.historyForKey(ctx, h.site.OwnerUserID, h.site.SiteID, h.row.KeyID, after, req.Archive)
	if err != nil {
		return none, err
	}
	processed := 0
	for _, row := range rows {
		processed++
		after = row.OrderKey
		if !row.PurgeAt.IsZero() && !s.now().Before(row.PurgeAt) {
			continue
		}
		current, err := s.refreshHistory(ctx, h, row)
		if err != nil {
			return none, err
		}
		if current == nil {
			continue
		}
		row = *current
		if req.Role != "" && row.Role != req.Role || req.Status != "" && row.Status != req.Status || req.Search != "" && !strings.Contains(strings.ToLower(row.Title), req.Search) || !from.IsZero() && row.StartedAt.Before(from) || !to.IsZero() && !row.StartedAt.Before(to) {
			continue
		}
		out.Calls = append(out.Calls, s.historyReply(h, row))
		if len(out.Calls) == req.Limit {
			break
		}
	}
	if processed < len(rows) || more {
		data, _ := json.Marshal(historyCursor{Scope: scope, After: after, Until: s.now().Add(30 * time.Minute).Unix()})
		out.NextCursor = callrules.SealHistoryToken(h.pepper, "cursor", data)
	}
	pending, err := s.store.historyPendingForKey(memql.ContextWithFreshRead(ctx), h.site.OwnerUserID, h.site.SiteID, h.row.KeyID)
	if err != nil {
		return none, err
	}
	out.Collecting = collecting || pending
	if !out.Collecting {
		out.Overview, err = s.historyOverview(ctx, h, req.Period)
		if err != nil {
			return none, err
		}
		pending, err = s.store.historyPendingForKey(memql.ContextWithFreshRead(ctx), h.site.OwnerUserID, h.site.SiteID, h.row.KeyID)
		if err != nil {
			return none, err
		}
		if pending {
			out.Collecting = true
			out.Overview = callrules.HistoryOverview{Currency: "USD", PeriodDays: req.Period, Series: []callrules.HistorySeries{}}
		}
	}
	if out.Overview.Usage == nil {
		out.Overview.Usage = historyUsageStatus("incomplete")
	}
	if err = s.historyLedger(ctx, h, &out.Overview); err != nil {
		return none, err
	}
	// Projection repair and ledger reads may yield to other requests. A revoked
	// credential or logged-out session must not receive a previously authorized
	// private balance/history snapshot after that work completes.
	current, err := s.authenticatedKey(memql.ContextWithFreshRead(ctx), sess)
	if err != nil {
		return none, err
	}
	if current.row.ID != h.row.ID || current.row.KeyID != h.row.KeyID || current.row.CredentialID != h.row.CredentialID {
		return none, callrules.ErrNoSession
	}
	if err = s.delegatedEffectAllowed(memql.ContextWithFreshRead(ctx), current.site, current.row.KeyID); err != nil {
		return none, err
	}
	return out, nil
}

func (s *Service) refreshHistory(ctx context.Context, h keyHolder, row historyRow) (*historyRow, error) {
	if row.SiteID != h.site.SiteID || row.KeyID != h.row.KeyID || row.OwnerUserID != h.site.OwnerUserID {
		return nil, callrules.ErrForbidden
	}
	if !row.ArchivedAt.IsZero() {
		return &row, nil
	}
	var current historyRow
	var source any
	var err error
	if row.SourceKind == "room" {
		room, e := s.store.roomByID(ctx, h.site.OwnerUserID, row.SourceID)
		if e != nil {
			return nil, e
		}
		if room == nil {
			return &row, nil
		}
		source = *room
		current, err = s.roomHistory(ctx, h.site, h.row.KeyID, *room, h.pepper)
	} else {
		b, e := s.store.bookingByID(ctx, h.site.OwnerUserID, row.SourceID)
		if e != nil {
			return nil, e
		}
		if b == nil {
			return &row, nil
		}
		if b.KeyID == h.row.KeyID {
			source = *b
			current, err = s.bookingHistory(ctx, h.site, h.row.KeyID, *b, h.pepper)
		} else if b.RoomID != "" {
			room, e := s.store.roomByID(ctx, h.site.OwnerUserID, b.RoomID)
			if e != nil {
				return nil, e
			}
			if room != nil {
				current, err = s.roomHistory(ctx, h.site, h.row.KeyID, *room, h.pepper)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if current.ID == "" {
		return &row, nil
	}
	if current.CanRename && current.Status == "active" {
		seat, room, err := s.seatAndRoom(ctx, h.site.OwnerUserID, h.row)
		if err != nil {
			return nil, err
		}
		current.CanRename = false
		if seat != nil && room != nil && seat.Host && seat.occupying() && room.open() {
			sourceID := room.ID
			if current.SourceKind == "booking" {
				sourceID = room.ScheduledCallID
			}
			current.CanRename = sourceID == current.SourceID
		}
	}
	projected, err := s.upsertHistory(ctx, current)
	if err == nil {
		s.acknowledgeHistorySource(ctx, h.site.OwnerUserID, h.row.KeyID, source)
	}
	return projected, err
}
func (s *Service) historyReply(h keyHolder, row historyRow) callrules.HistoryCall {
	result := callrules.HistoryCall{CostCents: row.CostCents, ID: row.ID, Title: row.Title, Role: row.Role, Status: row.Status, StartedAt: historyStamp(row.StartedAt), EndedAt: historyStamp(row.EndedAt), ArchivedAt: historyStamp(row.ArchivedAt), DeleteAt: historyStamp(row.PurgeAt), CanArchive: row.ArchivedAt.IsZero() && (row.Status == "ended" || row.Status == "cancelled")}
	// Host role alone is not rename authority. The action checks the operational
	// owner's key or actual live host seat; guest projections never get a shortcut.
	result.CanRename = row.CanRename && row.ArchivedAt.IsZero()
	if row.Status == "scheduled" {
		result.ScheduledAt = historyStamp(row.StartedAt)
	}
	if row.Status == "ended" && !row.StartedAt.IsZero() && !row.EndedAt.Before(row.StartedAt) {
		seconds := int64(row.EndedAt.Sub(row.StartedAt) / time.Second)
		result.DurationSeconds = &seconds
	}
	return result
}

// The dashboard's one actionable call is independent of history pagination,
// archive/search filters and reporting periods. These operational reads contain
// only the key's open rooms and bounded upcoming bookings, not all past calls.
// Projection and refresh retain the same private id and live rename authority
// as the history list; missing costs remain unknown.
func (s *Service) currentHistoryCall(ctx context.Context, h keyHolder) (*callrules.HistoryCall, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	rooms, err := s.store.openRoomsForKey(ctx, h.site.OwnerUserID, h.row.KeyID)
	if err != nil {
		return nil, err
	}
	sort.Slice(rooms, func(i, j int) bool {
		if (rooms[i].ID == h.row.RoomID) != (rooms[j].ID == h.row.RoomID) {
			return rooms[i].ID == h.row.RoomID
		}
		if !rooms[i].OpenedAt.Equal(rooms[j].OpenedAt) {
			return rooms[i].OpenedAt.After(rooms[j].OpenedAt)
		}
		return rooms[i].ID < rooms[j].ID
	})
	project := func(row historyRow) (*callrules.HistoryCall, error) {
		stored, err := s.upsertHistory(ctx, row)
		if err != nil || stored == nil {
			return nil, err
		}
		current, err := s.refreshHistory(ctx, h, *stored)
		if err != nil || current == nil {
			return nil, err
		}
		if !current.ArchivedAt.IsZero() || current.Role != "host" || (current.Status != "active" && current.Status != "scheduled") {
			return nil, nil
		}
		if current.Status == "scheduled" && current.StartedAt.Before(s.now()) {
			return nil, nil
		}
		reply := s.historyReply(h, *current)
		return &reply, nil
	}
	for _, room := range rooms {
		if room.KeyID != h.row.KeyID || room.SiteID != h.site.SiteID || room.OwnerUserID != h.site.OwnerUserID {
			return nil, callrules.ErrForbidden
		}
		if !room.open() {
			continue
		}
		row, err := s.roomHistory(ctx, h.site, h.row.KeyID, room, h.pepper)
		if err != nil {
			return nil, err
		}
		if current, err := project(row); err != nil || current != nil {
			return current, err
		}
	}
	bookings, err := s.store.bookingsForKey(ctx, h.site.OwnerUserID, h.row.KeyID)
	if err != nil {
		return nil, err
	}
	sort.Slice(bookings, func(i, j int) bool {
		if !bookings[i].Start.Equal(bookings[j].Start) {
			return bookings[i].Start.Before(bookings[j].Start)
		}
		return bookings[i].ID < bookings[j].ID
	})
	for _, booking := range bookings {
		if booking.KeyID != h.row.KeyID || booking.SiteID != h.site.SiteID {
			return nil, callrules.ErrForbidden
		}
		if booking.Start.Before(s.now()) || booking.status() == callrules.BookingCancelled || booking.status() == callrules.BookingEnded {
			continue
		}
		row, err := s.bookingHistory(ctx, h.site, h.row.KeyID, booking, h.pepper)
		if err != nil {
			return nil, err
		}
		if current, err := project(row); err != nil || current != nil {
			return current, err
		}
	}
	return nil, nil
}

// Overview is bounded independently of pagination. If more than 1,000 retained
// records or twenty native pages in either list need scanning, totals remain
// unavailable rather than turning a partial scan into a claimed account total.
// The page bound also applies when an operator lowers the native result cap.
// Posted-wallet costs are
// filled independently by historyLedger; history duration is not billable time.
func (s *Service) historyOverview(ctx context.Context, h keyHolder, period int) (callrules.HistoryOverview, error) {
	out := callrules.HistoryOverview{Currency: "USD", PeriodDays: period, Series: []callrules.HistorySeries{}}
	out.Usage = historyUsageStatus("limited")
	now := s.now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	first := today.AddDate(0, 0, 1-period)
	counts := make([]int64, period)
	durations := make([]int64, period)
	var hosted, hostedCalls, joinedCalls int64
	var insights, resources, agents int64
	insightsKnown, resourcesKnown, agentsKnown := true, true, true
	seen := make(map[string]bool)
	for _, archived := range []bool{false, true} {
		after := ""
		scanned := 0
		for page := 0; page < 20; page++ {
			rows, more, err := s.store.historyForKey(ctx, h.site.OwnerUserID, h.site.SiteID, h.row.KeyID, after, archived)
			if err != nil {
				return out, err
			}
			for _, row := range rows {
				if seen[row.ID] {
					return out, errIncompleteInventory
				}
				seen[row.ID] = true
				scanned++
				if scanned > 1000 {
					return out, nil
				}
				after = row.OrderKey
				if !row.PurgeAt.IsZero() && !now.Before(row.PurgeAt) || row.StartedAt.Before(first) || !row.StartedAt.Before(today.AddDate(0, 0, 1)) || row.Status == "cancelled" || row.Status == "scheduled" || row.Status == "ended" && row.EndedAt.IsZero() {
					continue
				}
				day := int(row.StartedAt.UTC().Sub(first) / (24 * time.Hour))
				if day < 0 || day >= period {
					continue
				}
				counts[day]++
				if row.Role == "host" {
					hostedCalls++
					countHistoryFlag(row.ConfiguredInsights, &insights, &insightsKnown)
					countHistoryFlag(row.ConfiguredResources, &resources, &resourcesKnown)
					countHistoryFlag(row.AgentHosted, &agents, &agentsKnown)
				} else {
					joinedCalls++
				}
				if row.Status == "ended" && !row.EndedAt.Before(row.StartedAt) {
					seconds := int64(row.EndedAt.Sub(row.StartedAt) / time.Second)
					if row.Role == "host" {
						durations[day] += seconds
						hosted += seconds
					}
				}
			}
			if !more {
				break
			}
			if page == 19 {
				return out, nil
			}
		}
	}
	for day := 0; day < period; day++ {
		out.Series = append(out.Series, callrules.HistorySeries{Date: first.AddDate(0, 0, day).Format("2006-01-02"), Calls: &counts[day], DurationSeconds: &durations[day]})
	}
	out.HostedCalls, out.JoinedCalls = &hostedCalls, &joinedCalls
	out.HostedSeconds = &hosted
	out.Usage = historyUsageStatus("complete")
	if insightsKnown {
		out.Usage.InsightsCalls = &insights
	}
	if resourcesKnown {
		out.Usage.ResourcesCalls = &resources
	}
	if agentsKnown {
		out.Usage.AgentHostedCalls = &agents
	}
	if !insightsKnown || !resourcesKnown || !agentsKnown {
		out.Usage.Status = "incomplete"
	}
	return out, nil
}

// The current/most recent seat is one bounded point read, so a newly ended call
// appears immediately even while the background repair cursor is on another phase.
func (s *Service) observeCurrentHistory(ctx context.Context, h keyHolder) error {
	ctx = memql.ContextWithFreshRead(ctx)
	seat, room, err := s.seatAndRoom(ctx, h.site.OwnerUserID, h.row)
	if err != nil || room == nil || seat == nil {
		return err
	}
	if seat.KeyID != h.row.KeyID || (seat.JoinedAt.IsZero() && room.KeyID != h.row.KeyID) {
		return nil
	}
	row, err := s.roomHistory(ctx, h.site, h.row.KeyID, *room, h.pepper)
	if err != nil {
		return err
	}
	_, err = s.upsertHistory(ctx, row)
	if err != nil {
		return err
	}
	s.acknowledgeHistorySource(ctx, h.site.OwnerUserID, h.row.KeyID, *seat)
	if room.KeyID != "" && room.KeyID != h.row.KeyID {
		ownerRow, e := s.roomHistory(ctx, h.site, room.KeyID, *room, h.pepper)
		if e != nil {
			return e
		}
		if _, e = s.upsertHistory(ctx, ownerRow); e != nil {
			return e
		}
	}
	s.acknowledgeHistorySource(ctx, h.site.OwnerUserID, room.KeyID, *room)
	return nil
}

// A booking's links must not be lost because secondary history projection failed.
// The bounded key-scoped repair repairs that failure later; its log carries no
// title, key, recipient, link, cookie or other caller-supplied content.
func (s *Service) observeBookedHistory(ctx context.Context, h keyHolder, id string) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	b, err := s.store.bookingByID(ctx, h.site.OwnerUserID, id)
	if err == nil && b != nil {
		var row historyRow
		row, err = s.bookingHistory(ctx, h.site, h.row.KeyID, *b, h.pepper)
		if err == nil {
			_, err = s.upsertHistory(ctx, row)
			if err == nil {
				s.acknowledgeHistorySource(ctx, h.site.OwnerUserID, h.row.KeyID, *b)
			}
		}
	}
	if err != nil && s.logger != nil {
		s.logger.Warn("calls: booked history projection awaits bounded repair", "component", pluginName)
	}
}
