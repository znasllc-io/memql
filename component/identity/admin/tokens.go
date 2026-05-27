package admin

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/nodetoken"
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

// NodeTokenAdapter is the narrow port the admin package uses to drive
// node-token operations from /admin/tokens (memql#343). Mirrors
// PATAdapter -- the wiring layer satisfies this with a closure around
// the live *nodetoken.Store. List + lookup + revoke; no Create surface
// because the only creation paths are /node/bootstrap + the operator
// CLI, neither of which routes through the admin web UI.
type NodeTokenAdapter interface {
	ListAll(ctx context.Context) ([]nodetoken.Row, error)
	LookupById(ctx context.Context, identityId string) (*nodetoken.Row, error)
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

// SetNodeTokenAdapter wires the node-token management dependency
// (memql#343). Called once at bootstrap. When nil, the page renders
// the PAT section normally and the node-token section reports "no
// node tokens" -- graceful degradation rather than 500.
func (s *AdminServer) SetNodeTokenAdapter(a NodeTokenAdapter) {
	if s == nil {
		return
	}
	s.nodeTokenAdapter = a
}

// handleTokensList renders /admin/tokens with every PAT + every node
// token in the cluster. The page is a single render with two table
// sections; the data fetches are independent (a failure on one side
// surfaces as an empty section, not a 500).
func (s *AdminServer) handleTokensList(w http.ResponseWriter, r *http.Request) {
	data := webtempl.AdminTokensData{
		Layout: s.layoutData(r, "Personal access tokens", false),
		Flash:  readFlash(r),
	}

	if s.patAdapter == nil {
		s.Logger.Warn("admin: tokens page requested but PAT adapter not wired")
	} else {
		rows, err := s.patAdapter.ListAll(r.Context())
		if err != nil {
			s.Logger.Warn("admin: tokens list failed", "error", err)
		}
		users := s.usersById(r.Context())
		views := make([]webtempl.AdminPATRow, 0, len(rows))
		active := 0
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
				active++
			}
			views = append(views, v)
		}
		data.Tokens = views
		data.TotalCount = len(views)
		data.ActiveCount = active
	}

	if s.nodeTokenAdapter != nil {
		nodeRows, err := s.nodeTokenAdapter.ListAll(r.Context())
		if err != nil {
			s.Logger.Warn("admin: node tokens list failed", "error", err)
		}
		nodeViews := make([]webtempl.AdminNodeTokenRow, 0, len(nodeRows))
		activeNodes := 0
		for _, row := range nodeRows {
			v := webtempl.AdminNodeTokenRow{
				ID:               row.ID,
				NodeID:           row.NodeId,
				NodeType:         row.NodeType,
				MintedBy:         row.MintedBy,
				KeyHashJTI:       row.KeyHash,
				Active:           row.Active,
				BootstrappedFrom: row.BootstrappedFrom,
			}
			if !row.ExpiresAt.IsZero() {
				v.ExpiresAt = row.ExpiresAt.UTC().Format(time.RFC3339)
			}
			if !row.LastConnectAt.IsZero() {
				v.LastConnectAt = row.LastConnectAt.UTC().Format(time.RFC3339)
			}
			if !row.BootstrappedAt.IsZero() {
				v.BootstrappedAt = row.BootstrappedAt.UTC().Format(time.RFC3339)
			}
			if !row.LastBootstrappedAt.IsZero() {
				v.LastBootstrappedAt = row.LastBootstrappedAt.UTC().Format(time.RFC3339)
			}
			if !row.RevokedAt.IsZero() {
				v.RevokedAt = row.RevokedAt.UTC().Format(time.RFC3339)
			}
			if row.Active {
				activeNodes++
			}
			nodeViews = append(nodeViews, v)
		}
		data.NodeTokens = nodeViews
		data.NodeTokenTotalCount = len(nodeViews)
		data.NodeTokenActiveCount = activeNodes
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

// handleNodeTokensRevoke revokes any node-token row in the cluster
// (memql#343). Mirrors handleTokensRevoke's shape; the only structural
// difference is the adapter + audit-action name. Admin-only; the same
// gate the rest of /admin/* uses applies.
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
	if err := s.nodeTokenAdapter.Revoke(r.Context(), id); err != nil {
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
