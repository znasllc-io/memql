package calls

import (
	"context"
	"testing"
	"time"

	lkauth "github.com/livekit/protocol/auth"
	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func TestMediaSignalDatabaseRefreshedWebhookIssuerKeepsNativeAuthority(t *testing.T) {
	cw, host, start := mediaSignalFixture(t, "signal-refresh-key")
	if cw == nil {
		return
	}
	ctx := context.Background()
	room, server, api, _, proof, err := cw.svc.authenticateMediaSignal(ctx, start.Join.Pass)
	require.NoError(t, err)
	sealed, err := cw.svc.store.resolveSecret(ctx, SecretMediaWebhook)
	require.NoError(t, err)
	webhook, err := ParseMediaCredential(sealed)
	require.NoError(t, err)
	require.NotEqual(t, api.Key, webhook.Key, "fixture must exercise the alternate issuer")
	mint := func(credential MediaCredential, identity, admission string) string {
		t.Helper()
		attrs := map[string]string{}
		if admission != "" {
			attrs[mediaAdmissionAttribute] = admission
		}
		token, err := MintPass(credential.Key, credential.Secret, callrules.KindPerson, room.MediaRoom, identity, "Refreshed participant", attrs, time.Minute)
		require.NoError(t, err)
		return token
	}
	refreshed := mint(webhook, start.Join.Identity, proof)
	require.NoError(t, cw.svc.CheckMediaSignal(ctx, refreshed))
	lease, err := cw.svc.BeginMediaSignal(ctx, refreshed)
	require.NoError(t, err)
	upstream, err := lkauth.ParseAPIToken(lease.UpstreamToken)
	require.NoError(t, err)
	require.Equal(t, api.Key, upstream.APIKey(), "a refreshed alternate issuer never changes the upstream/control credential")
	_, grants, err := upstream.Verify(api.Secret)
	require.NoError(t, err)
	require.Equal(t, proof, grants.Attributes[mediaAdmissionAttribute])
	require.Equal(t, start.Join.Identity, grants.Identity)
	require.NoError(t, lease.Confirm(ctx))
	require.NoError(t, lease.Close(ctx, true))

	for name, token := range map[string]string{
		"missing native proof": mint(webhook, start.Join.Identity, ""),
		"forged native proof":  mint(webhook, start.Join.Identity, proof+"00"),
		"different identity":   mint(webhook, "unapproved-identity", proof),
		"unconfigured issuer":  mint(MediaCredential{Key: "unconfigured-refresh-key", Secret: webhook.Secret}, start.Join.Identity, proof),
		"wrong signing secret": mint(MediaCredential{Key: webhook.Key, Secret: "incorrect-refresh-signing-secret"}, start.Join.Identity, proof),
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, cw.svc.CheckMediaSignal(ctx, token), "media_admission_invalid")
		})
	}
	// An otherwise valid webhook-signed proof must not expand the trust of a
	// different registered media server or a reconfigured bootstrap binding.
	for _, altered := range []MediaServer{
		{ID: "other-media-server", CredentialSecret: SecretMediaBootstrap},
		{ID: BootstrapMediaServerID, CredentialSecret: "calls.media.replacement"},
	} {
		_, err = cw.svc.mediaSignalSigningCredential(ctx, altered, api, webhook.Key)
		require.ErrorContains(t, err, "media_admission_invalid")
	}
	_, err = cw.svc.mediaSignalSigningCredential(ctx, server, api, "unconfigured-refresh-key")
	require.ErrorContains(t, err, "media_admission_invalid")

	row, _, err := cw.svc.session(ctx, cw.ref, host.Token, cw.clock.Now())
	require.NoError(t, err)
	release, err := cw.svc.gates(ctx, accountGate(row.KeyID))
	require.NoError(t, err)
	err = cw.svc.store.updateAccountSession(ctx, cw.owner, row, map[string]any{"endedAt": versionStamp(cw.clock.Now())}, cw.clock.Now())
	release()
	require.NoError(t, err)
	require.ErrorContains(t, cw.svc.CheckMediaSignal(ctx, refreshed), "media_session_closed")
	_, err = cw.svc.BeginMediaSignal(ctx, refreshed)
	require.ErrorContains(t, err, "media_session_closed", "a provider refresh cannot revive revoked native authority")
}
