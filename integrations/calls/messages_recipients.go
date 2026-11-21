package calls

import (
	"context"
	"encoding/json"
	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"slices"
)

type messageAgentBinding struct {
	GrantID        string `json:"grantId"`
	ApprovalDigest string `json:"approvalDigest"`
}

// One bounded batch per authority kind, never one provider call per recipient.
func (s *Store) messageAuthorityBatch(ctx context.Context, owner, query, concept string, ids []string) (map[string]map[string]any, error) {
	out := map[string]map[string]any{}
	unique := []string{}
	for _, v := range ids {
		if v != "" && !slices.Contains(unique, concept+":"+v) {
			unique = append(unique, concept+":"+v)
		}
	}
	if len(unique) == 0 {
		return out, nil
	}
	if len(unique) > 100 {
		return nil, accountRefusal("messages_unavailable")
	}
	call, e := langparser.RenderCall(query, map[string]any{"ids": unique})
	rows, e := s.readAs(auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(ctx)), owner, "message recipient authority", call, e)
	if e != nil {
		return nil, e
	}
	for _, row := range rows {
		out[memql.BareShortId(rowString(row, "id"))] = row
	}
	return out, nil
}
func (s *Service) messageRecipients(ctx context.Context, room roomRow, seats []seatRow) ([]string, error) {
	credentialIDs, keyIDs, grantIDs := []string{}, []string{}, []string{}
	bindings := map[string]messageAgentBinding{}
	for _, seat := range seats {
		if !seat.occupying() {
			continue
		}
		credentialIDs = append(credentialIDs, seat.CredentialID)
		keyIDs = append(keyIDs, seat.KeyID)
		if seat.Kind == "agent" {
			var b messageAgentBinding
			if len(seat.AgentData) <= 4096 {
				_ = json.Unmarshal([]byte(seat.AgentData), &b)
			}
			bindings[seat.Identity] = b
			grantIDs = append(grantIDs, b.GrantID)
		}
	}
	keys, e := s.store.messageAuthorityBatch(ctx, room.OwnerUserID, "callsMessageKeys", keyConcept, keyIDs)
	if e != nil {
		return nil, e
	}
	creds, e := s.store.messageAuthorityBatch(ctx, room.OwnerUserID, "callsMessageCredentials", credentialConcept, credentialIDs)
	if e != nil {
		return nil, e
	}
	grants, e := s.store.messageAuthorityBatch(ctx, room.OwnerUserID, "callsMessageDelegations", delegationConcept, grantIDs)
	if e != nil {
		return nil, e
	}
	out := []string{}
	for _, seat := range seats {
		if !seat.occupying() || (seat.Kind != "person" && seat.Kind != "agent" && seat.Kind != "avatar") {
			continue
		}
		key := keys[seat.KeyID]
		if key == nil || memql.BareShortId(rowString(key, "siteId")) != room.SiteID {
			continue
		}
		status := rowString(key, "status")
		if status != "" && status != "active" {
			continue
		}
		if seat.CredentialID != "" {
			c := creds[seat.CredentialID]
			if c == nil || memql.BareShortId(rowString(c, "siteId")) != room.SiteID || memql.BareShortId(rowString(c, "keyId")) != seat.KeyID || !rowTime(c, "revokedAt").IsZero() {
				continue
			}
		} else if rowString(key, "kind") == "guest" && rowString(key, "userHandle") != "" {
			continue
		}
		if seat.Kind == "agent" {
			b := bindings[seat.Identity]
			row := grants[b.GrantID]
			if row == nil || len(b.ApprovalDigest) != 64 {
				continue
			}
			d, e := delegationFromRow(row)
			if e != nil {
				return nil, e
			}
			if d.SiteID != room.SiteID || d.KeyID != seat.KeyID || d.State.CredentialID != seat.CredentialID || !d.State.RevokedAt.IsZero() || !s.now().Before(d.State.Request.ExpiresAt) || delegationApprovalDigest(d) != b.ApprovalDigest {
				continue
			}
		}
		out = append(out, seat.Identity)
	}
	return s.messageFamilyRecipients(ctx, room, seats, out)
}
