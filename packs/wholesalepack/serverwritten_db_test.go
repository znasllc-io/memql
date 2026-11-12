package wholesalepack_test

// serverwritten_db_test.go -- THE PACK'S ROWS ARE WRITTEN ONLY BY THE PACK
// (ruling 50). The engine's raw insert() is public language and never consults
// a mutation's @serverOnly; @serverWritten on a concept refuses every write
// without internal origin. Every concept the pack declares carries it: a raw
// insert is refused for the store's merchant, for a person holding the
// wholesale part and for a stranger, and the pack's own acts still write.
//
// Postgres-gated like every *_db_test.go.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	wholesalepack "github.com/znasllc-io/memql/packs/wholesalepack"
)

// rawInsert is the public insert() a client may write.
func rawInsert(concept, id string, payload map[string]any) string {
	raw, _ := json.Marshal(payload)
	return `insert(` + quote(concept) + `, id=` + quote(id) + `, payload=` + string(raw) + `)`
}

const serverWrittenRefusal = "@serverWritten requires internal origin"

// payloadsFor is a plausible row of every concept the pack declares, on s.
func payloadsFor(s accessStore) map[string]map[string]any {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	app := "v1:wholesale:application:" + s.appID
	zeros := strings.Repeat("0", 64)
	return map[string]map[string]any{
		"v1:wholesale:application": {"ownerUserId": s.owner, "storeId": s.store, "accountId": s.account,
			"companyName": "Kestrel Cycles", "applicantName": "Ada Example", "applicantEmail": "ada@kestrel.example.com"},
		"v1:wholesale:applicationDecision": {"applicationId": app, "storeId": s.store, "transition": "approve",
			"decidedBy": userRef(s.staff), "ownerUserId": s.owner, "accountId": s.account, "principalKind": "client"},
		"v1:wholesale:entitlement": {"ownerUserId": s.owner, "applicationId": app, "storeId": s.store,
			"buyerEmail": "ada@kestrel.example.com", "adapter": "customerTag", "state": "granted", "accountId": s.account},
		"v1:wholesale:wholesaleSettings": {"ownerUserId": s.owner, "storeId": s.store, "applicationsOpen": true,
			"accountId": s.account, "termsRequired": false},
		"v1:wholesale:documentVersion": {"ownerUserId": s.owner, "storeId": s.store, "accountId": s.account, "kind": "terms",
			"label": "Planted terms", "body": "Planted.", "sha256": zeros, "publishedBy": userRef(s.staff), "publishedAt": now},
		"v1:wholesale:privateLink": {"ownerUserId": s.owner, "storeId": s.store, "accountId": s.account, "applicationId": app,
			"purpose": "terms", "tokenHash": zeros, "issuedAt": now, "expiresAt": now},
		"v1:wholesale:acceptance": {"ownerUserId": s.owner, "storeId": s.store, "accountId": s.account, "applicationId": app,
			"applicantEmail": "forged@example.com", "signedName": "Forged Name", "acceptedAt": now,
			"termsVersionId": "forged", "termsSha256": zeros, "mapVersionId": "forged", "mapSha256": zeros, "statement": "forged"},
	}
}

// EVERY CONCEPT THE PACK DECLARES refuses a raw insert from the merchant, from a
// person holding the wholesale part, and from a stranger, whether the row is new
// or the pack's own.
func TestARawInsertOfEveryPackConceptIsRefused(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s, _ := termsStore(t, eng, "srvw-raw")
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	existing := map[string]string{
		"v1:wholesale:application":       s.appID,
		"v1:wholesale:wholesaleSettings": wholesalepack.SettingsRowID(s.store),
		"v1:wholesale:privateLink":       wholesalepack.PrivateLinkRowID(wholesalepack.LinkPurposeTerms, s.appID),
	}
	for concept, payload := range payloadsFor(s) {
		for who, actor := range map[string]string{"merchant": s.merchant, "staff": s.staff, "stranger": "user-srvw-stranger-" + n} {
			ids := []string{"srvw-" + who + "-" + n}
			if id := existing[concept]; id != "" {
				ids = append(ids, id)
			}
			for _, id := range ids {
				before := versionCount(t, concept, id)
				_, err := eng.Execute(signedIn(actor), rawInsert(concept, id, payload))
				if err == nil || !strings.Contains(err.Error(), serverWrittenRefusal) {
					t.Errorf("%s: a raw insert by the %s at %s answered %v, want %q", concept, who, id, err, serverWrittenRefusal)
				}
				if after := versionCount(t, concept, id); after != before {
					t.Errorf("%s: the %s's raw insert at %s wrote a version", concept, who, id)
				}
			}
		}
	}
}

// THE PACK'S OWN ACTS STILL WRITE every concept, through its writer: the
// application (the shopper's post), the settings (an update), the documents (a
// publish), the link (a mint), the acceptance and the link's usedAt (the
// acceptance form), the decision (an approve) and the entitlement (the store's
// automation provisioning it).
func TestThePacksOwnActsStillWriteEveryConcept(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s, token := termsStore(t, eng, "srvw-acts")
	_, terms := readTerms(t, eng, s, token)
	docs := listRows(terms["documents"])
	form := url.Values{"requestToken": {token}, "acceptTerms": {"on"}, "acceptMap": {"on"}, "signedName": {"Ada Example"},
		"termsSha256": {asString(docs[0]["sha256"])}, "mapSha256": {asString(docs[1]["sha256"])},
		"statementSha256": {asString(terms["statementSha256"])}}
	if loc := postAcceptance(t, eng, s, form).Header().Get("Location"); loc != "/wholesale/accepted" {
		t.Fatalf("the acceptance redirected to %q", loc)
	}
	if err := s.approve(eng, signedIn(s.staff)); err != nil {
		t.Fatalf("approve: %v", err)
	}
	wsExec(t, eng, auth.ContextWithInternalOrigin(asSiteOwner(s.owner)), `builtin wholesaleProvisionEntitlement(applicationId: `+quote(s.appID)+`)`)
	app := "v1:wholesale:application:" + s.appID
	for name, q := range map[string]string{
		"application": `query applicationById(applicationId: ` + quote(s.appID) + `)`,
		"settings":    `query wholesaleSettingsForStore(storeId: ` + quote(s.store) + `)`,
		"documents":   `query documentVersionsForStore(storeId: ` + quote(s.store) + `, kind: "terms")`,
		"link":        `query privateLinkForApplication(applicationId: ` + quote(app) + `, purpose: "terms")`,
		"acceptance":  `query acceptanceForApplication(applicationId: ` + quote(app) + `)`,
		"decision":    `query decisionsForApplication(applicationId: ` + quote(app) + `)`,
		"entitlement": `query entitlementForApplication(applicationId: ` + quote(app) + `)`,
	} {
		if rows := wsRows(t, eng, signedIn(s.merchant), q); len(rows) == 0 {
			t.Errorf("the pack's own act wrote no %s", name)
		}
	}
	if link := latestPayload(t, "v1:wholesale:privateLink", wholesalepack.PrivateLinkRowID(wholesalepack.LinkPurposeTerms, s.appID)); asString(link["usedAt"]) == "" {
		t.Fatalf("the link was not marked used: %+v", link)
	}
}

// THE REVIEWER'S PROBE, NOW REFUSED: an acceptance written around the pack.
// The stage stays waitingForTerms and the approve stays refused.
func TestAForgedAcceptanceIsRefusedAtTheWrite(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s, _ := termsStore(t, eng, "srvw-forged")
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	for who, actor := range map[string]string{"merchant": s.merchant, "staff": s.staff, "stranger": "user-srvw-stranger-" + n} {
		_, err := eng.Execute(signedIn(actor), rawInsert("v1:wholesale:acceptance", wholesalepack.AcceptanceRowID(s.appID),
			payloadsFor(s)["v1:wholesale:acceptance"]))
		if err == nil || !strings.Contains(err.Error(), serverWrittenRefusal) {
			t.Fatalf("a forged acceptance by the %s answered %v", who, err)
		}
	}
	got := answerOf(t, eng, signedIn(s.staff), `builtin wholesaleGet(applicationId: `+quote(s.appID)+`)`)
	if got["stage"] != wholesalepack.StageWaitingForTerms || got["acceptance"] != nil {
		t.Fatalf("after the refused forgery the application is %v with %v", got["stage"], got["acceptance"])
	}
	if err := s.approve(eng, signedIn(s.staff)); err == nil || !strings.Contains(err.Error(), wholesalepack.NotAcceptedSentence) {
		t.Fatalf("the approve after the refused forgery: %v", err)
	}
}

// THE REVIEWER'S PROBE, NOW REFUSED: a link row carrying the hash of a token
// the writer chose. The chosen token opens nothing.
func TestAPlantedLinkIsRefusedAtTheWrite(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s, _ := termsStore(t, eng, "srvw-planted")
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	token := "a-token-the-writer-chose-" + n
	payload := payloadsFor(s)["v1:wholesale:privateLink"]
	payload["tokenHash"] = sha256Text(token)
	payload["expiresAt"] = time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano)
	for who, actor := range map[string]string{"merchant": s.merchant, "staff": s.staff, "stranger": "user-srvw-stranger-" + n} {
		_, err := eng.Execute(signedIn(actor), rawInsert("v1:wholesale:privateLink", "srvw-planted-"+who+"-"+n, payload))
		if err == nil || !strings.Contains(err.Error(), serverWrittenRefusal) {
			t.Fatalf("a planted link by the %s answered %v", who, err)
		}
	}
	if code, got := readTerms(t, eng, s, token); code != 422 || got["error"] != wholesalepack.RefusalLinkInvalid {
		t.Fatalf("the chosen token answered %d %+v, want link_invalid", code, got)
	}
}

// THE REVIEWER'S PROBE, NOW REFUSED: the genuine acceptance written again by
// its own row's readers. The name and the address stay as accepted.
func TestAGenuineAcceptanceCannotBeRewritten(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s, token := termsStore(t, eng, "srvw-rewrite")
	_, terms := readTerms(t, eng, s, token)
	docs := listRows(terms["documents"])
	form := url.Values{"requestToken": {token}, "acceptTerms": {"on"}, "acceptMap": {"on"}, "signedName": {"Ada Example"},
		"termsSha256": {asString(docs[0]["sha256"])}, "mapSha256": {asString(docs[1]["sha256"])},
		"statementSha256": {asString(terms["statementSha256"])}}
	if loc := postAcceptance(t, eng, s, form).Header().Get("Location"); loc != "/wholesale/accepted" {
		t.Fatalf("the acceptance redirected to %q", loc)
	}
	rowID := wholesalepack.AcceptanceRowID(s.appID)
	for who, actor := range map[string]string{"merchant": s.merchant, "staff": s.staff} {
		_, err := eng.Execute(signedIn(actor), rawInsert("v1:wholesale:acceptance", rowID,
			map[string]any{"signedName": "Rewritten By " + who, "clientAddress": "203.0.113.77"}))
		if err == nil || !strings.Contains(err.Error(), serverWrittenRefusal) {
			t.Fatalf("a rewrite by the %s answered %v", who, err)
		}
	}
	stored := latestPayload(t, "v1:wholesale:acceptance", rowID)
	if stored["signedName"] != "Ada Example" || stored["clientAddress"] != "198.51.100.7" || versionCount(t, "v1:wholesale:acceptance", rowID) != 1 {
		t.Fatalf("the acceptance is now %+v", stored)
	}
}

// sha256Text is the hex SHA-256 of text.
func sha256Text(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
