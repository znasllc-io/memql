package admin

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/pat"
	webtempl "github.com/znasllc-io/memql/component/identity/web/templ"
)

// PATAdapter is the narrow port the admin package uses to drive PAT
// operations from the /admin/tokens handlers. The wiring layer
// satisfies this with a closure around the live *pat.Store.
type PATAdapter interface {
	ListAll(ctx context.Context) ([]pat.PATRow, error)
	LookupById(ctx context.Context, identityId string) (*pat.PATRow, error)
	Revoke(ctx context.Context, identityId string) error
}

// NodeTokenAdapter is the narrow port the admin package uses to
// drive node-token operations from the /admin/tokens handlers
// (memql#350). Mirrors PATAdapter's shape so the two cred families
// share the same admin surface. The wiring layer satisfies this with
// a closure around the live *identity.Store.
type NodeTokenAdapter interface {
	ListAll(ctx context.Context) ([]identity.NodeTokenRow, error)
	LookupById(ctx context.Context, identityId string) (*identity.NodeTokenRow, error)
	Revoke(ctx context.Context, identityId string) error
}

// SetPATAdapter wires the PAT-management dependency. Called once at
// bootstrap. When nil, the routes still mount but render an empty
// list -- the page degrades gracefully rather than 500ing.
func (s *AdminServer) SetPATAdapter(a PATAdapter) {
	if s == nil {
		return
	}
	s.patAdapter = a
}

// SetNodeTokenAdapter wires the node-token management dependency.
// Called once at bootstrap. When nil, the Node-tokens section on
// /admin/tokens renders empty -- the rest of the page (PATs) stays
// functional. memql#350.
func (s *AdminServer) SetNodeTokenAdapter(a NodeTokenAdapter) {
	if s == nil {
		return
	}
	s.nodeTokenAdapter = a
}

// handleTokensList renders /admin/tokens with every PAT in the
// cluster + (memql#350) every node_token identity, joined to the
// owning user's email where applicable. Both adapter ports are
// nil-safe: a binary without one or the other still renders the
// page with the sections it CAN populate.
func (s *AdminServer) handleTokensList(w http.ResponseWriter, r *http.Request) {
	if s.patAdapter == nil && s.nodeTokenAdapter == nil {
		s.Logger.Warn("admin: tokens page requested but no token adapters wired")
		data := webtempl.AdminTokensData{
			Layout: s.layoutData(r, "Tokens", false),
			Flash:  readFlash(r),
		}
		s.render(w, r, "admin/tokens", webtempl.AdminTokens(data))
		return
	}

	users := s.usersById(r.Context())

	patViews := make([]webtempl.AdminPATRow, 0)
	patActive := 0
	if s.patAdapter != nil {
		rows, err := s.patAdapter.ListAll(r.Context())
		if err != nil {
			s.Logger.Warn("admin: tokens list failed", "error", err)
		}
		patViews = make([]webtempl.AdminPATRow, 0, len(rows))
		for _, row := range rows {
			v := webtempl.AdminPATRow{
				ID:     row.ID,
				UserID: row.UserId,
				Label:  row.Label,
				Active: row.Active,
			}
			if u, ok := users[row.UserId]; ok {
				v.OwnerEmail = u.PrimaryEmail
			}
			if !row.LastUsedAt.IsZero() {
				v.LastUsedAt = row.LastUsedAt.UTC().Format(time.RFC3339)
			}
			if !row.CreatedAt.IsZero() {
				v.CreatedAt = row.CreatedAt.UTC().Format(time.RFC3339)
			}
			if row.Active {
				patActive++
			}
			patViews = append(patViews, v)
		}
	}

	nodeViews := make([]webtempl.AdminNodeTokenRow, 0)
	nodeActive := 0
	if s.nodeTokenAdapter != nil {
		rows, err := s.nodeTokenAdapter.ListAll(r.Context())
		if err != nil {
			s.Logger.Warn("admin: node-token list failed", "error", err)
		}
		nodeViews = make([]webtempl.AdminNodeTokenRow, 0, len(rows))
		for _, row := range rows {
			v := webtempl.AdminNodeTokenRow{
				ID:               row.ID,
				NodeID:           row.NodeId,
				NodeType:         row.NodeType,
				Active:           row.Active,
				MintedBy:         row.MintedBy,
				ExpiresAt:        row.ExpiresAt,
				LastConnectAt:    row.LastConnectAt,
				BootstrappedAt:   row.BootstrappedAt,
				BootstrappedFrom: row.BootstrappedFrom,
			}
			// Bootstrap-path rows carry a synthetic mintedBy
			// ("system:node-bootstrap"). Render them as
			// "(bootstrapped)" so the admin can tell a bootstrap-
			// minted row from an operator-CLI mint at a glance.
			if strings.HasPrefix(row.MintedBy, "system:") {
				v.Bootstrapped = true
			} else if u, ok := users[row.MintedBy]; ok {
				v.MintedByEmail = u.PrimaryEmail
			}
			if !row.CreatedAt.IsZero() {
				v.CreatedAt = row.CreatedAt.UTC().Format(time.RFC3339)
			}
			if row.Active {
				nodeActive++
			}
			nodeViews = append(nodeViews, v)
		}
	}

	data := webtempl.AdminTokensData{
		Layout:              s.layoutData(r, "Tokens", false),
		Flash:               readFlash(r),
		Tokens:              patViews,
		TotalCount:          len(patViews),
		ActiveCount:         patActive,
		NodeTokens:          nodeViews,
		NodeTokenTotalCount: len(nodeViews),
		NodeTokenActiveCnt:  nodeActive,
	}
	s.render(w, r, "admin/tokens", webtempl.AdminTokens(data))
}

// handleTokensRevoke revokes any PAT in the cluster. Admin-only;
// audited.
func (s *AdminServer) handleTokensRevoke(w http.ResponseWriter, r *http.Request) {
	if s.patAdapter == nil {
		http.Redirect(w, r, "/admin/tokens?flash=Token+management+not+available&flash_kind=error", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/admin/tokens?flash=Form+submission+failed&flash_kind=error", http.StatusSeeOther)
		return
	}
	id := strings.TrimSpace(r.PostForm.Get("id"))
	if id == "" {
		http.Redirect(w, r, "/admin/tokens?flash=Missing+token+id&flash_kind=error", http.StatusSeeOther)
		return
	}
	row, err := s.patAdapter.LookupById(r.Context(), id)
	if err != nil || row == nil {
		http.Redirect(w, r, "/admin/tokens?flash=Token+not+found&flash_kind=error", http.StatusSeeOther)
		return
	}
	if err := s.patAdapter.Revoke(r.Context(), id); err != nil {
		s.Logger.Warn("admin: revoke token failed", "error", err, "id", id)
		http.Redirect(w, r, "/admin/tokens?flash=Revoke+failed&flash_kind=error", http.StatusSeeOther)
		return
	}
	s.audit(r, identity.AuditEvent{
		Category:    identity.AuditCategoryAdmin,
		Action:      "pat_revoked_admin",
		TargetType:  "identity",
		TargetId:    row.ID,
		TargetEmail: lookupEmail(s.usersById(r.Context()), row.UserId),
		Detail: map[string]any{
			"ownerUserId": row.UserId,
			"label":       row.Label,
		},
		Outcome: identity.AuditOutcomeSuccess,
	})
	http.Redirect(w, r, "/admin/tokens?flash=Token+revoked&flash_kind=success", http.StatusSeeOther)
}

// handleNodeTokensRevoke flips a node_token identity's active flag
// to false. Admin-only; audited as node_token_revoked_admin. memql#350.
//
// The /node/bootstrap pre-mint gate (memql#343) consults `active`
// and refuses to re-mint a revoked row, so a successful revoke
// here means the next time the affected node tries to self-
// bootstrap, identity will reject the request rather than mint a
// fresh JWT. Production deploys that don't use the self-bootstrap
// path get the same revoke surface by virtue of the existing JWT
// expiry + the verifier revocation gate (when wired).
func (s *AdminServer) handleNodeTokensRevoke(w http.ResponseWriter, r *http.Request) {
	if s.nodeTokenAdapter == nil {
		http.Redirect(w, r, "/admin/tokens?flash=Node-token+management+not+available&flash_kind=error", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/admin/tokens?flash=Form+submission+failed&flash_kind=error", http.StatusSeeOther)
		return
	}
	id := strings.TrimSpace(r.PostForm.Get("id"))
	if id == "" {
		http.Redirect(w, r, "/admin/tokens?flash=Missing+token+id&flash_kind=error", http.StatusSeeOther)
		return
	}
	row, err := s.nodeTokenAdapter.LookupById(r.Context(), id)
	if err != nil || row == nil {
		http.Redirect(w, r, "/admin/tokens?flash=Node+token+not+found&flash_kind=error", http.StatusSeeOther)
		return
	}
	// The node_token row is a machine credential, so its write is gated
	// by the memql#2513 credential-actor guard: only a system actor may
	// write it (the revoke read-merges the row, so identityType="node_token"
	// is present in the guard-visible payload). requireAdmin authorized the
	// operator above; the engine write runs under the system credential
	// actor, and the admin's identity is recorded in the audit event below.
	// memql#2549.
	if err := s.nodeTokenAdapter.Revoke(identity.ContextWithSystemCredentialActor(r.Context()), id); err != nil {
		s.Logger.Warn("admin: revoke node token failed", "error", err, "id", id)
		http.Redirect(w, r, "/admin/tokens?flash=Revoke+failed&flash_kind=error", http.StatusSeeOther)
		return
	}
	s.audit(r, identity.AuditEvent{
		Category:   identity.AuditCategoryAdmin,
		Action:     "node_token_revoked_admin",
		TargetType: "identity",
		TargetId:   row.ID,
		Detail: map[string]any{
			"nodeId":   row.NodeId,
			"nodeType": row.NodeType,
			"mintedBy": row.MintedBy,
		},
		Outcome: identity.AuditOutcomeSuccess,
	})
	http.Redirect(w, r, "/admin/tokens?flash=Node+token+revoked&flash_kind=success", http.StatusSeeOther)
}

// usersById is a small index of active users keyed by canonical id.
// The admin tokens list calls this once per page render so the
// per-row email lookup is in-memory.
func (s *AdminServer) usersById(ctx context.Context) map[string]userView {
	out := map[string]userView{}
	users, err := s.queryUsers(ctx, "")
	if err != nil {
		return out
	}
	for _, u := range users {
		out[u.ID] = u
	}
	return out
}

func lookupEmail(idx map[string]userView, userId string) string {
	if u, ok := idx[userId]; ok {
		return u.PrimaryEmail
	}
	return ""
}
