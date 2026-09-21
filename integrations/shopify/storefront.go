package shopify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// storefront.go -- the Storefront GraphQL client, and the three things a
// storefront preview can ask a store (epic memql#5531, issue memql#5547).
//
// # Why it is here and not beside the preview code
//
// The module taxonomy's whole test is "does it talk to somebody else's
// system", and `shopify` is the package this repository already classifies as
// the one that talks to Shopify -- "Storefront + Admin APIs", in
// module_taxonomy_test.go's own words. A second Storefront caller in a package
// classified as a component would be exactly the drift that table exists to
// stop. So the CALL lives here, `sitePreview` reaches it through a builtin over
// the engine (the seam component/packages already uses to reach customDomainAdd),
// and the observation ROWS are written by the package that owns them.
//
// # It is not the Admin client, and must not become it
//
// The Admin API is cost-bucket paced; the Storefront API is not, and it is
// authenticated by a PUBLIC token that a browser holds anyway. So there is no
// bucket to read, no throttle to honour, and nothing here is on a backfill's
// path -- this client answers a person clicking "exercise this" a few times an
// hour. Three requests, a short timeout, no retries: a probe that retried would
// report a store as healthy that a shopper's browser would have given up on.
//
// # What it deliberately does not do
//
// It does not pay. The checkout walk happens in a browser on Shopify's hosted
// checkout and no request this process makes can witness it; the test that does
// is the product's. `checkoutUrl` coming back is the LAST thing this cluster can
// honestly observe, and the report says so rather than implying a completed
// order.

// StorefrontEndpoint builds the Storefront GraphQL URL for a store.
//
// BOTH HALVES ARE VALIDATED, for AdminEndpoint's reason exactly: they come off
// the same operator-written v1:shopify:store row, and the composed string is a
// URL this process then sends a token to. See NormalizeShopDomain for what that
// row could otherwise do -- the reasoning is an SSRF control, not tidiness, and
// it applies here identically because the row is the same row.
func StorefrontEndpoint(domain, apiVersion string) (string, error) {
	host, err := NormalizeShopDomain(domain)
	if err != nil {
		return "", err
	}
	version, err := normalizeAPIVersion(apiVersion)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("https://%s/api/%s/graphql.json", host, version), nil
}

// ProbeStep is one observation: whether it answered, the shortest true thing
// about what came back, why it did not, and how long it took.
//
// `Detail` NEVER CARRIES THE CHECKOUT URL and never carries a token. The
// observation rows these become are readable by every operator the composite
// tier admits, and a checkout URL is a live cart somebody else could pay from.
// The host is recorded instead, which is what an operator actually checks.
type ProbeStep struct {
	OK         bool
	Detail     string
	Failure    string
	DurationMs int
}

// ProbeReport is the three active observations of one exercise.
//
// THE FOURTH IS NOT HERE. An order arriving in the mirror is observed
// passively, by the connector doing its ordinary job, and nothing this client
// does could make it happen.
type ProbeReport struct {
	Catalog  ProbeStep
	Cart     ProbeStep
	Checkout ProbeStep
}

// storefrontTimeout bounds each of the two requests.
//
// TEN SECONDS, and the number is about what it is measuring. This probe exists
// to answer "would a shopper's browser get an answer"; a browser gives up long
// before a patient server client would, so a probe that waited a minute would
// report a store as working that no shopper could use.
const storefrontTimeout = 10 * time.Second

// StorefrontProbe runs the three observations against one store.
//
// TWO REQUESTS FOR THREE OBSERVATIONS, and that is honest rather than an
// economy: `cartCreate` answers both "a cart accepted a line" and "a checkoutUrl
// came back", because they are two facts about one reply. Splitting them into
// two calls would have made the second measure a second cart.
//
// THE STEPS ARE DEPENDENT AND THE REPORT SAYS SO. Nothing can be added to a cart
// without a variant to add, so a catalog that answered nothing leaves the cart
// step failed with that as its reason rather than with a GraphQL error that
// would read like a Shopify problem.
func StorefrontProbe(ctx context.Context, endpoint, token string) ProbeReport {
	var report ProbeReport

	variantID, title, step := storefrontCatalog(ctx, endpoint, token)
	report.Catalog = step
	if !step.OK {
		report.Cart = ProbeStep{Failure: "not attempted: the catalog read did not return a purchasable variant to add"}
		report.Checkout = ProbeStep{Failure: "not attempted: no cart was created"}
		return report
	}

	report.Cart, report.Checkout = storefrontCart(ctx, endpoint, token, variantID, title)
	return report
}

// storefrontCatalog asks for one purchasable variant.
//
// `first: 1` rather than a count: this is not an inventory read, it is the
// question "does this store answer, and does it have something a cart could
// hold". A store with a catalog full of unavailable products answers the first
// half and fails the second, which is a real and useful distinction for
// somebody about to exercise a storefront.
func storefrontCatalog(ctx context.Context, endpoint, token string) (variantID, title string, step ProbeStep) {
	const query = `{ products(first: 1) { edges { node { title handle variants(first: 1) { edges { node { id availableForSale } } } } } } }`
	started := time.Now()
	var out struct {
		Products struct {
			Edges []struct {
				Node struct {
					Title    string `json:"title"`
					Handle   string `json:"handle"`
					Variants struct {
						Edges []struct {
							Node struct {
								ID               string `json:"id"`
								AvailableForSale bool   `json:"availableForSale"`
							} `json:"node"`
						} `json:"edges"`
					} `json:"variants"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"products"`
	}
	if err := storefrontCall(ctx, endpoint, token, query, nil, &out); err != nil {
		return "", "", ProbeStep{Failure: err.Error(), DurationMs: sinceMs(started)}
	}
	if len(out.Products.Edges) == 0 {
		return "", "", ProbeStep{
			Failure:    "the store answered, and its catalog is empty -- there is no product for a cart to hold",
			DurationMs: sinceMs(started),
		}
	}
	node := out.Products.Edges[0].Node
	if len(node.Variants.Edges) == 0 {
		return "", "", ProbeStep{
			Failure:    fmt.Sprintf("the store answered with product %q, which has no variant a cart could hold", node.Handle),
			DurationMs: sinceMs(started),
		}
	}
	variant := node.Variants.Edges[0].Node
	step = ProbeStep{
		OK:         true,
		Detail:     fmt.Sprintf("read product %q", node.Handle),
		DurationMs: sinceMs(started),
	}
	if !variant.AvailableForSale {
		// STILL OK, and the detail says why it matters. The catalog read is what
		// was being observed and it answered; an unavailable variant is the
		// CART's problem, and reporting it as a catalog failure would send an
		// operator looking at the wrong thing.
		step.Detail += " (its first variant is not available for sale)"
	}
	return variant.ID, node.Title, step
}

// storefrontCart creates a cart holding one line and reads the checkout URL off
// the same reply.
func storefrontCart(ctx context.Context, endpoint, token, variantID, title string) (cart, checkout ProbeStep) {
	const mutation = `mutation previewCart($lines: [CartLineInput!]!) {
  cartCreate(input: { lines: $lines }) {
    cart { id checkoutUrl totalQuantity }
    userErrors { field message }
  }
}`
	started := time.Now()
	var out struct {
		CartCreate struct {
			Cart *struct {
				ID            string `json:"id"`
				CheckoutURL   string `json:"checkoutUrl"`
				TotalQuantity int    `json:"totalQuantity"`
			} `json:"cart"`
			UserErrors []struct {
				Field   []string `json:"field"`
				Message string   `json:"message"`
			} `json:"userErrors"`
		} `json:"cartCreate"`
	}
	vars := map[string]any{"lines": []map[string]any{{"merchandiseId": variantID, "quantity": 1}}}
	if err := storefrontCall(ctx, endpoint, token, mutation, vars, &out); err != nil {
		ms := sinceMs(started)
		return ProbeStep{Failure: err.Error(), DurationMs: ms},
			ProbeStep{Failure: "not attempted: the cart was not created", DurationMs: 0}
	}
	ms := sinceMs(started)
	if len(out.CartCreate.UserErrors) > 0 {
		msgs := make([]string, 0, len(out.CartCreate.UserErrors))
		for _, e := range out.CartCreate.UserErrors {
			msgs = append(msgs, e.Message)
		}
		return ProbeStep{Failure: "the store refused the line: " + strings.Join(msgs, "; "), DurationMs: ms},
			ProbeStep{Failure: "not attempted: the cart was not created", DurationMs: 0}
	}
	if out.CartCreate.Cart == nil {
		return ProbeStep{Failure: "the store returned no cart and no error, which is a reply this client cannot read as either outcome", DurationMs: ms},
			ProbeStep{Failure: "not attempted: the cart was not created", DurationMs: 0}
	}

	cart = ProbeStep{
		OK:         true,
		Detail:     fmt.Sprintf("a cart accepted one line of %q", title),
		DurationMs: ms,
	}

	// THE URL ITSELF IS NOT RECORDED -- only its host. See ProbeStep.
	raw := strings.TrimSpace(out.CartCreate.Cart.CheckoutURL)
	if raw == "" {
		checkout = ProbeStep{Failure: "the cart was created and carried no checkoutUrl, so there is nowhere for a shopper to pay"}
		return cart, checkout
	}
	checkout = ProbeStep{
		OK:     true,
		Detail: "checkout is hosted at " + checkoutHost(raw) + "; the payment walk itself happens in a browser and is not something this cluster can observe",
	}
	return cart, checkout
}

// checkoutHost reduces a checkout URL to its host. A parse failure answers a
// fixed string rather than the URL: the whole point of reducing it is that the
// URL must not reach a row, and a fallback that leaked it on a parse error
// would be the leak.
func checkoutHost(raw string) string {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
	if i := strings.IndexAny(trimmed, "/?#"); i >= 0 {
		trimmed = trimmed[:i]
	}
	trimmed = strings.TrimSpace(trimmed)
	if trimmed == "" || strings.ContainsAny(trimmed, " \t") {
		return "a host this client could not read"
	}
	return trimmed
}

// storefrontCall posts one GraphQL document and decodes `data` into out.
//
// EVERY FAILURE COMES BACK AS A SENTENCE, not as a wrapped error chain, because
// the string lands on an observation row an operator reads. "401 -- the
// Storefront token was refused" is a diagnosis; "Post
// \"https://...\": ... : unexpected status" is a stack trace with a URL in it.
func storefrontCall(ctx context.Context, endpoint, token, query string, vars map[string]any, out any) error {
	body := map[string]any{"query": query}
	if len(vars) > 0 {
		body["variables"] = vars
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("could not build the Storefront request: %w", err)
	}

	callCtx, cancel := context.WithTimeout(ctx, storefrontTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("could not build the Storefront request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Shopify-Storefront-Access-Token", token)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if callCtx.Err() != nil {
			return fmt.Errorf("the store did not answer within %s", storefrontTimeout)
		}
		return fmt.Errorf("the store could not be reached: %w", err)
	}
	defer resp.Body.Close()

	// 1 MiB is far more than any of these replies, and it is a bound rather
	// than a size estimate: this reads somebody else's server into memory.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("the store's reply could not be read: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("the Storefront token was refused (%d) -- check storefrontTokenRef on the store row and that the token is a Storefront API token rather than an Admin one", resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("the store answered 404 -- check the domain and the pinned API version on the store row")
	case resp.StatusCode >= 400:
		return fmt.Errorf("the store answered %d", resp.StatusCode)
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("the store's reply was not JSON this client could read")
	}
	if len(envelope.Errors) > 0 {
		msgs := make([]string, 0, len(envelope.Errors))
		for _, e := range envelope.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("the store returned an error: %s", strings.Join(msgs, "; "))
	}
	if len(envelope.Data) == 0 {
		return fmt.Errorf("the store returned neither data nor an error")
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("the store's reply did not have the shape this client asked for")
	}
	return nil
}

func sinceMs(started time.Time) int {
	return int(time.Since(started).Milliseconds())
}
