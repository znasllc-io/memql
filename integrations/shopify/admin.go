package shopify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	stdsync "sync"
	"time"
)

// admin.go -- the cost-aware Admin GraphQL client.
//
// Shopify's Admin API is not rate-limited by requests, it is limited by a
// LEAKY BUCKET of query cost points: a bucket size, a restore rate per
// second, and a cost charged per query. Every response says where the bucket
// stands, in extensions.cost.throttleStatus. A client that ignores it is a
// client that discovers the limit by being refused -- and a backfill that
// discovers it that way turns a five-minute job into an hour of retries.
//
// So this client reads the bucket on every response and WAITS before the
// call that would overdraw it, rather than after. The 429 handling below is
// still there because pacing cannot be perfect (other apps and the merchant's
// own admin share the bucket), but on a healthy store it should never fire.

// AdminEndpoint builds the Admin GraphQL URL for a store and version.
func AdminEndpoint(domain, apiVersion string) string {
	return fmt.Sprintf("https://%s/admin/api/%s/graphql.json", strings.TrimSuffix(domain, "/"), apiVersion)
}

// ThrottleStatus is Shopify's own view of the cost bucket.
type ThrottleStatus struct {
	MaximumAvailable   float64 `json:"maximumAvailable"`
	CurrentlyAvailable float64 `json:"currentlyAvailable"`
	RestoreRate        float64 `json:"restoreRate"`
}

type costExtension struct {
	RequestedQueryCost float64        `json:"requestedQueryCost"`
	ActualQueryCost    float64        `json:"actualQueryCost"`
	ThrottleStatus     ThrottleStatus `json:"throttleStatus"`
}

// AdminResponse is a decoded Admin GraphQL reply.
type AdminResponse struct {
	Data       json.RawMessage
	Errors     []GraphQLError
	Throttle   ThrottleStatus
	QueryCost  float64
	StatusCode int
}

// GraphQLError is one entry of the errors array.
type GraphQLError struct {
	Message    string         `json:"message"`
	Extensions map[string]any `json:"extensions,omitempty"`
	Path       []any          `json:"path,omitempty"`
}

// Code reads the vendor error code, which is what distinguishes a throttle
// (retry) from a validation failure (do not).
func (e GraphQLError) Code() string {
	if e.Extensions == nil {
		return ""
	}
	c, _ := e.Extensions["code"].(string)
	return c
}

// Error makes an error slice printable without exposing the query.
type adminErrors []GraphQLError

func (e adminErrors) Error() string {
	parts := make([]string, 0, len(e))
	for _, err := range e {
		if code := err.Code(); code != "" {
			parts = append(parts, code+": "+err.Message)
			continue
		}
		parts = append(parts, err.Message)
	}
	return "shopify admin: " + strings.Join(parts, "; ")
}

// Throttled reports whether the failure is Shopify asking us to slow down.
func (e adminErrors) Throttled() bool {
	for _, err := range e {
		if err.Code() == "THROTTLED" {
			return true
		}
	}
	return false
}

// AdminError is a typed failure from the Admin API.
type AdminError struct {
	StatusCode int
	Errors     []GraphQLError
	RetryAfter time.Duration
	Body       string
}

func (e *AdminError) Error() string {
	if len(e.Errors) > 0 {
		return adminErrors(e.Errors).Error()
	}
	return fmt.Sprintf("shopify admin: HTTP %d", e.StatusCode)
}

// Retryable reports whether trying the same call again could succeed.
//
// The distinction is the whole reason this type exists. A throttle, a 429 and
// a 5xx will succeed later; a validation error, a missing scope and a 404
// will fail identically forever, and retrying them is how a queue stops
// draining and a backfill never finishes.
func (e *AdminError) Retryable() bool {
	if e == nil {
		return false
	}
	if e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500 {
		return true
	}
	return adminErrors(e.Errors).Throttled()
}

// AdminClient calls one deployment's stores. It is safe for concurrent use;
// the pacing state is per store.
type AdminClient struct {
	http     *http.Client
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
	endpoint func(store Store) string
	// MaxRetries bounds throttle retries for a single call.
	MaxRetries int
	// Reserve is the number of cost points to keep in the bucket. Pacing
	// waits until at least this much is available, so a burst of cheap
	// calls cannot leave the bucket empty for the expensive one behind
	// them.
	Reserve float64

	mu     stdsync.Mutex
	bucket map[string]ThrottleStatus
	seenAt map[string]time.Time
}

// NewAdminClient builds a client with production defaults.
func NewAdminClient() *AdminClient {
	return &AdminClient{
		http:       &http.Client{Timeout: 60 * time.Second},
		now:        time.Now,
		sleep:      sleepCtx,
		MaxRetries: 4,
		Reserve:    100,
		bucket:     map[string]ThrottleStatus{},
		seenAt:     map[string]time.Time{},
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Do runs one operation against a store and returns the decoded data.
//
// `token` is passed per call rather than held: see the note on Store.
func (c *AdminClient) Do(ctx context.Context, store Store, token, document, operation string, variables map[string]any) (*AdminResponse, error) {
	if c == nil {
		return nil, fmt.Errorf("shopify: admin client is nil")
	}
	var last error
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if err := c.pace(ctx, store.ID); err != nil {
			return nil, err
		}
		resp, err := c.once(ctx, store, token, document, operation, variables)
		if err == nil {
			return resp, nil
		}
		last = err
		ae, ok := err.(*AdminError)
		if !ok || !ae.Retryable() {
			return nil, err
		}
		wait := ae.RetryAfter
		if wait <= 0 {
			wait = backoffFor(attempt)
		}
		if sleepErr := c.sleep(ctx, wait); sleepErr != nil {
			return nil, sleepErr
		}
	}
	return nil, fmt.Errorf("shopify: gave up after %d attempts: %w", c.MaxRetries+1, last)
}

// backoffFor is the wait between throttle retries when Shopify does not say.
// Doubling from a second, capped -- long enough to let the bucket restore,
// short enough that a backfill still finishes.
func backoffFor(attempt int) time.Duration {
	d := time.Second << attempt
	if d > 30*time.Second {
		return 30 * time.Second
	}
	return d
}

func (c *AdminClient) once(ctx context.Context, store Store, token, document, operation string, variables map[string]any) (*AdminResponse, error) {
	body := map[string]any{"query": document}
	if operation != "" {
		body["operationName"] = operation
	}
	if len(variables) > 0 {
		body["variables"] = variables
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("shopify: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpointFor(store), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Shopify-Access-Token", token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("shopify: admin request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("shopify: read admin response: %w", err)
	}

	var envelope struct {
		Data       json.RawMessage `json:"data"`
		Errors     []GraphQLError  `json:"errors"`
		Extensions struct {
			Cost costExtension `json:"cost"`
		} `json:"extensions"`
	}
	// A non-JSON body is still worth reporting: a 502 from the edge in
	// front of Shopify carries HTML, and an operator needs the status more
	// than the parse error.
	_ = json.Unmarshal(raw, &envelope)

	c.record(store.ID, envelope.Extensions.Cost.ThrottleStatus)

	if resp.StatusCode != http.StatusOK {
		return nil, &AdminError{
			StatusCode: resp.StatusCode,
			Errors:     envelope.Errors,
			RetryAfter: retryAfter(resp.Header.Get("Retry-After")),
			Body:       truncate(string(raw), 512),
		}
	}
	if len(envelope.Errors) > 0 {
		return nil, &AdminError{StatusCode: resp.StatusCode, Errors: envelope.Errors}
	}
	return &AdminResponse{
		Data:       envelope.Data,
		Throttle:   envelope.Extensions.Cost.ThrottleStatus,
		QueryCost:  envelope.Extensions.Cost.ActualQueryCost,
		StatusCode: resp.StatusCode,
	}, nil
}

func (c *AdminClient) endpointFor(store Store) string {
	if c.endpoint != nil {
		return c.endpoint(store)
	}
	return AdminEndpoint(store.Domain, store.APIVersion)
}

// record stores the bucket state a response reported.
func (c *AdminClient) record(storeID string, status ThrottleStatus) {
	if status.MaximumAvailable == 0 && status.RestoreRate == 0 {
		return // a response that carried no cost extension says nothing
	}
	c.mu.Lock()
	c.bucket[storeID] = status
	c.seenAt[storeID] = c.now()
	c.mu.Unlock()
}

// pace waits until the store's bucket has restored past the reserve.
//
// The wait is computed from the LAST OBSERVED bucket plus the restore rate
// times the elapsed time, because Shopify only reports the bucket in a
// response and the whole point is to wait BEFORE the next request.
func (c *AdminClient) pace(ctx context.Context, storeID string) error {
	c.mu.Lock()
	status, ok := c.bucket[storeID]
	seen := c.seenAt[storeID]
	c.mu.Unlock()
	if !ok || status.RestoreRate <= 0 {
		return nil
	}
	available := status.CurrentlyAvailable + status.RestoreRate*c.now().Sub(seen).Seconds()
	if available > status.MaximumAvailable {
		available = status.MaximumAvailable
	}
	reserve := c.Reserve
	if reserve > status.MaximumAvailable {
		reserve = status.MaximumAvailable / 2
	}
	if available >= reserve {
		return nil
	}
	wait := time.Duration((reserve - available) / status.RestoreRate * float64(time.Second))
	if wait > 30*time.Second {
		wait = 30 * time.Second
	}
	return c.sleep(ctx, wait)
}

// Bucket exposes the last observed cost bucket for a store, for the health
// the portal renders.
func (c *AdminClient) Bucket(storeID string) (ThrottleStatus, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.bucket[storeID]
	return s, ok
}

func retryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(header, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	if when, err := http.ParseTime(header); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// DecodeInto unmarshals the data half of a response.
func (r *AdminResponse) DecodeInto(v any) error {
	if r == nil || len(r.Data) == 0 {
		return fmt.Errorf("shopify: response carried no data")
	}
	return json.Unmarshal(r.Data, v)
}

// DataMap decodes the data half into a generic map, which is what the mirror
// mapper walks -- the mirror is generated, so there is no generated Go struct
// to decode into and there should not be one: two generated shapes for the
// same fields is two things to keep in step.
func (r *AdminResponse) DataMap() (map[string]any, error) {
	if r == nil || len(r.Data) == 0 {
		return nil, fmt.Errorf("shopify: response carried no data")
	}
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(r.Data))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("shopify: decode data: %w", err)
	}
	return out, nil
}
