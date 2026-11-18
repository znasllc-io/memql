package inbound

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// verify_jwt_test.go -- the jwt-hs256-body-sha256 scheme: a sender that signs a JWT
// over its claims, one of which is the SHA-256 of the body, and sends the token as a
// header (a LiveKit media server's webhooks).
//
// The tokens here are minted by hand with the standard library, so each test can
// build exactly the malformed token it is about. That a token LiveKit's own SDK mints
// verifies is held elsewhere, where the SDK is in the module graph:
// app/transport_inbound_livekit_test.go TestALiveKitSignedRequestVerifies.

const (
	jwtTestSecret = "jwt-secret-0123456789abcdefghijklmnop"
	jwtTestIssuer = "APIjwtissuer"
	jwtTolerance  = 5 * time.Minute
)

// jwtNow is the receiver's clock in these tests, testHandler's too.
var jwtNow = time.Unix(1_700_000_000, 0).UTC()

var hs256Header = map[string]any{"alg": "HS256", "typ": "JWT"}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// mintJWT signs header and claims with HMAC-SHA256 under secret, WHATEVER the header
// says its algorithm is: what a forger would send.
func mintJWT(t *testing.T, header, claims map[string]any, secret string) string {
	t.Helper()
	h, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	c, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signing := b64url(h) + "." + b64url(c)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signing))
	return signing + "." + b64url(mac.Sum(nil))
}

// bodyClaims is what a media server signs for one webhook body: its key as the issuer,
// five minutes of validity from issued, and the body's digest in standard base64.
func bodyClaims(body []byte, issued time.Time) map[string]any {
	sum := sha256.Sum256(body)
	return map[string]any{
		"iss":    jwtTestIssuer,
		"nbf":    issued.Unix(),
		"iat":    issued.Unix(),
		"exp":    issued.Add(5 * time.Minute).Unix(),
		"sha256": base64.StdEncoding.EncodeToString(sum[:]),
	}
}

func checkJWT(token string, body []byte) error {
	return verifyJWTBodySHA256(token, body, []byte(jwtTestSecret), jwtTestIssuer, jwtNow, jwtTolerance)
}

func requireUnverified(t *testing.T, err error, why string) {
	t.Helper()
	if err == nil {
		t.Fatalf("a token was accepted: %s", why)
	}
	if !errors.Is(err, errUnverified) {
		t.Fatalf("a refusal that is not errUnverified (%v): every refusal of an unauthenticated caller answers the same flat 401", err)
	}
}

func jwtSource() SourceConfig {
	return SourceConfig{Scheme: SchemeJWTHS256BodySHA256, Secret: jwtTestSecret, SignatureHeader: "Authorization", JWTIssuer: jwtTestIssuer}
}

func jwtRequest(body []byte, token string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/inbound/acme", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/webhook+json")
	r.Header.Set("Authorization", token)
	return r
}

// TestJWTSchemeAccepts: a token signed with the source's secret, naming its issuer,
// inside its validity and carrying the body's digest is accepted, by the verifier and
// by the receiver, which stages the delivery as verified. The row's identity is the
// body's digest and never the token: the same body under a token minted a second
// later (a sender's second attempt at one event) lands on the same row. Neither the
// token nor the header it rode in reaches the staged statement.
func TestJWTSchemeAccepts(t *testing.T) {
	body := []byte(`{"event":"participant_joined","createdAt":"1700000000"}`)
	if err := checkJWT(mintJWT(t, hs256Header, bodyClaims(body, jwtNow), jwtTestSecret), body); err != nil {
		t.Fatalf("a well-formed token was refused: %v", err)
	}
	// A digest written without base64 padding is the same digest.
	unpadded := bodyClaims(body, jwtNow)
	sum := sha256.Sum256(body)
	unpadded["sha256"] = base64.RawStdEncoding.EncodeToString(sum[:])
	if err := checkJWT(mintJWT(t, hs256Header, unpadded, jwtTestSecret), body); err != nil {
		t.Fatalf("the body's digest without padding was refused: %v", err)
	}

	eng := &fakeEngine{}
	h := testHandler(t, eng, jwtSource())
	first := mintJWT(t, hs256Header, bodyClaims(body, jwtNow), jwtTestSecret)
	second := mintJWT(t, hs256Header, bodyClaims(body, jwtNow.Add(time.Second)), jwtTestSecret)
	ids := map[string]bool{}
	for _, token := range []string{first, second} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, jwtRequest(body, token))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
		}
		var reply map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		ids[reply["id"]] = true
	}
	if len(ids) != 1 {
		t.Errorf("one body under two tokens staged %d rows (%v): the row's identity is the body's digest, so a sender's retry is free", len(ids), ids)
	}
	for _, staged := range eng.calls {
		if !strings.Contains(staged, "signatureVerified: true") {
			t.Errorf("a verified delivery was staged unverified:\n%s", staged)
		}
		if strings.Contains(staged, first) || strings.Contains(staged, second) || strings.Contains(strings.ToLower(staged), "authorization") {
			t.Errorf("the token reached the staged statement:\n%s", staged)
		}
	}
}

// TestJWTSchemeReadsATokenBehindAPrefix: a sender that writes "Bearer <token>" is
// configured with SIGNATURE_PREFIX=Bearer. The environment's value is trimmed at boot, so
// the prefix arrives without its space, and the token after it is read without the space
// too. A header without the prefix is refused.
func TestJWTSchemeReadsATokenBehindAPrefix(t *testing.T) {
	t.Setenv("MEMQL_INBOUND_SOURCE_ALLOWLIST", "acme")
	for k, v := range map[string]string{
		"SIGNATURE_SCHEME": SchemeJWTHS256BodySHA256, "SECRET": jwtTestSecret, "SIGNATURE_HEADER": "Authorization",
		"JWT_ISSUER": jwtTestIssuer, "SIGNATURE_PREFIX": "Bearer ",
	} {
		t.Setenv("MEMQL_INBOUND_SOURCE_ACME_"+k, v)
	}
	src, ok := LoadConfig(quietLogger()).Sources["acme"]
	if !ok {
		t.Fatal("a JWT source with a prefix was dropped")
	}
	body := []byte(`{"event":"participant_joined"}`)
	token := mintJWT(t, hs256Header, bodyClaims(body, jwtNow), jwtTestSecret)
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	if verified, err := verify(src, header, body, jwtNow, jwtTolerance); err != nil || !verified {
		t.Fatalf("a token behind its configured prefix was refused: %v", err)
	}
	header.Set("Authorization", token)
	if _, err := verify(src, header, body, jwtNow, jwtTolerance); err == nil {
		t.Error("a token without the configured prefix was accepted")
	}
}

// TestJWTSchemeRefusesAWrongBodyHash: the token signs a digest, so a body changed
// after signing, or a digest of something else, is refused -- by the receiver too,
// which stages nothing. The digest is held to the body exactly: the hex of the same
// digest is not its base64, and a sender that writes it that way is misconfigured.
func TestJWTSchemeRefusesAWrongBodyHash(t *testing.T) {
	body := []byte(`{"event":"participant_left"}`)
	token := mintJWT(t, hs256Header, bodyClaims(body, jwtNow), jwtTestSecret)
	requireUnverified(t, checkJWT(token, []byte(`{"event":"participant_joined"}`)), "the body was changed after signing")
	requireUnverified(t, checkJWT(token, append(append([]byte(nil), body...), ' ')), "a byte was appended after signing")

	hexDigest := bodyClaims(body, jwtNow)
	sum := sha256.Sum256(body)
	hexDigest["sha256"] = hex.EncodeToString(sum[:])
	requireUnverified(t, checkJWT(mintJWT(t, hs256Header, hexDigest, jwtTestSecret), body), "the digest is hex, not base64")
	notBase64 := bodyClaims(body, jwtNow)
	notBase64["sha256"] = "not a digest at all!"
	requireUnverified(t, checkJWT(mintJWT(t, hs256Header, notBase64, jwtTestSecret), body), "the digest does not decode")

	eng := &fakeEngine{}
	h := testHandler(t, eng, jwtSource())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, jwtRequest([]byte(`{"event":"room_finished"}`), token))
	if rec.Code != http.StatusUnauthorized || len(eng.calls) != 0 {
		t.Errorf("a body the token does not sign answered %d and staged %d row(s)", rec.Code, len(eng.calls))
	}
}

// TestJWTSchemeRefusesExpiryOutsideTolerance: a token is accepted from its nbf to its
// exp, each widened by the receiver's tolerance for the two clocks' skew, and not a
// moment outside that. A token with no exp is refused outright: it would be valid for
// ever, and a captured request with it replayable for ever.
func TestJWTSchemeRefusesExpiryOutsideTolerance(t *testing.T) {
	body := []byte(`{"event":"track_published"}`)
	at := func(issued time.Time) error {
		return checkJWT(mintJWT(t, hs256Header, bodyClaims(body, issued), jwtTestSecret), body)
	}
	// Expired, inside and outside the skew allowance: exp is issued + 5 minutes.
	if err := at(jwtNow.Add(-10*time.Minute + time.Second)); err != nil {
		t.Errorf("a token that expired less than the tolerance ago was refused: %v", err)
	}
	requireUnverified(t, at(jwtNow.Add(-10*time.Minute-time.Second)), "it expired more than the tolerance ago")
	// Not valid yet, inside and outside the skew allowance.
	if err := at(jwtNow.Add(jwtTolerance - time.Second)); err != nil {
		t.Errorf("a token valid from less than the tolerance ahead was refused: %v", err)
	}
	requireUnverified(t, at(jwtNow.Add(jwtTolerance+time.Second)), "it is valid only from more than the tolerance ahead")

	forever := bodyClaims(body, jwtNow)
	delete(forever, "exp")
	requireUnverified(t, checkJWT(mintJWT(t, hs256Header, forever, jwtTestSecret), body), "it carries no exp")
	notATime := bodyClaims(body, jwtNow)
	notATime["exp"] = "tomorrow"
	requireUnverified(t, checkJWT(mintJWT(t, hs256Header, notATime, jwtTestSecret), body), "its exp is not a number")
	// An nbf is optional (RFC 7519), and a token without one is valid until its exp.
	noNbf := bodyClaims(body, jwtNow)
	delete(noNbf, "nbf")
	if err := checkJWT(mintJWT(t, hs256Header, noNbf, jwtTestSecret), body); err != nil {
		t.Errorf("a token with no nbf was refused: %v", err)
	}
}

// TestJWTSchemeRefusesAWrongIssuer: the issuer is the key the sender signs as, and a
// token naming another is refused even when it verifies -- two keys sharing one
// secret are still two senders. A token naming none is refused, and so is every token
// when the source configures none: an empty issuer would match any token that left
// the claim out.
func TestJWTSchemeRefusesAWrongIssuer(t *testing.T) {
	body := []byte(`{"event":"participant_joined"}`)
	other := bodyClaims(body, jwtNow)
	other["iss"] = "APIsomebodyelse"
	requireUnverified(t, checkJWT(mintJWT(t, hs256Header, other, jwtTestSecret), body), "it names another issuer")
	none := bodyClaims(body, jwtNow)
	delete(none, "iss")
	requireUnverified(t, checkJWT(mintJWT(t, hs256Header, none, jwtTestSecret), body), "it names no issuer")

	blank := bodyClaims(body, jwtNow)
	blank["iss"] = ""
	err := verifyJWTBodySHA256(mintJWT(t, hs256Header, blank, jwtTestSecret), body, []byte(jwtTestSecret), "", jwtNow, jwtTolerance)
	requireUnverified(t, err, "the source configures no issuer")
	err = verifyJWTBodySHA256(mintJWT(t, hs256Header, bodyClaims(body, jwtNow), ""), body, nil, jwtTestIssuer, jwtNow, jwtTolerance)
	requireUnverified(t, err, "the source has no secret, so nothing it signs can be checked")
}

// TestJWTSchemeRefusesAlgNoneAndRS256: the scheme is HS256 and nothing else. A token
// whose header names another algorithm is refused BEFORE its signature is looked at --
// "none" with no signature, "none" with one, RS256 signed with the shared secret (the
// algorithm-confusion shape), HS512, a lowercase spelling, no alg at all -- and so is a
// header carrying a critical extension, which RFC 7515 makes invalid when it is not
// understood. A token signed under another secret is refused on its signature, and so
// is anything that is not three base64url parts.
func TestJWTSchemeRefusesAlgNoneAndRS256(t *testing.T) {
	body := []byte(`{"event":"participant_left"}`)
	claims := bodyClaims(body, jwtNow)

	unsigned := mintJWT(t, map[string]any{"alg": "none"}, claims, jwtTestSecret)
	unsigned = unsigned[:strings.LastIndex(unsigned, ".")+1]
	requireUnverified(t, checkJWT(unsigned, body), `alg "none" with no signature`)
	for _, header := range []map[string]any{
		{"alg": "none"},
		{"alg": "RS256", "typ": "JWT"},
		{"alg": "HS512"},
		{"alg": "hs256"},
		{"typ": "JWT"},
		{"alg": "HS256", "crit": []string{"b64"}, "b64": false},
	} {
		requireUnverified(t, checkJWT(mintJWT(t, header, claims, jwtTestSecret), body), "the header is "+mustJSON(t, header))
	}
	requireUnverified(t, checkJWT(mintJWT(t, hs256Header, claims, "another-secret-0123456789abcdefghijkl"), body), "it is signed under another secret")

	good := mintJWT(t, hs256Header, claims, jwtTestSecret)
	parts := strings.Split(good, ".")
	for name, token := range map[string]string{
		"two parts":                parts[0] + "." + parts[1],
		"four parts":               good + "." + parts[2],
		"an empty signature":       parts[0] + "." + parts[1] + ".",
		"a signature cut short":    parts[0] + "." + parts[1] + "." + parts[2][:len(parts[2])-4],
		"padded base64":            parts[0] + "." + parts[1] + "." + base64.URLEncoding.EncodeToString(mustDecode(t, parts[2])),
		"a header that is no JSON": b64url([]byte("not json")) + "." + parts[1] + "." + parts[2],
		"a Bearer prefix":          "Bearer " + good,
		"nothing":                  "",
	} {
		requireUnverified(t, checkJWT(token, body), name)
	}
}

// TestJWTSchemeRefusesAMissingSha256Claim: the digest claim is what binds the token
// to the body. Without it, or with it empty, a token is a credential that verifies
// ANY body for as long as it lives, so it is refused.
func TestJWTSchemeRefusesAMissingSha256Claim(t *testing.T) {
	body := []byte(`{"event":"room_finished"}`)
	missing := bodyClaims(body, jwtNow)
	delete(missing, "sha256")
	requireUnverified(t, checkJWT(mintJWT(t, hs256Header, missing, jwtTestSecret), body), "it carries no sha256 claim")
	empty := bodyClaims(body, jwtNow)
	empty["sha256"] = ""
	requireUnverified(t, checkJWT(mintJWT(t, hs256Header, empty, jwtTestSecret), body), "its sha256 claim is empty")
	number := bodyClaims(body, jwtNow)
	number["sha256"] = 42
	requireUnverified(t, checkJWT(mintJWT(t, hs256Header, number, jwtTestSecret), body), "its sha256 claim is not a string")
}

// TestJWTSchemeConfigIsCompleteOrDropped: an environment source using the scheme needs
// its secret, its header and the issuer the token must name; one missing any of them
// is dropped at boot, never admitted. So is one combining the scheme with a setting it
// cannot honour: a timestamp header or a composite header -- the token carries its own
// times, and an unsigned timestamp folded into the row's identity would let one
// captured delivery mint a row per value. JWT_ISSUER set on an HMAC source is refused
// too: it would read as checked and check nothing.
func TestJWTSchemeConfigIsCompleteOrDropped(t *testing.T) {
	complete := map[string]string{
		"SIGNATURE_SCHEME": SchemeJWTHS256BodySHA256, "SECRET": jwtTestSecret,
		"SIGNATURE_HEADER": "Authorization", "JWT_ISSUER": jwtTestIssuer,
	}
	load := func(t *testing.T, env map[string]string) (SourceConfig, bool) {
		t.Helper()
		t.Setenv("MEMQL_INBOUND_SOURCE_ALLOWLIST", "acme")
		for k, v := range env {
			t.Setenv("MEMQL_INBOUND_SOURCE_ACME_"+k, v)
		}
		src, ok := LoadConfig(quietLogger()).Sources["acme"]
		return src, ok
	}
	t.Run("complete", func(t *testing.T) {
		src, ok := load(t, complete)
		if !ok {
			t.Fatal("a complete JWT source was dropped")
		}
		if src.JWTIssuer != jwtTestIssuer || src.Scheme != SchemeJWTHS256BodySHA256 {
			t.Errorf("resolved %+v", src)
		}
	})
	for name, change := range map[string]map[string]string{
		"no issuer":                      {"JWT_ISSUER": ""},
		"no secret":                      {"SECRET": ""},
		"no header":                      {"SIGNATURE_HEADER": ""},
		"a timestamp header":             {"TIMESTAMP_HEADER": "X-Ts"},
		"a composite header":             {"ELEMENT_SEPARATOR": ",", "SIGNATURE_ELEMENT": "v1"},
		"an issuer on an HMAC source":    {"SIGNATURE_SCHEME": SchemeHMACSHA256Hex},
		"an issuer on an unverified one": {"SIGNATURE_SCHEME": SchemeNone},
	} {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range complete {
				env[k] = v
			}
			for k, v := range change {
				env[k] = v
			}
			if src, ok := load(t, env); ok {
				t.Errorf("a source with %s was admitted: %+v", name, src)
			}
		})
	}
}

// TestJWTSchemeIdentityIsTheBodyDigest: whatever a policy says about a timestamp
// header, a JWT source's row identity is the body's digest alone. The token, not a
// header beside it, carries the sender's times, so a value in such a header is
// unsigned, and folded into the identity it would let one captured delivery mint a
// row per value. A registered policy is built in code rather than loaded, so this is
// held where the identity is taken, not only at boot.
func TestJWTSchemeIdentityIsTheBodyDigest(t *testing.T) {
	src := jwtSource()
	src.TimestampHeader = "X-Ts"
	body := []byte(`{"event":"participant_joined"}`)
	header := http.Header{}
	header.Set("X-Ts", "1")
	first := identityKeyFor(src, header, body)
	header.Set("X-Ts", "2")
	if second := identityKeyFor(src, header, body); first != second {
		t.Errorf("an unsigned header changed a JWT source's row identity: %s, then %s", first, second)
	}
	sum := sha256.Sum256(body)
	if first != hex.EncodeToString(sum[:]) {
		t.Errorf("a JWT source's identity is %s, want the body's digest", first)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func mustDecode(t *testing.T, part string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
