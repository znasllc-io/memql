package shopify

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// wholesale_calls.go -- the individual Admin calls wholesale.go composes.
//
// SPLIT FROM THE FLOW ON PURPOSE. wholesale.go is the argument -- what is
// created, in what order, what is never deleted, where the ceiling is
// checked. This file is the mechanics: one document and one decoder per
// call, each doing exactly one thing, so the flow above reads as the
// sequence it is rather than as three hundred lines of GraphQL.
//
// Every call here follows CreateDraftOrderFromQuote's shape, which is the
// package's established one for an Admin WRITE as opposed to a metafield
// projection: adminCall, then userErrorsFrom, then DecodeInto, then a
// refusal naming the object when the response carried none. The last step
// matters -- Shopify answers a 200 with a null payload and no userErrors
// often enough that treating "no error" as "it worked" would record a
// reference to an object that does not exist.

// ---------------------------------------------------------------------------
// Customers
// ---------------------------------------------------------------------------

// customerByEmail finds a customer, or returns "" when there is none.
//
// "" IS NOT AN ERROR. Both callers want to know whether the customer
// exists, and they want opposite things when the answer is no: the B2B path
// creates one, the tag path refuses. Encoding "not found" as an error would
// make both of them parse it back out.
func (c *Connector) customerByEmail(ctx context.Context, store Store, email string) (string, error) {
	clean := strings.TrimSpace(email)
	if clean == "" {
		return "", fmt.Errorf("shopify: a buyer email is required to find a customer")
	}
	resp, err := c.adminCall(ctx, store, customerSearchQuery, "ShopifyCustomerByEmail",
		// QUOTED AS A LITERAL rather than interpolated into the query
		// string: an email is caller-supplied text and Shopify's search
		// syntax has its own operators, so an address containing one would
		// otherwise change what is being asked.
		map[string]any{"query": "email:" + strconv.Quote(clean)})
	if err != nil {
		return "", err
	}
	var decoded struct {
		Customers struct {
			Nodes []struct {
				ID    string `json:"id"`
				Email string `json:"email"`
			} `json:"nodes"`
		} `json:"customers"`
	}
	if err := resp.DecodeInto(&decoded); err != nil {
		return "", err
	}
	for _, node := range decoded.Customers.Nodes {
		// EXACT MATCH REQUIRED. Shopify's email: search is a prefix and
		// token search, so "email:\"sam@example.com\"" can answer with
		// sam@example.com.au. Entitling the wrong customer is the one
		// mistake here nobody would notice until a stranger got trade
		// prices.
		if strings.EqualFold(strings.TrimSpace(node.Email), clean) {
			return node.ID, nil
		}
	}
	return "", nil
}

const customerSearchQuery = `query ShopifyCustomerByEmail($query: String!) {
  customers(first: 10, query: $query) {
    nodes { id email }
  }
}`

// createCustomer makes the account the B2B contact will be.
func (c *Connector) createCustomer(ctx context.Context, store Store, name, email string) (string, error) {
	first, last := splitName(name)
	input := map[string]any{"email": strings.TrimSpace(email)}
	if first != "" {
		input["firstName"] = first
	}
	if last != "" {
		input["lastName"] = last
	}
	resp, err := c.adminCall(ctx, store, customerCreateMutation, "ShopifyCustomerCreate",
		map[string]any{"input": input})
	if err != nil {
		return "", err
	}
	if ueErr := userErrorsFrom(resp, "customerCreate"); ueErr != nil {
		return "", ueErr
	}
	var decoded struct {
		CustomerCreate struct {
			Customer struct {
				ID string `json:"id"`
			} `json:"customer"`
		} `json:"customerCreate"`
	}
	if err := resp.DecodeInto(&decoded); err != nil {
		return "", err
	}
	if decoded.CustomerCreate.Customer.ID == "" {
		return "", fmt.Errorf("shopify: customerCreate returned no customer")
	}
	return decoded.CustomerCreate.Customer.ID, nil
}

const customerCreateMutation = `mutation ShopifyCustomerCreate($input: CustomerInput!) {
  customerCreate(input: $input) {
    customer { id email }
    userErrors { field message }
  }
}`

// splitName divides a submitted name into the two fields Shopify wants.
//
// ON THE LAST SPACE, and everything before it is the first name. It is
// wrong for plenty of names and there is no correct answer available: the
// pack collects ONE name field because that is the minimum an application
// needs, and inventing a second on the form to satisfy Shopify's schema
// would be the pack drifting toward one integration's shape. A name with no
// space becomes the first name and the last stays empty, which Shopify
// accepts.
func splitName(name string) (first, last string) {
	clean := strings.Join(strings.Fields(name), " ")
	if clean == "" {
		return "", ""
	}
	idx := strings.LastIndex(clean, " ")
	if idx < 0 {
		return clean, ""
	}
	return clean[:idx], clean[idx+1:]
}

// ---------------------------------------------------------------------------
// Companies
// ---------------------------------------------------------------------------

// companyByExternalID finds a company a previous run created.
//
// THIS IS WHAT MAKES PROVISION IDEMPOTENT. The pack retries after a
// failure, and without this a retry would make a second company for one
// application -- a mess a merchant has to clean up by hand, and one they
// would only find when a buyer appeared twice in their B2B list.
func (c *Connector) companyByExternalID(ctx context.Context, store Store,
	externalID string) (company, location string, err error) {
	resp, err := c.adminCall(ctx, store, companySearchQuery, "ShopifyCompanyByExternalID",
		map[string]any{"query": "external_id:" + strconv.Quote(externalID)})
	if err != nil {
		return "", "", err
	}
	var decoded struct {
		Companies struct {
			Nodes []struct {
				ID         string `json:"id"`
				ExternalID string `json:"externalId"`
				Locations  struct {
					Nodes []struct {
						ID string `json:"id"`
					} `json:"nodes"`
				} `json:"locations"`
			} `json:"nodes"`
		} `json:"companies"`
	}
	if err := resp.DecodeInto(&decoded); err != nil {
		return "", "", err
	}
	for _, node := range decoded.Companies.Nodes {
		// EXACT MATCH, for customerByEmail's reason: a prefix search that
		// answered with a different application's company would attach this
		// buyer to somebody else's trade account.
		if strings.TrimSpace(node.ExternalID) != externalID {
			continue
		}
		if len(node.Locations.Nodes) == 0 {
			// A company with no location cannot carry a catalog, and this
			// package does not repair one it did not finish: the refusal
			// names it so an operator can delete the half-made company and
			// provision again.
			return "", "", fmt.Errorf("shopify: company %s exists for this application but has no location; delete it in the Shopify admin and provision again", node.ID)
		}
		return node.ID, node.Locations.Nodes[0].ID, nil
	}
	return "", "", nil
}

const companySearchQuery = `query ShopifyCompanyByExternalID($query: String!) {
  companies(first: 10, query: $query) {
    nodes {
      id
      externalId
      locations(first: 1) { nodes { id } }
    }
  }
}`

// createCompany makes the company and its first location in one call.
//
// ONE CALL RATHER THAN TWO, because companyCreate takes the location
// inline: a company created without one and then given one is two chances
// to fail, and the failure between them leaves exactly the half-made
// company companyByExternalID has to refuse above.
func (c *Connector) createCompany(ctx context.Context, store Store,
	in WholesaleAccountInput, externalID string) (company, location string, err error) {
	input := map[string]any{
		"company": map[string]any{
			"name":       strings.TrimSpace(in.CompanyName),
			"externalId": externalID,
			"note":       "Created by MemQL from wholesale application " + in.ApplicationID,
		},
		"companyLocation": map[string]any{
			"name": strings.TrimSpace(in.CompanyName),
			// BUYER EXPERIENCE, NOT PAYMENT TERMS. The location is created
			// so the catalog has something to attach to; what a buyer may
			// pay with is the merchant's own decision and this connector
			// does not make it for them.
			"buyerExperienceConfiguration": map[string]any{
				"checkoutToDraft": false,
			},
		},
	}
	resp, err := c.adminCall(ctx, store, companyCreateMutation, "ShopifyCompanyCreate",
		map[string]any{"input": input})
	if err != nil {
		return "", "", err
	}
	if ueErr := userErrorsFrom(resp, "companyCreate"); ueErr != nil {
		return "", "", ueErr
	}
	var decoded struct {
		CompanyCreate struct {
			Company struct {
				ID        string `json:"id"`
				Locations struct {
					Nodes []struct {
						ID string `json:"id"`
					} `json:"nodes"`
				} `json:"locations"`
			} `json:"company"`
		} `json:"companyCreate"`
	}
	if err := resp.DecodeInto(&decoded); err != nil {
		return "", "", err
	}
	if decoded.CompanyCreate.Company.ID == "" {
		return "", "", fmt.Errorf("shopify: companyCreate returned no company")
	}
	if len(decoded.CompanyCreate.Company.Locations.Nodes) == 0 {
		return "", "", fmt.Errorf("shopify: companyCreate returned company %s with no location, so there is nothing to attach a catalog to", decoded.CompanyCreate.Company.ID)
	}
	return decoded.CompanyCreate.Company.ID,
		decoded.CompanyCreate.Company.Locations.Nodes[0].ID, nil
}

const companyCreateMutation = `mutation ShopifyCompanyCreate($input: CompanyCreateInput!) {
  companyCreate(input: $input) {
    company {
      id
      name
      externalId
      locations(first: 1) { nodes { id } }
    }
    userErrors { field message code }
  }
}`

// assignCustomerAsContact makes the buyer a contact of the company.
func (c *Connector) assignCustomerAsContact(ctx context.Context, store Store,
	companyGID, customerGID string) (string, error) {
	resp, err := c.adminCall(ctx, store, companyAssignContactMutation, "ShopifyCompanyAssignContact",
		map[string]any{"companyId": companyGID, "customerId": customerGID})
	if err != nil {
		return "", err
	}
	if ueErr := userErrorsFrom(resp, "companyAssignCustomerAsContact"); ueErr != nil {
		return "", ueErr
	}
	var decoded struct {
		Assign struct {
			CompanyContact struct {
				ID string `json:"id"`
			} `json:"companyContact"`
		} `json:"companyAssignCustomerAsContact"`
	}
	if err := resp.DecodeInto(&decoded); err != nil {
		return "", err
	}
	if decoded.Assign.CompanyContact.ID == "" {
		return "", fmt.Errorf("shopify: companyAssignCustomerAsContact returned no contact")
	}
	return decoded.Assign.CompanyContact.ID, nil
}

const companyAssignContactMutation = `mutation ShopifyCompanyAssignContact($companyId: ID!, $customerId: ID!) {
  companyAssignCustomerAsContact(companyId: $companyId, customerId: $customerId) {
    companyContact { id }
    userErrors { field message code }
  }
}`

// ---------------------------------------------------------------------------
// Catalog and price list
// ---------------------------------------------------------------------------

// createCatalog makes the company-location catalog, ACTIVE.
func (c *Connector) createCatalog(ctx context.Context, store Store,
	title, companyLocationGID string) (string, error) {
	input := map[string]any{
		"title":  title,
		"status": "ACTIVE",
		"context": map[string]any{
			"companyLocationIds": []string{companyLocationGID},
		},
	}
	resp, err := c.adminCall(ctx, store, catalogCreateMutation, "ShopifyCatalogCreate",
		map[string]any{"input": input})
	if err != nil {
		return "", err
	}
	if ueErr := userErrorsFrom(resp, "catalogCreate"); ueErr != nil {
		return "", ueErr
	}
	var decoded struct {
		CatalogCreate struct {
			Catalog struct {
				ID string `json:"id"`
			} `json:"catalog"`
		} `json:"catalogCreate"`
	}
	if err := resp.DecodeInto(&decoded); err != nil {
		return "", err
	}
	if decoded.CatalogCreate.Catalog.ID == "" {
		return "", fmt.Errorf("shopify: catalogCreate returned no catalog")
	}
	return decoded.CatalogCreate.Catalog.ID, nil
}

const catalogCreateMutation = `mutation ShopifyCatalogCreate($input: CatalogCreateInput!) {
  catalogCreate(input: $input) {
    catalog { id title status }
    userErrors { field message code }
  }
}`

const catalogDraftMutation = `mutation ShopifyCatalogDraft($id: ID!, $input: CatalogUpdateInput!) {
  catalogUpdate(id: $id, input: $input) {
    catalog { id status }
    userErrors { field message code }
  }
}`

// createPriceList attaches the buyer's prices to the catalog.
//
// A PERCENTAGE OFF THE PUBLISHED PRICE, not a per-variant price book. The
// pack collects no prices -- it has no idea what a merchant sells -- so the
// only entitlement it can express generically is an adjustment, and the
// merchant sets real per-variant prices afterwards in their own admin if
// they want them. A zero adjustment creates the price list anyway, which is
// what gives them somewhere to put those prices.
func (c *Connector) createPriceList(ctx context.Context, store Store,
	title, catalogGID, currency string, percentOff int) (string, error) {
	cur := strings.ToUpper(strings.TrimSpace(currency))
	if cur == "" {
		// The store's own currency is the right default and this connector
		// does not guess a different one: a price list in a currency the
		// catalog's market does not use prices nothing.
		cur = "USD"
	}
	if percentOff < 0 || percentOff > 100 {
		return "", fmt.Errorf("shopify: a wholesale adjustment of %d%% is not a percentage off the published price", percentOff)
	}
	input := map[string]any{
		"name":      title,
		"currency":  cur,
		"catalogId": catalogGID,
		"parent": map[string]any{
			"adjustment": map[string]any{
				"type":  "PERCENTAGE_DECREASE",
				"value": percentOff,
			},
		},
	}
	resp, err := c.adminCall(ctx, store, priceListCreateMutation, "ShopifyPriceListCreate",
		map[string]any{"input": input})
	if err != nil {
		return "", err
	}
	if ueErr := userErrorsFrom(resp, "priceListCreate"); ueErr != nil {
		return "", ueErr
	}
	var decoded struct {
		PriceListCreate struct {
			PriceList struct {
				ID string `json:"id"`
			} `json:"priceList"`
		} `json:"priceListCreate"`
	}
	if err := resp.DecodeInto(&decoded); err != nil {
		return "", err
	}
	if decoded.PriceListCreate.PriceList.ID == "" {
		return "", fmt.Errorf("shopify: priceListCreate returned no price list")
	}
	return decoded.PriceListCreate.PriceList.ID, nil
}

const priceListCreateMutation = `mutation ShopifyPriceListCreate($input: PriceListCreateInput!) {
  priceListCreate(input: $input) {
    priceList { id name currency }
    userErrors { field message code }
  }
}`

// ---------------------------------------------------------------------------
// Tags
// ---------------------------------------------------------------------------

const tagsAddMutation = `mutation ShopifyTagsAdd($id: ID!, $tags: [String!]!) {
  tagsAdd(id: $id, tags: $tags) {
    node { id }
    userErrors { field message }
  }
}`

const tagsRemoveMutation = `mutation ShopifyTagsRemove($id: ID!, $tags: [String!]!) {
  tagsRemove(id: $id, tags: $tags) {
    node { id }
    userErrors { field message }
  }
}`
