package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/devicecode"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// End-to-end coverage of the RFC 8628 device grant (memql#3410),
// driven through the REAL handlers and the REAL devicecode.Store
// against a fake engine. Going through the store rather than a stubbed
// interface is deliberate: the store is where the mutation strings are
// built, and a stub would let a typo'd argument name pass every test
// here and fail the first time it met a database.
//
// What the state-machine tests pin, in the issue's words: pending ->
// approved -> redeemed, denied, expired, slow_down on fast polling,
// single redemption only, and PKCE surviving the whole round trip.

// deviceFakeEngine serves the device-grant queries and mutations from
// one in-memory row, plus the session/user writes the success path
// makes on its way out.
type deviceFakeEngine struct {
	mu sync.Mutex

	row *deviceFakeRow

	// createdWith records the raw createDeviceCode call so a test can
	// assert what did (and did not) reach the engine.
	createdWith string
	polls       int
	redeems     int
	sessions    int
}

type deviceFakeRow struct {
	id                  string
	clientId            string
	deviceCodeHash      string
	userCodeHash        string
	status              string
	scope               string
	codeChallenge       string
	codeChallengeMethod string
	expiresAt           time.Time
	intervalSeconds     int
	lastPolledAt        time.Time
	approvedByUserId    string
	sourceIP            string
	userAgent           string
	createdAt           time.Time
}

const deviceTestUserId = "v1:identity:user:approver-1"

func (f *deviceFakeEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case strings.Contains(q, "createDeviceCode("):
		f.createdWith = q
		f.row = &deviceFakeRow{
			id:                  "v1:identity:deviceCode:" + deviceArg(q, "deviceCodeId"),
			clientId:            deviceArg(q, "clientId"),
			deviceCodeHash:      deviceArg(q, "deviceCodeHash"),
			userCodeHash:        deviceArg(q, "userCodeHash"),
			status:              devicecode.StatusPending,
			scope:               deviceArg(q, "scope"),
			codeChallenge:       deviceArg(q, "codeChallenge"),
			codeChallengeMethod: deviceArg(q, "codeChallengeMethod"),
			expiresAt:           deviceTimeArg(q, "expiresAt"),
			intervalSeconds:     deviceIntArg(q, "intervalSeconds"),
			sourceIP:            deviceArg(q, "sourceIP"),
			userAgent:           deviceArg(q, "userAgent"),
			createdAt:           time.Now().UTC(),
		}
		return emptyResult(), nil

	case strings.Contains(q, "deviceCodeByDeviceCodeHash("):
		return f.matched(deviceArg(q, "deviceCodeHash"), func(r *deviceFakeRow) string { return r.deviceCodeHash }), nil

	case strings.Contains(q, "deviceCodeByUserCodeHash("):
		return f.matched(deviceArg(q, "userCodeHash"), func(r *deviceFakeRow) string { return r.userCodeHash }), nil

	case strings.Contains(q, "touchDeviceCodePoll("):
		f.polls++
		if f.row != nil {
			f.row.lastPolledAt = deviceTimeArg(q, "lastPolledAt")
			f.row.intervalSeconds = deviceIntArg(q, "intervalSeconds")
		}
		return emptyResult(), nil

	case strings.Contains(q, "approveDeviceCode("):
		if f.row != nil {
			f.row.status = devicecode.StatusApproved
			f.row.approvedByUserId = deviceTestUserId
		}
		return emptyResult(), nil

	case strings.Contains(q, "denyDeviceCode("):
		if f.row != nil {
			f.row.status = devicecode.StatusDenied
			f.row.approvedByUserId = deviceTestUserId
		}
		return emptyResult(), nil

	case strings.Contains(q, "redeemDeviceCode("):
		f.redeems++
		if f.row != nil {
			f.row.status = devicecode.StatusRedeemed
		}
		return emptyResult(), nil

	case strings.Contains(q, "createAuthSession("):
		f.sessions++
		return emptyResult(), nil

	case strings.Contains(q, "userByIdSystem(") || strings.Contains(q, "userById("):
		node := &memqlv1.MemoryNode{
			Id: deviceTestUserId,
			Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
				"id":           structpb.NewStringValue(deviceTestUserId),
				"primaryEmail": structpb.NewStringValue("approver@example.com"),
				"role":         structpb.NewStringValue("owner"),
			}},
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{node}}}, nil
	}
	return emptyResult(), nil
}

func (f *deviceFakeEngine) matched(presented string, key func(*deviceFakeRow) string) *memqlengine.ExecuteResult {
	if f.row == nil || presented == "" || key(f.row) != presented {
		return emptyResult()
	}
	r := f.row
	fields := map[string]*structpb.Value{
		"id":                  structpb.NewStringValue(r.id),
		"clientId":            structpb.NewStringValue(r.clientId),
		"deviceCodeHash":      structpb.NewStringValue(r.deviceCodeHash),
		"userCodeHash":        structpb.NewStringValue(r.userCodeHash),
		"status":              structpb.NewStringValue(r.status),
		"scope":               structpb.NewStringValue(r.scope),
		"codeChallenge":       structpb.NewStringValue(r.codeChallenge),
		"codeChallengeMethod": structpb.NewStringValue(r.codeChallengeMethod),
		"expiresAt":           structpb.NewStringValue(stampOrEmpty(r.expiresAt)),
		"intervalSeconds":     structpb.NewNumberValue(float64(r.intervalSeconds)),
		"lastPolledAt":        structpb.NewStringValue(stampOrEmpty(r.lastPolledAt)),
		"approvedByUserId":    structpb.NewStringValue(r.approvedByUserId),
		"sourceIP":            structpb.NewStringValue(r.sourceIP),
		"userAgent":           structpb.NewStringValue(r.userAgent),
		"createdAt":           structpb.NewStringValue(stampOrEmpty(r.createdAt)),
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{
		{Id: r.id, Payload: &structpb.Struct{Fields: fields}},
	}}}
}

// rewindPoll moves the poll clock back far enough that the next poll is
// inside its budget. Tests that are exercising the STATE machine should
// not also be waiting on the poll clock.
func (f *deviceFakeEngine) rewindPoll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.row != nil {
		f.row.lastPolledAt = time.Time{}
	}
}

func (f *deviceFakeEngine) expire() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.row != nil {
		f.row.expiresAt = time.Now().UTC().Add(-time.Minute)
		f.row.lastPolledAt = time.Time{}
	}
}

func (f *deviceFakeEngine) snapshot() deviceFakeRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.row == nil {
		return deviceFakeRow{}
	}
	return *f.row
}

func emptyResult() *memqlengine.ExecuteResult {
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}
}

func stampOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// deviceArg pulls a `key:"value"` string argument out of a serialized
// engine call. The device store writes its args with %q and no spaces,
// so this is a deliberately narrow reader rather than a parser.
func deviceArg(q, key string) string {
	i := strings.Index(q, key+`:"`)
	if i < 0 {
		return ""
	}
	rest := q[i+len(key)+2:]
	var b strings.Builder
	for j := 0; j < len(rest); j++ {
		if rest[j] == '\\' && j+1 < len(rest) {
			b.WriteByte(rest[j+1])
			j++
			continue
		}
		if rest[j] == '"' {
			break
		}
		b.WriteByte(rest[j])
	}
	return b.String()
}

func deviceIntArg(q, key string) int {
	i := strings.Index(q, key+":")
	if i < 0 {
		return 0
	}
	rest := q[i+len(key)+1:]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	n, _ := strconv.Atoi(rest[:end])
	return n
}

func deviceTimeArg(q, key string) time.Time {
	raw := deviceArg(q, key)
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

const deviceTestClientId = "test-device-client"

func newDeviceTestServer(t *testing.T) (*Server, *deviceFakeEngine) {
	t.Helper()
	eng := &deviceFakeEngine{}
	store := &identity.Store{Engine: eng}

	dir := t.TempDir()
	km, err := identity.NewKeyManager(dir, "")
	if err != nil {
		t.Fatalf("new key manager: %v", err)
	}
	if err := km.Load(); err != nil {
		t.Fatalf("load keys: %v", err)
	}
	cfg := identity.Config{
		Enabled:     true,
		BaseURL:     "https://identity.test",
		JWTAudience: "memql",
		KeyDir:      dir,
		RegisteredClients: []identity.RegisteredClient{
			{ClientId: deviceTestClientId, Name: "Test Device Client"},
		},
	}
	iss, err := identity.NewJWTIssuer(km, cfg)
	if err != nil {
		t.Fatalf("new issuer: %v", err)
	}
	return &Server{
		Cfg:         cfg,
		Store:       store,
		Issuer:      iss,
		DeviceCodes: &devicecode.Store{Engine: eng},
	}, eng
}

// requestDeviceCode drives POST /device/code and returns the parsed
// success body. form is appended to the mandatory client_id.
func requestDeviceCode(t *testing.T, s *Server, extra string) DeviceAuthorizationResponse {
	t.Helper()
	body := "client_id=" + deviceTestClientId
	if extra != "" {
		body += "&" + extra
	}
	req := httptest.NewRequest(http.MethodPost, "/device/code", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// requireSecureRequest: the response carries two plaintext
	// credentials, so the endpoint refuses a cleartext hop.
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	s.handleDeviceCode(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /device/code status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out DeviceAuthorizationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode device authorization response: %v; body=%s", err, rec.Body.String())
	}
	return out
}

// pollToken drives one device-grant poll against /oauth/token.
func pollToken(t *testing.T, s *Server, deviceCode, extra string) *httptest.ResponseRecorder {
	t.Helper()
	form := "grant_type=" + DeviceGrantType +
		"&client_id=" + deviceTestClientId +
		"&device_code=" + deviceCode
	if extra != "" {
		form += "&" + extra
	}
	return postTokenForm(t, s, form)
}

func deviceErrorBody(t *testing.T, rec *httptest.ResponseRecorder) deviceGrantErrorResponse {
	t.Helper()
	var out deviceGrantErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode device grant error: %v; body=%s", err, rec.Body.String())
	}
	return out
}

func TestDeviceAuthorizationRequest_ReturnsTheRFCContract(t *testing.T) {
	s, eng := newDeviceTestServer(t)
	resp := requestDeviceCode(t, s, "scope=openid")

	if !strings.HasPrefix(resp.DeviceCode, devicecode.DeviceCodePrefix) {
		t.Fatalf("device_code = %q, want the %q prefix", resp.DeviceCode, devicecode.DeviceCodePrefix)
	}
	if devicecode.CanonicalizeUserCode(resp.UserCode) == "" {
		t.Fatalf("user_code = %q, which does not canonicalize", resp.UserCode)
	}
	if !strings.Contains(resp.UserCode, devicecode.UserCodeSeparator) {
		t.Fatalf("user_code = %q, want two separated groups", resp.UserCode)
	}
	if resp.VerificationURI != "https://identity.test/device" {
		t.Fatalf("verification_uri = %q", resp.VerificationURI)
	}
	if !strings.HasPrefix(resp.VerificationURIComplete, resp.VerificationURI+"?user_code=") {
		t.Fatalf("verification_uri_complete = %q, want it to prefill the code", resp.VerificationURIComplete)
	}
	if resp.ExpiresIn != int(devicecode.DefaultTTL/time.Second) {
		t.Fatalf("expires_in = %d, want %d", resp.ExpiresIn, int(devicecode.DefaultTTL/time.Second))
	}
	if resp.Interval != devicecode.DefaultIntervalSeconds {
		t.Fatalf("interval = %d, want %d", resp.Interval, devicecode.DefaultIntervalSeconds)
	}

	// Neither plaintext may reach the engine. This is the whole
	// hash-at-rest claim, checked against the actual mutation string.
	if strings.Contains(eng.createdWith, resp.DeviceCode) {
		t.Fatal("the plaintext device_code was persisted")
	}
	canonicalUserCode := devicecode.CanonicalizeUserCode(resp.UserCode)
	if strings.Contains(eng.createdWith, canonicalUserCode) || strings.Contains(eng.createdWith, resp.UserCode) {
		t.Fatal("the plaintext user_code was persisted")
	}
	row := eng.snapshot()
	if row.deviceCodeHash != devicecode.HashDeviceCode(resp.DeviceCode) {
		t.Fatal("the stored deviceCodeHash is not the digest of the returned device_code")
	}
	if row.userCodeHash != devicecode.HashUserCode(resp.UserCode) {
		t.Fatal("the stored userCodeHash is not the digest of the returned user_code")
	}
	if row.status != devicecode.StatusPending {
		t.Fatalf("row is born in status %q, want pending", row.status)
	}
}

func TestDeviceAuthorizationRequest_RejectsUnregisteredClient(t *testing.T) {
	s, _ := newDeviceTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/device/code", strings.NewReader("client_id=nobody"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	s.handleDeviceCode(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec.Body.Bytes()); got != "invalid_client" {
		t.Fatalf("error = %q, want invalid_client", got)
	}
}

func TestDeviceAuthorizationRequest_RequiresHTTPS(t *testing.T) {
	s, _ := newDeviceTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/device/code",
		strings.NewReader("client_id="+deviceTestClientId))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleDeviceCode(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 on a cleartext hop; body=%s", rec.Code, rec.Body.String())
	}
}

// The full happy path: pending -> approved -> redeemed, and the single
// redemption that follows it.
func TestDeviceGrant_PendingApprovedRedeemed(t *testing.T) {
	s, eng := newDeviceTestServer(t)
	resp := requestDeviceCode(t, s, "")

	// pending
	rec := pollToken(t, s, resp.DeviceCode, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("pending poll status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := deviceErrorBody(t, rec).Error; got != "authorization_pending" {
		t.Fatalf("pending poll error = %q, want authorization_pending", got)
	}

	// the human approves
	approveViaStore(t, s, resp.UserCode)
	eng.rewindPoll()

	// approved -> tokens
	rec = pollToken(t, s, resp.DeviceCode, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("approved poll status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var tok tokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &tok); err != nil {
		t.Fatalf("decode token response: %v; body=%s", err, rec.Body.String())
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" {
		t.Fatalf("expected an access + refresh pair, got %+v", tok)
	}
	if tok.TokenType != "Bearer" {
		t.Fatalf("token_type = %q, want Bearer", tok.TokenType)
	}
	// The session must belong to the human who approved, not to the
	// device.
	claims, err := s.Issuer.VerifyAccessToken(tok.AccessToken, time.Now().UTC())
	if err != nil {
		t.Fatalf("the minted access token does not verify: %v", err)
	}
	if claims.Subject != deviceTestUserId {
		t.Fatalf("access token subject = %q, want the approving user %q", claims.Subject, deviceTestUserId)
	}
	if eng.sessions != 1 {
		t.Fatalf("expected exactly one session row, got %d", eng.sessions)
	}

	// single redemption only
	eng.rewindPoll()
	rec = pollToken(t, s, resp.DeviceCode, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("replay status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := deviceErrorBody(t, rec).Error; got != "invalid_grant" {
		t.Fatalf("replay error = %q, want invalid_grant", got)
	}
	if eng.sessions != 1 {
		t.Fatalf("a replayed device_code minted a second session (%d total)", eng.sessions)
	}
	if eng.redeems != 1 {
		t.Fatalf("expected exactly one redeem write, got %d", eng.redeems)
	}
}

func TestDeviceGrant_Denied(t *testing.T) {
	s, eng := newDeviceTestServer(t)
	resp := requestDeviceCode(t, s, "")

	denyViaStore(t, s, resp.UserCode)
	eng.rewindPoll()

	rec := pollToken(t, s, resp.DeviceCode, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := deviceErrorBody(t, rec).Error; got != "access_denied" {
		t.Fatalf("error = %q, want access_denied", got)
	}
	if eng.sessions != 0 {
		t.Fatal("a denied authorization minted a session")
	}
}

func TestDeviceGrant_Expired(t *testing.T) {
	s, eng := newDeviceTestServer(t)
	resp := requestDeviceCode(t, s, "")

	// Expiry beats approval: even an approved row past its window is
	// expired_token, not a free session.
	approveViaStore(t, s, resp.UserCode)
	eng.expire()

	rec := pollToken(t, s, resp.DeviceCode, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := deviceErrorBody(t, rec).Error; got != "expired_token" {
		t.Fatalf("error = %q, want expired_token", got)
	}
	if eng.sessions != 0 {
		t.Fatal("an expired authorization minted a session")
	}
}

func TestDeviceGrant_SlowDownRaisesTheInterval(t *testing.T) {
	s, eng := newDeviceTestServer(t)
	resp := requestDeviceCode(t, s, "")

	// First poll: allowed (no prior clock), and it stamps one.
	if rec := pollToken(t, s, resp.DeviceCode, ""); deviceErrorBody(t, rec).Error != "authorization_pending" {
		t.Fatalf("first poll should be accepted-but-pending; body=%s", rec.Body.String())
	}
	before := eng.snapshot().intervalSeconds
	if before != devicecode.DefaultIntervalSeconds {
		t.Fatalf("interval after the first poll = %d, want %d", before, devicecode.DefaultIntervalSeconds)
	}

	// Second poll, immediately: too fast.
	rec := pollToken(t, s, resp.DeviceCode, "")
	body := deviceErrorBody(t, rec)
	if rec.Code != http.StatusBadRequest || body.Error != "slow_down" {
		t.Fatalf("fast poll = %d/%q, want 400/slow_down; body=%s", rec.Code, body.Error, rec.Body.String())
	}
	wantInterval := before + devicecode.SlowDownIncrementSeconds
	if body.Interval != wantInterval {
		t.Fatalf("slow_down advertised interval = %d, want %d", body.Interval, wantInterval)
	}
	if got := eng.snapshot().intervalSeconds; got != wantInterval {
		t.Fatalf("persisted interval = %d, want it raised to %d", got, wantInterval)
	}

	// And again: the escalation compounds rather than resetting.
	rec = pollToken(t, s, resp.DeviceCode, "")
	body = deviceErrorBody(t, rec)
	if body.Error != "slow_down" {
		t.Fatalf("third poll error = %q, want slow_down", body.Error)
	}
	if body.Interval != wantInterval+devicecode.SlowDownIncrementSeconds {
		t.Fatalf("interval = %d, want %d", body.Interval, wantInterval+devicecode.SlowDownIncrementSeconds)
	}
}

func TestDeviceGrant_UnknownAndMismatchedCodes(t *testing.T) {
	s, _ := newDeviceTestServer(t)
	resp := requestDeviceCode(t, s, "")

	t.Run("unknown device_code", func(t *testing.T) {
		rec := pollToken(t, s, devicecode.DeviceCodePrefix+"not-a-real-code", "")
		if got := deviceErrorBody(t, rec).Error; got != "invalid_grant" {
			t.Fatalf("error = %q, want invalid_grant", got)
		}
	})

	t.Run("missing device_code", func(t *testing.T) {
		rec := postTokenForm(t, s, "grant_type="+DeviceGrantType+"&client_id="+deviceTestClientId)
		if got := deviceErrorBody(t, rec).Error; got != "invalid_request" {
			t.Fatalf("error = %q, want invalid_request", got)
		}
	})

	t.Run("another client's device_code", func(t *testing.T) {
		rec := postTokenForm(t, s, "grant_type="+DeviceGrantType+
			"&client_id=some-other-client&device_code="+resp.DeviceCode)
		if got := deviceErrorBody(t, rec).Error; got != "invalid_grant" {
			t.Fatalf("error = %q, want invalid_grant", got)
		}
	})
}

// PKCE has to survive the human round trip: the challenge is bound at
// POST /device/code, the row sits through an approval on a different
// device, and the verifier is checked at redemption.
func TestDeviceGrant_PKCEBindingSurvivesTheFlow(t *testing.T) {
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	challenge := s256Challenge(verifier)

	t.Run("correct verifier redeems", func(t *testing.T) {
		s, eng := newDeviceTestServer(t)
		resp := requestDeviceCode(t, s, "code_challenge="+challenge+"&code_challenge_method=S256")
		if got := eng.snapshot().codeChallenge; got != challenge {
			t.Fatalf("stored codeChallenge = %q, want %q", got, challenge)
		}
		approveViaStore(t, s, resp.UserCode)
		eng.rewindPoll()

		rec := pollToken(t, s, resp.DeviceCode, "code_verifier="+verifier)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("missing verifier is refused", func(t *testing.T) {
		s, eng := newDeviceTestServer(t)
		resp := requestDeviceCode(t, s, "code_challenge="+challenge+"&code_challenge_method=S256")
		approveViaStore(t, s, resp.UserCode)
		eng.rewindPoll()

		rec := pollToken(t, s, resp.DeviceCode, "")
		if got := deviceErrorBody(t, rec).Error; got != "invalid_grant" {
			t.Fatalf("error = %q, want invalid_grant", got)
		}
		if eng.sessions != 0 {
			t.Fatal("a PKCE-bound authorization redeemed without a verifier")
		}
		// The row must NOT be spent by a failed PKCE check -- the
		// legitimate device still holds a live authorization.
		if eng.redeems != 0 {
			t.Fatal("a failed PKCE check consumed the authorization")
		}
	})

	t.Run("wrong verifier is refused", func(t *testing.T) {
		s, eng := newDeviceTestServer(t)
		resp := requestDeviceCode(t, s, "code_challenge="+challenge+"&code_challenge_method=S256")
		approveViaStore(t, s, resp.UserCode)
		eng.rewindPoll()

		rec := pollToken(t, s, resp.DeviceCode, "code_verifier=not-the-verifier-at-all-000000000000")
		if got := deviceErrorBody(t, rec).Error; got != "invalid_grant" {
			t.Fatalf("error = %q, want invalid_grant", got)
		}
		if eng.sessions != 0 {
			t.Fatal("a wrong verifier still minted a session")
		}
	})

	t.Run("an unsupported method is refused at request time", func(t *testing.T) {
		s, _ := newDeviceTestServer(t)
		req := httptest.NewRequest(http.MethodPost, "/device/code", strings.NewReader(
			"client_id="+deviceTestClientId+"&code_challenge="+challenge+"&code_challenge_method=S512"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Forwarded-Proto", "https")
		rec := httptest.NewRecorder()
		s.handleDeviceCode(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		if got := errorCode(t, rec.Body.Bytes()); got != "invalid_request" {
			t.Fatalf("error = %q, want invalid_request", got)
		}
	})
}

// The token endpoint's grant switch must name the new grant when it
// refuses an unknown one -- the message is the only discovery surface a
// developer reading a 400 gets.
func TestUnsupportedGrantMessageNamesTheDeviceGrant(t *testing.T) {
	s, _ := newDeviceTestServer(t)
	rec := postTokenForm(t, s, "grant_type=password")
	if got := errorCode(t, rec.Body.Bytes()); got != "unsupported_grant_type" {
		t.Fatalf("error = %q, want unsupported_grant_type", got)
	}
	if !strings.Contains(rec.Body.String(), DeviceGrantType) {
		t.Fatalf("the unsupported_grant_type message does not mention %q: %s", DeviceGrantType, rec.Body.String())
	}
}

// approveViaStore / denyViaStore stand in for the verification page:
// they resolve the row by the USER code (the lookup the page makes) and
// drive the same store transition the page's handler drives.
func approveViaStore(t *testing.T, s *Server, userCode string) {
	t.Helper()
	row := lookupByUserCode(t, s, userCode)
	if err := s.DeviceCodes.Approve(context.Background(), row.ID); err != nil {
		t.Fatalf("approve: %v", err)
	}
}

func denyViaStore(t *testing.T, s *Server, userCode string) {
	t.Helper()
	row := lookupByUserCode(t, s, userCode)
	if err := s.DeviceCodes.Deny(context.Background(), row.ID); err != nil {
		t.Fatalf("deny: %v", err)
	}
}

func lookupByUserCode(t *testing.T, s *Server, userCode string) *devicecode.Row {
	t.Helper()
	// Deliberately look up through a SLOPPILY typed form of the code --
	// lowercase and without the separator -- because that is what a
	// human at a keyboard produces and the page has to find the row
	// anyway.
	typed := strings.ToLower(strings.ReplaceAll(userCode, devicecode.UserCodeSeparator, ""))
	row, err := s.DeviceCodes.LookupByUserCodeHash(context.Background(), devicecode.HashUserCode(typed))
	if err != nil {
		t.Fatalf("lookup by user code: %v", err)
	}
	if row == nil {
		t.Fatalf("no device authorization found for user_code %q (typed as %q)", userCode, typed)
	}
	return row
}

// POST /device/code is per-IP rate limited: each accepted request
// writes a row and burns a user_code out of a finite space, so the
// limiter is what stops an attacker carpeting that space with pending
// codes hoping a human approves one.
func TestDeviceAuthorizationRequestIsRateLimitedPerIP(t *testing.T) {
	t.Setenv(envDeviceCodePerHour, "2")
	s, _ := newDeviceTestServer(t)

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/device/code",
			strings.NewReader("client_id="+deviceTestClientId))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Forwarded-Proto", "https")
		rec := httptest.NewRecorder()
		s.handleDeviceCode(rec, req)
		return rec
	}
	for i := 0; i < 2; i++ {
		if rec := post(); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 while inside the budget; body=%s",
				i, rec.Code, rec.Body.String())
		}
	}
	rec := post()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 once the budget is spent; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a 429 must carry Retry-After so a client knows when to come back")
	}
	// RFC 8628 clients already understand slow_down as "back off", so
	// that is the code a throttled device authorization returns.
	if got := errorCode(t, rec.Body.Bytes()); got != "slow_down" {
		t.Fatalf("error = %q, want slow_down", got)
	}
}
