package inboundhop

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/inbound"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify"
	"google.golang.org/protobuf/types/known/structpb"
)

// The APP-LEVEL twin of TestWebhookReceiptAndDispatchAcrossEngines' staged
// metadata assertion: a compliance delivery on /inbound/shopify, verified by
// the managed app's secret, stages exactly the allowlisted X-Shopify-*
// headers the request carried, and neither the bearer, the cookie, the
// signature nor an unrequested header.
//
// The receiver and the connector are both the real ones: the receiver
// resolves `shopify` through memqlsync to the bound connector, whose
// managedInboundSource supplies the allowlist and the secret. Only their
// engines are fakes, and on purpose: the managed app's client id and secret
// are GLOBAL rows keyed by a fixed name, which integrations/shopify's own
// db-gated suite also seeds, so seeding them here would make both packages
// read an ambiguous name whenever the db-tests lane runs them together.
func TestAppLevelReceiptStagesExactlyTheAllowlistedMetadata(t *testing.T) {
	const appSecret = "managed-app-secret"
	conn := shopify.NewConnector(managedAppEngine{}, nil,
		shopify.NewStoreRegistry(managedAppEngine{}, func(_ context.Context, name string) (string, error) {
			if name == "SHOPIFY_CONNECT_CLIENT_SECRET" {
				return appSecret, nil
			}
			return "", nil
		}), shopify.NewAdminClient())
	if err := memqlsync.Bind(conn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { memqlsync.UnbindForTest(shopify.ConnectorName) })

	const shop = "acme.myshopify.com"
	body := `{"shop_domain":"` + shop + `","shop_id":954889}`
	mac := hmac.New(sha256.New, []byte(appSecret))
	_, _ = mac.Write([]byte(body))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/inbound/"+shopify.ConnectorName, strings.NewReader(body))
	req.Header.Set(shopify.HeaderHMAC, signature)
	delivery := map[string]string{
		"X-Shopify-Topic":        shopify.TopicShopRedact,
		"X-Shopify-Shop-Domain":  shop,
		"X-Shopify-Webhook-Id":   "wh-app-level-1",
		"X-Shopify-Event-Id":     "ev-app-level-1",
		"X-Shopify-Triggered-At": "2026-09-27T12:00:00Z",
		"X-Shopify-Api-Version":  "2026-07",
	}
	for k, v := range delivery {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer must-not-persist")
	req.Header.Set("Cookie", "session=must-not-persist")
	req.Header.Set("X-Unrequested", "leak-me")

	eng := &stagingCapture{}
	rec := httptest.NewRecorder()
	inbound.NewHandler(inbound.Config{Enabled: true, MaxBodyBytes: 4096}, eng, nil).ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || len(eng.calls) != 1 {
		t.Fatalf("app-level receipt: %d, %d staging calls: %s", rec.Code, len(eng.calls), rec.Body)
	}

	args := stagedArgs(t, eng.calls[0])
	if args["source"] != shopify.ConnectorName {
		t.Fatalf("staged under source %q, want the app-level %q", args["source"], shopify.ConnectorName)
	}
	var staged map[string]string
	if err := json.Unmarshal([]byte(args["headersJson"]), &staged); err != nil {
		t.Fatalf("staged headersJson %q: %v", args["headersJson"], err)
	}
	want := map[string]string{}
	for k, v := range delivery {
		want[strings.ToLower(k)] = v
	}
	if !maps.Equal(staged, want) {
		t.Fatalf("staged delivery headers %v, want exactly %v", staged, want)
	}
	for _, secret := range []string{"must-not-persist", "leak-me", signature} {
		if strings.Contains(eng.calls[0], secret) {
			t.Errorf("the staging call carries %q, which no allowlist names", secret)
		}
	}
}

// managedAppEngine answers the two named-row reads the connector makes to
// decide whether the managed app can sign: the client id variable and the
// sealed-secret row. The sealed VALUE is never decrypted here -- the
// registry's resolver above supplies the plaintext -- it only has to be
// present, which is what managedAppSigning requires of it.
type managedAppEngine struct{ memql.IntegrationEngineAccess }

func (managedAppEngine) Execute(_ context.Context, q string) (*memql.ExecuteResult, error) {
	var payload map[string]any
	switch {
	case strings.Contains(q, `"SHOPIFY_CONNECT_CLIENT_ID"`):
		payload = map[string]any{"name": "SHOPIFY_CONNECT_CLIENT_ID", "value": "managed-app"}
	case strings.Contains(q, `"SHOPIFY_CONNECT_CLIENT_SECRET"`):
		payload = map[string]any{"name": "SHOPIFY_CONNECT_CLIENT_SECRET", "encryptedValue": "sealed"}
	default:
		return &memql.ExecuteResult{}, nil
	}
	pb, err := structpb.NewStruct(payload)
	if err != nil {
		return nil, err
	}
	return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{Id: "row", Payload: pb}}}}, nil
}

// stagingCapture records the receiver's staging call instead of running it.
type stagingCapture struct{ calls []string }

func (s *stagingCapture) Execute(_ context.Context, q string) (any, error) {
	s.calls = append(s.calls, q)
	return nil, nil
}

// stagedArgs reads the rendered staging call back through the real MemQL
// lexer and parser, so a value is compared as the engine would decode it.
func stagedArgs(t *testing.T, call string) map[string]string {
	t.Helper()
	tokens, err := langparser.NewLexer(call).Tokenize()
	if err != nil {
		t.Fatalf("the MemQL lexer refused the staging call: %v\n%s", err, call)
	}
	if _, err := langparser.NewParser(tokens).Parse(); err != nil {
		t.Fatalf("the MemQL parser refused the staging call: %v\n%s", err, call)
	}
	args := map[string]string{}
	for i := 0; i+2 < len(tokens); i++ {
		if tokens[i].Type == langparser.TokenIdentifier && tokens[i+1].Type == langparser.TokenColon {
			args[tokens[i].Literal] = tokens[i+2].Literal
		}
	}
	return args
}
