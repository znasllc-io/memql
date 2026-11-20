package calls

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
	"github.com/znasllc-io/memql/integrations/email"
)

// This uses the same factory that attaches encrypted capture storage at boot;
// the call service receives the configured sender, not a fake acknowledging Send.
func TestInvitationCapturePersistsOneEncryptedMessage(t *testing.T) {
	cw, _, _ := invitationWorld(t, "invite-capture")
	if cw == nil {
		return
	}
	t.Setenv(email.TransportEnv, "capture")
	t.Setenv("MEMQL_EMAIL_INSTALLATION", "")
	var sender email.Sender
	for _, plugin := range memql.RegisteredPlugins() {
		if plugin.Name != "email" {
			continue
		}
		provider, err := plugin.Factory(memql.PluginContext{
			Engine: cw.eng, Logger: cw.eng.Logger,
			GateDB:                func() *sql.DB { return sharedBoot.gates },
			ResolveSystemVariable: cw.eng.ResolveSystemVariable,
			ResolveSystemSecret:   cw.eng.ResolveSystemSecret,
		})
		require.NoError(t, err)
		sender = provider.(*email.Integration).SenderAccess()
	}
	require.NotNil(t, sender)
	require.True(t, email.CapturesMessages(sender))
	host, start := cw.started("Host")
	svc := cw.svc.WithInvitationSender(func() email.Sender { return sender })
	req := callrules.InviteRequest{Email: "capture@example.test", Token: start.Call.GuestToken}
	result, err := svc.Invite(context.Background(), host, req)
	require.NoError(t, err)
	require.Equal(t, callrules.InviteResult{Status: "captured"}, result)
	result, err = svc.Invite(context.Background(), host, req)
	require.NoError(t, err)
	require.Equal(t, callrules.InviteResult{Status: "captured", Duplicate: true}, result)

	var stored []memorynodes.MemoryNode
	require.NoError(t, cw.db.NewSelect().Model(&stored).Where("concept = ?", "v1:email:capturedMessage").Scan(context.Background()))
	var expectedIntent string
	for id := range cw.rowsOf(invitationConcept) {
		expectedIntent = "calls-invitation-" + id
	}
	require.NotEmpty(t, expectedIntent)
	var found int
	for _, row := range stored {
		var payload map[string]any
		require.NoError(t, json.Unmarshal(row.Payload, &payload))
		sealed, _ := payload["encryptedValue"].(string)
		plain, err := secret.Decrypt(sealed)
		if err != nil {
			continue
		} // Other tests may use another master key.
		var message email.Message
		require.NoError(t, json.Unmarshal([]byte(plain), &message))
		if message.IntentID != expectedIntent {
			continue
		}
		found++
		id := row.ID
		t.Cleanup(func() {
			_, _ = cw.db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("concept = ? AND id = ?", "v1:email:capturedMessage", id).Exec(context.Background())
		})
		require.NotContains(t, string(row.Payload), req.Email)
		require.NotContains(t, string(row.Payload), start.Call.GuestToken)
		require.NotContains(t, string(row.Payload), start.Call.HostToken)
		require.Equal(t, req.Email, message.To)
		require.Contains(t, message.HTMLBody, "/favicon.png")
		require.Contains(t, message.HTMLBody, "Join call")
		require.Contains(t, message.TextBody, "#j="+start.Call.GuestToken)
		require.False(t, strings.Contains(message.HTMLBody+message.TextBody, start.Call.HostToken))
	}
	require.Equal(t, 1, found, "the real capture transport must persist exactly one encrypted invitation")
}
