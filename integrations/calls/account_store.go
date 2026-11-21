package calls

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	gowebauthn "github.com/go-webauthn/webauthn/webauthn"
	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

const credentialConcept = "v1:calls:credential"

type credentialRow struct {
	ID, SiteID, KeyID, Label                string
	Credential                              gowebauthn.Credential
	AddedAt, LastUsedAt, RevokedAt, Version time.Time
}

func credentialDigest(raw []byte) string { return callrules.CredentialDigest(raw) }
func credentialGate(raw []byte) string   { return "calls.credential." + credentialDigest(raw) }
func accountGate(key string) string      { return "calls.account." + memql.BareShortId(key) }

func credentialFromRow(r map[string]any) (*credentialRow, error) {
	out := &credentialRow{ID: memql.BareShortId(rowString(r, "id")), SiteID: memql.BareShortId(rowString(r, "siteId")), KeyID: memql.BareShortId(rowString(r, "keyId")), Label: rowString(r, "label"), AddedAt: rowTime(r, "addedAt"), LastUsedAt: rowTime(r, "lastUsedAt"), RevokedAt: rowTime(r, "revokedAt"), Version: rowTime(r, "createdAt")}
	if err := json.Unmarshal([]byte(rowString(r, "credentialData")), &out.Credential); err != nil || len(out.Credential.ID) == 0 || len(out.Credential.PublicKey) == 0 {
		return nil, fmt.Errorf("calls: stored public credential is invalid")
	}
	return out, nil
}

func (s *Store) credentialByDigest(ctx context.Context, raw []byte) (*credentialRow, error) {
	call, err := langparser.RenderCall("callsCredentialByDigest", map[string]any{"credentialDigest": credentialDigest(raw)})
	rows, err := s.readAsCluster(auth.ContextWithInternalOrigin(ctx), "a public credential", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("calls: credential is not unique")
	}
	return credentialFromRow(rows[0])
}
func (s *Store) credentialByID(ctx context.Context, owner, rowID string) (*credentialRow, error) {
	call, err := langparser.RenderCall("callsCredentialById", map[string]any{"id": memql.BareShortId(rowID)})
	rows, err := s.readAs(auth.ContextWithInternalOrigin(ctx), owner, "the public credential", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return credentialFromRow(rows[0])
}
func (s *Store) credentialsForKey(ctx context.Context, owner, keyID string) ([]credentialRow, error) {
	call, err := langparser.RenderCall("callsCredentialsForKey", map[string]any{"keyId": memql.BareShortId(keyID)})
	rows, err := s.readAs(auth.ContextWithInternalOrigin(ctx), owner, "the key's public credentials", call, err)
	if err != nil {
		return nil, err
	}
	out := make([]credentialRow, 0, len(rows))
	for _, r := range rows {
		c, e := credentialFromRow(r)
		if e != nil {
			return nil, e
		}
		out = append(out, *c)
	}
	return out, nil
}
func (s *Store) createCredential(ctx context.Context, owner string, c credentialRow, atTime time.Time) error {
	data, err := json.Marshal(c.Credential)
	if err != nil {
		return fmt.Errorf("calls: cannot encode public credential")
	}
	call, err := langparser.RenderCall("createCallsCredential", map[string]any{"credentialRowId": c.ID, "siteId": c.SiteID, "keyId": c.KeyID, "credentialDigest": credentialDigest(c.Credential.ID), "credentialData": string(data), "label": c.Label, "addedAt": versionStamp(c.AddedAt), "versionTime": versionStamp(atTime)})
	return s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, absent(credentialConcept, c.ID), "the public credential", call, err)
}
func (s *Store) updateCredential(ctx context.Context, owner string, c credentialRow, fields map[string]any, now time.Time) error {
	fields["credentialRowId"] = c.ID
	fields["versionTime"] = versionStamp(memql.VersionTimeAfter(c.Version, now))
	call, err := langparser.RenderCall("updateCallsCredential", fields)
	return s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, at(credentialConcept, c.ID, c.Version), "the public credential", call, err)
}
func (s *Store) updateAccountSession(ctx context.Context, owner string, row *sessionRow, fields map[string]any, now time.Time) error {
	fields["sessionId"] = row.ID
	fields["versionTime"] = versionStamp(memql.VersionTimeAfter(row.Version, now))
	call, err := langparser.RenderCall("updateCallsSession", fields)
	return s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, at(sessionConcept, row.ID, row.Version), "the account session", call, err)
}

// authenticatedKey is the account boundary shared with private history. It
// trusts only the cookie and site, and deliberately permits a frozen key to
// inspect its own records without granting it another call.
func (s *Service) authenticatedKey(ctx context.Context, sess callrules.Session) (keyHolder, error) {
	site, err := checkSite(sess.Site)
	if err != nil {
		return keyHolder{}, callrules.ErrNoSession
	}
	row, pepper, err := s.session(ctx, site, sess.Token, s.now())
	if err != nil {
		return keyHolder{}, err
	}
	if row.KeyID == "" {
		return keyHolder{}, callrules.ErrNoSession
	}
	key, err := s.store.keyByID(ctx, site.OwnerUserID, row.KeyID)
	if err != nil {
		return keyHolder{}, err
	}
	if key == nil || key.SiteID != site.SiteID {
		return keyHolder{}, callrules.ErrNoSession
	}
	return keyHolder{site: site, row: row, pepper: pepper}, nil
}

// Every session read, including rechecks under room gates, rejects revoked
// authentication. A revoke cannot leave a queue poll or media-token renewal
// using a session that was read before the credential changed.
func (s *Store) checkedSessionRow(ctx context.Context, owner string, row *sessionRow) (*sessionRow, error) {
	if row.KeyID != "" {
		key, err := s.keyByID(ctx, owner, row.KeyID)
		if err != nil {
			return nil, err
		}
		if key != nil && key.Status == "retired" {
			return nil, nil
		}
	}
	if row.CredentialID == "" {
		if row.KeyID != "" {
			key, err := s.keyByID(ctx, owner, row.KeyID)
			if err != nil {
				return nil, err
			}
			if key != nil && key.Kind == "guest" && key.UserHandle != "" {
				credentials, err := s.credentialsForKey(ctx, owner, key.ID)
				if err != nil {
					return nil, err
				}
				if len(credentials) > 0 {
					return nil, nil
				}
			}
		}
		return row, nil
	}
	cred, err := s.credentialByID(ctx, owner, row.CredentialID)
	if err != nil {
		return nil, err
	}
	if cred == nil || cred.SiteID != row.SiteID || cred.KeyID != row.KeyID || !cred.RevokedAt.IsZero() {
		return nil, nil
	}
	return row, nil
}

// Revocation has already committed. Slow or unavailable media cannot undo it;
// reconciliation sees the same credential binding and retries after this short
// best-effort path. No account/credential gate spans a provider call.
func (s *Service) disconnectCredentialSeats(ctx context.Context, site callrules.SiteRef, credentialID string) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	call, err := langparser.RenderCall("callsSeatsForCredential", map[string]any{"credentialId": credentialID})
	rows, err := s.store.readAs(auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(cleanup)), site.OwnerUserID, "seats authenticated by the revoked credential", call, err)
	if err != nil {
		return
	}
	for _, r := range rows {
		if cleanup.Err() != nil {
			return
		}
		seat := seatFromRow(r)
		room, err := s.store.roomByID(cleanup, site.OwnerUserID, seat.RoomID)
		if err != nil || room == nil || room.SiteID != site.SiteID || !room.open() {
			continue
		}
		if seat.occupying() {
			if err := s.disconnect(cleanup, *room, seat.Identity); err != nil {
				continue
			}
		}
		_, ended, err := s.leaveGated(cleanup, site.OwnerUserID, *room, seat.ID)
		if err == nil && ended {
			_ = s.deleteMediaRoom(cleanup, *room)
		}
	}
}
