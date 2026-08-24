package shopify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"sort"
	"strings"
	stdsync "sync"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// propagate.go -- the push channel, proven on a domain that cannot break a
// store (design D7).
//
// The first thing MemQL writes into a live Shopify store is a metafield in a
// namespace nobody else uses. That is deliberate: metafields are additive,
// they change no price, no inventory and no order, and a wrong one is
// invisible to a shopper. B2B catalogs and price lists are the next flip, and
// they get their own review when the wholesale application asks -- not a
// quiet extension of this one.
//
// THE DEAD-LETTER RULE. Shopify reports validation failures as `userErrors`
// inside a 200 response. Those will fail identically forever: a metafield
// whose value does not match its definition's type is not going to start
// matching. So a userError DEAD-LETTERS -- it stops, it is recorded, and a
// person looks at it -- while a throttle or a 5xx retries. Retrying a
// userError is how a queue stops draining, with the outbox growing and every
// individual attempt looking like a transient failure.

// MemQLMetafieldNamespace is the namespace every pushed metafield lives in.
// One namespace, ours, so nothing this connector writes can collide with the
// merchant's own metafields or another app's.
const MemQLMetafieldNamespace = "memql"

// bulkMutationThreshold is how many metafields make a batch worth staging.
//
// metafieldsSet takes up to 25 in one call, so below a few hundred the direct
// path is simply faster: a bulk mutation costs a staged upload, a poll loop
// and a JSONL round trip. Above it, the direct path is hundreds of calls
// against a cost bucket a backfill may already be using.
const bulkMutationThreshold = 250

const metafieldsSetMutation = `mutation ShopifyMetafieldsSet($metafields: [MetafieldsSetInput!]!) {
  metafieldsSet(metafields: $metafields) {
    metafields { id namespace key }
    userErrors { field message code }
  }
}`

const metafieldDefinitionCreateMutation = `mutation ShopifyMetafieldDefinitionCreate($definition: MetafieldDefinitionInput!) {
  metafieldDefinitionCreate(definition: $definition) {
    createdDefinition { id name }
    userErrors { field message code }
  }
}`

const stagedUploadsCreateMutation = `mutation ShopifyStagedUploadsCreate($input: [StagedUploadInput!]!) {
  stagedUploadsCreate(input: $input) {
    stagedTargets { url resourceUrl parameters { name value } }
    userErrors { field message }
  }
}`

const bulkMutationRunMutation = `mutation ShopifyBulkMutationRun($mutation: String!, $stagedUploadPath: String!) {
  bulkOperationRunMutation(mutation: $mutation, stagedUploadPath: $stagedUploadPath) {
    bulkOperation { id status }
    userErrors { field message }
  }
}`

// metafieldProjection is one MemQL field's landing place in Shopify.
type metafieldProjection struct {
	// Key is the metafield key inside the memql namespace.
	Key string
	// Type is the Shopify metafield type the value must match.
	Type string
	// OwnerType is the Admin MetafieldOwnerType the definition binds to.
	OwnerType string
	// StorefrontAccess says whether the headless storefront may read it.
	// public_read only where the storefront needs it: a note about a
	// customer is internal, and making it storefront-readable would put it
	// one query away from anybody with the public token.
	StorefrontAccess string
	// From is the MemQL field the value comes from.
	From string
	// OwnerFrom is the MemQL field carrying the target GID.
	OwnerFrom string
}

// projections is the closed set of MemQL-origin fields this connector pushes.
//
// Closed, and reviewed as a set rather than derived from @mirroredTo: a
// concept declaring that annotation says "somebody pushes this", and WHAT
// lands where in a live store is a decision each field earns individually.
var projections = map[string][]metafieldProjection{
	"v1:commerce:productContent": {
		{Key: "description", Type: "multi_line_text_field", OwnerType: "PRODUCT", StorefrontAccess: "PUBLIC_READ", From: "description", OwnerFrom: "productGid"},
		{Key: "summary", Type: "single_line_text_field", OwnerType: "PRODUCT", StorefrontAccess: "PUBLIC_READ", From: "summary", OwnerFrom: "productGid"},
		{Key: "keywords", Type: "list.single_line_text_field", OwnerType: "PRODUCT", StorefrontAccess: "PUBLIC_READ", From: "keywords", OwnerFrom: "productGid"},
		{Key: "blocks", Type: "json", OwnerType: "PRODUCT", StorefrontAccess: "PUBLIC_READ", From: "blocks", OwnerFrom: "productGid"},
	},
	"v1:commerce:customerNote": {
		{Key: "note", Type: "multi_line_text_field", OwnerType: "CUSTOMER", StorefrontAccess: "", From: "note", OwnerFrom: "customerGid"},
	},
	"v1:commerce:companyLocationNote": {
		{Key: "note", Type: "multi_line_text_field", OwnerType: "COMPANY_LOCATION", StorefrontAccess: "", From: "note", OwnerFrom: "companyLocationGid"},
	},
	"v1:commerce:creditLimit": {
		{Key: "creditLimit", Type: "money", OwnerType: "COMPANY_LOCATION", StorefrontAccess: "", From: "limitAmount", OwnerFrom: "companyLocationGid"},
		{Key: "creditLimitStatus", Type: "single_line_text_field", OwnerType: "COMPANY_LOCATION", StorefrontAccess: "", From: "status", OwnerFrom: "companyLocationGid"},
	},
}

// metafieldInput is one entry of metafieldsSet's input.
type metafieldInput struct {
	OwnerID   string `json:"ownerId"`
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
	Type      string `json:"type"`
	Value     string `json:"value"`
}

// definitionCache remembers which metafield definitions have been created,
// per store. Creating one is idempotent at the far end (a duplicate answers
// with a TAKEN userError), so this saves a round trip rather than preventing
// a mistake.
type definitionCache struct {
	mu   stdsync.Mutex
	seen map[string]bool
}

func newDefinitionCache() *definitionCache { return &definitionCache{seen: map[string]bool{}} }

func (d *definitionCache) mark(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen[key] {
		return false
	}
	d.seen[key] = true
	return true
}

// Propagate implements sync.Connector: push one MemQL-authored change.
//
// A refusal the far end will repeat -- a Shopify userError, an
// unconfigured store, a concept with no projection -- is returned as
// sync.Permanent, which the drain dead-letters immediately instead of
// spending its attempt budget on a request that cannot succeed. A
// throttle, a 429 or a 5xx is returned bare and retried.
func (c *Connector) Propagate(ctx context.Context, entry memqlsync.OutboxEntry) (memqlsync.PropagateResult, error) {
	specs, known := projections[entry.Concept]
	if !known {
		return memqlsync.PropagateResult{}, memqlsync.Permanentf("no projection is defined for %s", entry.Concept)
	}
	storeID, _ := entry.Payload["storeId"].(string)
	store, ok := c.stores.ByID(ctx, storeID)
	if !ok {
		return memqlsync.PropagateResult{}, memqlsync.Permanentf("store %q is not configured", storeID)
	}
	if store.RedactedAt != "" {
		return memqlsync.PropagateResult{}, memqlsync.Permanentf("store %q has been redacted", storeID)
	}
	if entry.Action == memqlsync.OutboxRetire {
		// A retirement clears the metafields rather than deleting the
		// row's owner: MemQL retiring its own note must not touch the
		// merchant's product.
		return c.propagateRetire(ctx, store, specs, entry)
	}

	inputs, err := metafieldInputsFor(specs, entry)
	if err != nil {
		return memqlsync.PropagateResult{}, memqlsync.Permanent(err)
	}
	if len(inputs) == 0 {
		// Nothing to push: every projected field is empty. DELIVERED
		// rather than failed -- there is no error here, the row simply
		// has nothing in it yet, and retrying would never change that.
		return memqlsync.PropagateResult{AlreadyDelivered: true}, nil
	}

	if err := c.ensureDefinitions(ctx, store, specs); err != nil {
		// A definition we could not create is not fatal: metafieldsSet
		// creates an undefined metafield anyway (it just has no type
		// validation or storefront access). Logged, and the push
		// continues, because refusing here would stop the whole channel
		// over an optional refinement.
		c.logger.Warn("shopify: could not ensure metafield definitions", "store", store.ID, "error", err)
	}

	if len(inputs) >= bulkMutationThreshold {
		return c.propagateBulk(ctx, store, inputs)
	}
	return c.propagateDirect(ctx, store, inputs)
}

// propagateRetire clears a row's projected metafields.
func (c *Connector) propagateRetire(ctx context.Context, store Store, specs []metafieldProjection, entry memqlsync.OutboxEntry) (memqlsync.PropagateResult, error) {
	var inputs []metafieldInput
	for _, spec := range specs {
		owner, _ := entry.Payload[spec.OwnerFrom].(string)
		if owner == "" {
			continue
		}
		inputs = append(inputs, metafieldInput{
			OwnerID: owner, Namespace: MemQLMetafieldNamespace,
			Key: spec.Key, Type: spec.Type, Value: emptyValueFor(spec.Type),
		})
	}
	if len(inputs) == 0 {
		return memqlsync.PropagateResult{AlreadyDelivered: true}, nil
	}
	return c.propagateDirect(ctx, store, inputs)
}

// emptyValueFor is what a retirement writes: a value the metafield's
// declared type accepts and a reader sees as absent. Shopify has no
// "unset" through metafieldsSet, and metafieldsDelete would remove a
// definition-bearing field the storefront queries by name.
func emptyValueFor(metafieldType string) string {
	switch {
	case strings.HasPrefix(metafieldType, "list."):
		return "[]"
	case metafieldType == "json":
		return "{}"
	default:
		return ""
	}
}

// metafieldInputsFor turns one outbox entry into metafield inputs.
func metafieldInputsFor(specs []metafieldProjection, entry memqlsync.OutboxEntry) ([]metafieldInput, error) {
	var out []metafieldInput
	for _, spec := range specs {
		owner, _ := entry.Payload[spec.OwnerFrom].(string)
		if owner == "" {
			return nil, fmt.Errorf("row carries no %s, so there is nothing to write the metafield on", spec.OwnerFrom)
		}
		raw, present := entry.Payload[spec.From]
		if !present {
			continue
		}
		value, ok, err := metafieldValue(spec.Type, raw)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		out = append(out, metafieldInput{
			OwnerID:   owner,
			Namespace: MemQLMetafieldNamespace,
			Key:       spec.Key,
			Type:      spec.Type,
			Value:     value,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// metafieldValue renders a MemQL value in the string form the metafield's
// declared type requires. Shopify takes every metafield value as a STRING and
// validates it against the type, so a list has to arrive as JSON and a money
// value as a JSON object -- getting this wrong is a userError, which
// dead-letters, which is why it is checked here first.
func metafieldValue(metafieldType string, raw any) (string, bool, error) {
	switch {
	case raw == nil:
		return "", false, nil
	case strings.HasPrefix(metafieldType, "list."), metafieldType == "json":
		if s, ok := raw.(string); ok {
			if strings.TrimSpace(s) == "" {
				return "", false, nil
			}
			return s, true, nil
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			return "", false, fmt.Errorf("value for a %s metafield could not be encoded: %w", metafieldType, err)
		}
		if string(encoded) == "null" || string(encoded) == "[]" || string(encoded) == "{}" {
			return "", false, nil
		}
		return string(encoded), true, nil
	case metafieldType == "money":
		amount := fmt.Sprintf("%v", raw)
		if strings.TrimSpace(amount) == "" {
			return "", false, nil
		}
		encoded, err := json.Marshal(map[string]any{"amount": amount, "currency_code": "USD"})
		if err != nil {
			return "", false, err
		}
		return string(encoded), true, nil
	default:
		s := fmt.Sprintf("%v", raw)
		if strings.TrimSpace(s) == "" {
			return "", false, nil
		}
		return s, true, nil
	}
}

// ensureDefinitions creates the metafield definitions the projections need.
//
// A definition is what gives a metafield its type validation, its admin UI
// and -- for productContent -- its storefront access. Without one the value
// still lands, so this is a refinement rather than a prerequisite, which is
// why a failure here is logged rather than fatal.
func (c *Connector) ensureDefinitions(ctx context.Context, store Store, specs []metafieldProjection) error {
	for _, spec := range specs {
		cacheKey := store.ID + "\x00" + spec.OwnerType + "\x00" + spec.Key
		if !c.definitions.mark(cacheKey) {
			continue
		}
		definition := map[string]any{
			"name":      "MemQL " + spec.Key,
			"namespace": MemQLMetafieldNamespace,
			"key":       spec.Key,
			"type":      spec.Type,
			"ownerType": spec.OwnerType,
		}
		if spec.StorefrontAccess != "" {
			definition["access"] = map[string]any{"storefront": spec.StorefrontAccess}
		}
		resp, err := c.adminCall(ctx, store, metafieldDefinitionCreateMutation, "ShopifyMetafieldDefinitionCreate", map[string]any{"definition": definition})
		if err != nil {
			return err
		}
		if ueErr := userErrorsFrom(resp, "metafieldDefinitionCreate"); ueErr != nil {
			// TAKEN means it already exists, which is the expected answer
			// on every run after the first.
			if strings.Contains(strings.ToUpper(ueErr.Error()), "TAKEN") {
				continue
			}
			return ueErr
		}
	}
	return nil
}

func (c *Connector) propagateDirect(ctx context.Context, store Store, inputs []metafieldInput) (memqlsync.PropagateResult, error) {
	// metafieldsSet accepts 25 at a time.
	const batch = 25
	var lastID string
	for start := 0; start < len(inputs); start += batch {
		end := start + batch
		if end > len(inputs) {
			end = len(inputs)
		}
		resp, err := c.adminCall(ctx, store, metafieldsSetMutation, "ShopifyMetafieldsSet", map[string]any{
			"metafields": inputs[start:end],
		})
		if err != nil {
			return memqlsync.PropagateResult{}, classify(err)
		}
		if ueErr := userErrorsFrom(resp, "metafieldsSet"); ueErr != nil {
			return memqlsync.PropagateResult{}, memqlsync.Permanent(ueErr)
		}
		var decoded struct {
			MetafieldsSet struct {
				Metafields []struct {
					ID string `json:"id"`
				} `json:"metafields"`
			} `json:"metafieldsSet"`
		}
		if err := resp.DecodeInto(&decoded); err == nil && len(decoded.MetafieldsSet.Metafields) > 0 {
			lastID = decoded.MetafieldsSet.Metafields[len(decoded.MetafieldsSet.Metafields)-1].ID
		}
	}
	return memqlsync.PropagateResult{ExternalId: lastID}, nil
}

// propagateBulk stages a JSONL of metafieldsSet inputs and runs it as one
// bulk mutation.
func (c *Connector) propagateBulk(ctx context.Context, store Store, inputs []metafieldInput) (memqlsync.PropagateResult, error) {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, in := range inputs {
		if err := enc.Encode(map[string]any{"metafields": []metafieldInput{in}}); err != nil {
				return memqlsync.PropagateResult{}, memqlsync.Permanentf("could not encode the staged JSONL: %v", err)
		}
	}
	path, err := c.stageUpload(ctx, store, body.Bytes())
	if err != nil {
		return memqlsync.PropagateResult{}, classify(err)
	}
	resp, err := c.adminCall(ctx, store, bulkMutationRunMutation, "ShopifyBulkMutationRun", map[string]any{
		"mutation":         metafieldsSetMutation,
		"stagedUploadPath": path,
	})
	if err != nil {
		return memqlsync.PropagateResult{}, classify(err)
	}
	if ueErr := userErrorsFrom(resp, "bulkOperationRunMutation"); ueErr != nil {
		return memqlsync.PropagateResult{}, memqlsync.Permanent(ueErr)
	}
	var decoded struct {
		BulkOperationRunMutation struct {
			BulkOperation bulkOperation `json:"bulkOperation"`
		} `json:"bulkOperationRunMutation"`
	}
	_ = resp.DecodeInto(&decoded)
	return memqlsync.PropagateResult{ExternalId: decoded.BulkOperationRunMutation.BulkOperation.ID}, nil
}

// stageUpload asks Shopify for an upload target and PUTs the JSONL to it.
func (c *Connector) stageUpload(ctx context.Context, store Store, payload []byte) (string, error) {
	resp, err := c.adminCall(ctx, store, stagedUploadsCreateMutation, "ShopifyStagedUploadsCreate", map[string]any{
		"input": []map[string]any{{
			"resource":   "BULK_MUTATION_VARIABLES",
			"filename":   "memql-metafields.jsonl",
			"mimeType":   "text/jsonl",
			"httpMethod": "POST",
		}},
	})
	if err != nil {
		return "", err
	}
	if ueErr := userErrorsFrom(resp, "stagedUploadsCreate"); ueErr != nil {
		return "", ueErr
	}
	var decoded struct {
		StagedUploadsCreate struct {
			StagedTargets []struct {
				URL         string `json:"url"`
				ResourceURL string `json:"resourceUrl"`
				Parameters  []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"parameters"`
			} `json:"stagedTargets"`
		} `json:"stagedUploadsCreate"`
	}
	if err := resp.DecodeInto(&decoded); err != nil {
		return "", err
	}
	if len(decoded.StagedUploadsCreate.StagedTargets) == 0 {
		return "", fmt.Errorf("shopify: stagedUploadsCreate returned no target")
	}
	target := decoded.StagedUploadsCreate.StagedTargets[0]

	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	var key string
	for _, p := range target.Parameters {
		if p.Name == "key" {
			key = p.Value
		}
		if err := writer.WriteField(p.Name, p.Value); err != nil {
			return "", err
		}
	}
	part, err := writer.CreateFormFile("file", "memql-metafields.jsonl")
	if err != nil {
		return "", err
	}
	if _, err := part.Write(payload); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, &form)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	uploadResp, err := c.admin.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("shopify: staged upload: %w", err)
	}
	defer uploadResp.Body.Close()
	if uploadResp.StatusCode < 200 || uploadResp.StatusCode >= 300 {
		return "", fmt.Errorf("shopify: staged upload returned HTTP %d", uploadResp.StatusCode)
	}
	if key == "" {
		return "", fmt.Errorf("shopify: staged upload target carried no key parameter")
	}
	return key, nil
}

// classify turns a transport error into what the drain should do.
//
// The distinction is the whole point: a throttle, a 429 and a 5xx will
// succeed later, while a validation error, a missing scope and a 404 will
// fail identically forever. Retrying the second class is how a queue
// stops draining, with every individual attempt looking transient.
//
// An UNCLASSIFIED error retries. A network failure is the common case and
// dead-lettering it would lose a legitimate change to a blip.
func classify(err error) error {
	if ae, ok := err.(*AdminError); ok {
		if ae.Retryable() {
			return ae
		}
		return memqlsync.Permanent(ae)
	}
	if ue, ok := err.(*UserError); ok {
		return memqlsync.Permanent(ue)
	}
	return err
}

// CreateDraftOrderFromQuote is the quote-acceptance push (spec 7.3).
//
// The one place the connector writes something a customer will SEE, and it is
// still not a change to the store's own data: a draft order is an offer,
// nothing is charged, and the merchant can delete it. Prices are locked from
// the quote rather than recalculated, which is the whole point of quoting.
func (c *Connector) CreateDraftOrderFromQuote(ctx context.Context, store Store, quote QuoteInput) (string, error) {
	input := map[string]any{
		"lineItems": quote.LineItems(),
		"purchasingEntity": map[string]any{
			"purchasingCompany": map[string]any{
				"companyId":         quote.CompanyGID,
				"companyLocationId": quote.CompanyLocationGID,
				"companyContactId":  quote.CompanyContactGID,
			},
		},
	}
	if quote.PONumber != "" {
		input["poNumber"] = quote.PONumber
	}
	if quote.PaymentTermsTemplateGID != "" {
		input["paymentTerms"] = map[string]any{
			"paymentTermsTemplateId": quote.PaymentTermsTemplateGID,
		}
	}
	if quote.Note != "" {
		input["note"] = quote.Note
	}
	resp, err := c.adminCall(ctx, store, draftOrderCreateMutation, "ShopifyDraftOrderCreate", map[string]any{"input": input})
	if err != nil {
		return "", err
	}
	if ueErr := userErrorsFrom(resp, "draftOrderCreate"); ueErr != nil {
		return "", ueErr
	}
	var decoded struct {
		DraftOrderCreate struct {
			DraftOrder struct {
				ID string `json:"id"`
			} `json:"draftOrder"`
		} `json:"draftOrderCreate"`
	}
	if err := resp.DecodeInto(&decoded); err != nil {
		return "", err
	}
	if decoded.DraftOrderCreate.DraftOrder.ID == "" {
		return "", fmt.Errorf("shopify: draftOrderCreate returned no draft order")
	}
	return decoded.DraftOrderCreate.DraftOrder.ID, nil
}

const draftOrderCreateMutation = `mutation ShopifyDraftOrderCreate($input: DraftOrderInput!) {
  draftOrderCreate(input: $input) {
    draftOrder { id name totalPriceSet { shopMoney { amount currencyCode } } }
    userErrors { field message }
  }
}`

// QuoteInput is the accepted quote, as the draft-order call needs it.
type QuoteInput struct {
	CompanyGID              string
	CompanyLocationGID      string
	CompanyContactGID       string
	PONumber                string
	PaymentTermsTemplateGID string
	Note                    string
	CurrencyCode            string
	Lines                   []QuoteLine
}

// QuoteLine is one priced line.
type QuoteLine struct {
	VariantGID string
	Quantity   int
	UnitAmount string
}

// LineItems renders the lines with their LOCKED prices.
//
// priceOverride is what makes a quote a quote: without it Shopify recalculates
// from the catalog and the buyer is charged today's price for something they
// accepted at last month's.
func (q QuoteInput) LineItems() []map[string]any {
	out := make([]map[string]any, 0, len(q.Lines))
	for _, line := range q.Lines {
		item := map[string]any{
			"variantId": line.VariantGID,
			"quantity":  line.Quantity,
		}
		if line.UnitAmount != "" {
			item["priceOverride"] = map[string]any{
				"amount":       line.UnitAmount,
				"currencyCode": q.CurrencyCode,
			}
		}
		out = append(out, item)
	}
	return out
}

var _ = time.Second
