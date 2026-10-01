package email

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
)

const TransportEnv = "MEMQL_EMAIL_TRANSPORT"
const maxCaptureBytes = 1024 * 1024

// CaptureSender stores test mail through the shared engine. It never dials a
// recipient or falls through to another transport. The installation chooses
// capture explicitly; hostname and user role never select it implicitly.
type CaptureSender struct{ store ConfigWriter }

type capturedMessage struct {
	ID        string `json:"id"`
	CreatedAt string `json:"createdAt"`
	AccountID string `json:"accountId,omitempty"`
	From      string `json:"from"`
	FromName  string `json:"fromName"`
	Message
}

func captureContext(ctx context.Context) context.Context {
	ctx = auth.ContextWithAccess(ctx, auth.SystemActor("email-capture"))
	ctx = auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: "email-capture"})
	return auth.ContextWithInternalOrigin(ctx)
}

func (s *CaptureSender) Send(ctx context.Context, msg Message, as SendAs) error {
	if err := msg.Validate(); err != nil {
		return err
	}
	if err := as.Validate(); err != nil {
		return err
	}
	if s == nil || s.store == nil {
		return fmt.Errorf("email: capture storage is unavailable")
	}
	if len(msg.TextBody)+len(msg.HTMLBody) > maxCaptureBytes {
		return fmt.Errorf("email: captured message exceeds 1 MiB")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return fmt.Errorf("email: cannot allocate capture id")
	}
	from, name := resolveIdentity(as, "no-reply@example.test", "MemQL")
	item := capturedMessage{ID: hex.EncodeToString(id[:]), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), AccountID: as.AccountID, From: from, FromName: name, Message: msg}
	body, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("email: cannot encode captured message")
	}
	sealed, _, err := secret.Encrypt(string(body))
	if err != nil {
		return fmt.Errorf("email: cannot encrypt captured message: %w", err)
	}
	_, err = s.store.Execute(captureContext(ctx), renderConfigCall("recordCapturedEmail", map[string]string{"id": item.ID, "encryptedValue": sealed}))
	// Do not wrap an engine error which could contain query arguments.
	if err != nil {
		return fmt.Errorf("email: could not persist captured message")
	}
	return nil
}

func captureSender(sender Sender, ctx context.Context) (*CaptureSender, bool) {
	if lazy, ok := sender.(*LazySender); ok {
		// Capture is an installation value fixed at boot. A status read must
		// not freeze a still-unconfigured external sender in the lazy cache.
		sender = lazy.envResolved
	}
	s, ok := sender.(*CaptureSender)
	return s, ok
}

func (i *Integration) handleInbox(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	// Capture can contain sign-in tokens for arbitrary test recipients. It is
	// an explicitly restricted operator test surface, never an ordinary user's
	// mailbox and never a synthetic sender's opportunity to read credentials.
	if err := configureAuthorized(ctx); err != nil {
		return nil, err
	}
	ac, _ := auth.AccessFromContext(ctx)
	if ac.Synthetic {
		return nil, fmt.Errorf("email: test inbox requires a signed-in operator")
	}
	s, ok := captureSender(i.sender, ctx)
	if !ok {
		return configureResult(map[string]any{"mode": "external", "messages": []capturedMessage{}})
	}
	if s.store == nil {
		return nil, fmt.Errorf("email: capture storage is unavailable")
	}
	since := time.Now().UTC().Add(-7 * 24 * time.Hour).Format(time.RFC3339Nano)
	res, err := s.store.Execute(captureContext(ctx), strings.Replace(renderConfigCall("capturedEmails", map[string]string{"since": since}), "mutation ", "query ", 1))
	if err != nil {
		return nil, fmt.Errorf("email: could not read captured messages")
	}
	items := make([]capturedMessage, 0)
	for _, row := range memql.MaterializeRows(res) {
		sealed, _ := row["encryptedValue"].(string)
		plain, err := secret.Decrypt(sealed)
		if err != nil {
			return nil, fmt.Errorf("email: could not decrypt captured message")
		}
		var item capturedMessage
		if json.Unmarshal([]byte(plain), &item) != nil {
			return nil, fmt.Errorf("email: invalid captured message")
		}
		items = append(items, item)
	}
	if i.logger != nil {
		i.logger.Info("email: operator read test inbox", "actor", ac.UserId, "count", len(items))
	}
	return configureResult(map[string]any{"mode": "capture", "messages": items})
}
