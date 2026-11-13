package reviewspack

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
)

const requestConcept = "v1:reviews:reviewRequest"

// The durable retry envelope is authenticated and purpose-bound. The bearer
// never appears in a request row in plaintext. Reopening a pending send must
// recover exactly its original bearer and deadline: Campaigns may have already
// accepted that send and return its receipt without sending another message.
type retryEnvelope struct {
	Purpose   string `json:"purpose"`
	RequestID string `json:"requestId"`
	Owner     string `json:"owner"`
	Store     string `json:"store"`
	Account   string `json:"account"`
	Site      string `json:"site"`
	Order     string `json:"order"`
	Product   string `json:"product"`
	Email     string `json:"email"`
	Issued    string `json:"issued"`
	Expires   string `json:"expires"`
	Host      string `json:"host"`
	Token     string `json:"token"`
}

func envelopeFor(row map[string]any, owner, host, token string) retryEnvelope {
	return retryEnvelope{Purpose: "reviews/request-retry/v1", RequestID: asString(row["requestId"]), Owner: owner,
		Store: asString(row["storeId"]), Account: asString(row["accountId"]), Site: asString(row["siteId"]),
		Order: asString(row["orderGid"]), Product: asString(row["productHandle"]), Email: asString(row["email"]),
		Issued: asTime(row["issuedAt"]).Format(time.RFC3339Nano), Expires: asTime(row["expiresAt"]).Format(time.RFC3339Nano), Host: host, Token: token}
}

func sealRetry(row map[string]any, owner, host, token string) (string, error) {
	plain, err := json.Marshal(envelopeFor(row, owner, host, token))
	if err != nil {
		return "", fmt.Errorf("reviews: cannot encode retry envelope")
	}
	cipher, _, err := secret.Encrypt(string(plain))
	if err != nil {
		return "", fmt.Errorf("reviews: retry encryption is unavailable")
	}
	return cipher, nil
}

func openRetry(row map[string]any, owner string) (retryEnvelope, error) {
	var envelope retryEnvelope
	plain, err := secret.Decrypt(asString(row["tokenCiphertext"]))
	if err != nil || json.Unmarshal([]byte(plain), &envelope) != nil {
		return retryEnvelope{}, fmt.Errorf("reviews: the original retry token cannot be recovered; no replacement was issued")
	}
	want := envelopeFor(row, owner, envelope.Host, envelope.Token)
	if envelope != want || envelope.Token == "" || envelope.Host == "" || tokenHash(envelope.Token) != asString(row["tokenHash"]) {
		return retryEnvelope{}, fmt.Errorf("reviews: retry envelope does not match its request")
	}
	return envelope, nil
}

type requestLease interface {
	Context(context.Context) context.Context
	Verify(context.Context) error
	Release()
}

type requestCoordinator func(context.Context, string) (requestLease, error)

func databaseRequestCoordinator(resolve func() *sql.DB) requestCoordinator {
	return func(ctx context.Context, id string) (requestLease, error) {
		db, err := memql.ResolveWriteGatePool(resolve, "reviews request", "wire PluginContext.GateDB")
		if err != nil {
			return nil, err
		}
		return memql.AcquireGateHold(ctx, db, 0, "reviews/request/"+bareID(id, requestConcept+":"))
	}
}

// Use the separate gate pool and lend its transaction to synchronous nested
// Campaigns gates. Reads are fresh; a fence still rejects a stale writer if its
// gate session is lost and another process takes over. Bound each held phase.
func (p *Provider) lockRequest(ctx context.Context, id string) (context.Context, requestLease, func(), error) {
	if p.coordinateRequest == nil {
		return ctx, nil, nil, fmt.Errorf("reviews: request coordination is unavailable")
	}
	bounded, cancel := context.WithTimeout(ctx, 55*time.Second)
	hold, err := p.coordinateRequest(bounded, id)
	if err != nil {
		cancel()
		return ctx, nil, nil, err
	}
	return memql.ContextWithFreshRead(hold.Context(bounded)), hold, func() { hold.Release(); cancel() }, nil
}

func (p *Provider) currentRequest(ctx context.Context, id string) (map[string]any, time.Time, error) {
	rows, err := p.rowsFor(memql.ContextWithFreshRead(ctx), "reviewRequestById", map[string]string{"requestId": bareID(id, requestConcept+":")})
	if err != nil {
		return nil, time.Time{}, err
	}
	if len(rows) == 0 {
		return nil, time.Time{}, nil
	}
	if len(rows) != 1 {
		return nil, time.Time{}, fmt.Errorf("reviews: request lookup is ambiguous")
	}
	at := asTime(rows[0]["createdAt"])
	if at.IsZero() {
		return nil, time.Time{}, fmt.Errorf("reviews: request has no version witness")
	}
	return rows[0], at, nil
}

// Only mutation arguments, never the read envelope's id or createdAt.
func requestWrite(row map[string]any, id string) map[string]any {
	write := map[string]any{"requestId": bareID(id, requestConcept+":")}
	for _, key := range strings.Fields("storeId accountId siteId orderGid orderName productHandle productTitle email tokenHash tokenCiphertext issuedAt expiresAt sentAt sendState sendNote erasedAt") {
		if value, ok := row[key]; ok && value != nil {
			write[key] = value
		}
	}
	return write
}

func (p *Provider) writeRequest(ctx context.Context, hold requestLease, id string, at time.Time, row map[string]any) error {
	if err := hold.Verify(ctx); err != nil {
		return err
	}
	return p.writer.Write(memql.ContextWithRowVersionFence(ctx, requestConcept, id, at), "putReviewRequest", row)
}

// Erasure records its terminal decision before rewriting private data. If that
// rewrite fails, neither a retry nor manual issuance may reopen the request in
// the interval before the operator finishes the erasure.
func (p *Provider) requestWasErased(ctx context.Context, id string) (bool, error) {
	reviewID := canonicalReviewID(reviewIDForRequest(bareID(id, requestConcept+":")))
	rows, err := p.rowsFor(memql.ContextWithFreshRead(ctx), "reviewById", map[string]string{"reviewId": reviewID})
	if err != nil || len(rows) == 0 {
		return false, err
	}
	decisions, err := p.decisionsForReview(ctx, reviewID)
	if err != nil {
		return false, err
	}
	return StateOf(VersionOf(rows[0]), decisions) == StateErased, nil
}
