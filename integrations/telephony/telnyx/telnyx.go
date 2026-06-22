// Package telnyx implements the telephony.CarrierProvider over the Telnyx v2
// REST API: DID search / purchase / release / inbound-routing. Telnyx is the
// first carrier — chosen for a clean programmatic number API and a low
// per-minute trunk rate. Because everything sits behind CarrierProvider,
// swapping carriers is a new package, not a caller change.
//
// Secrets (the API key) are resolved by the caller and injected; this package
// never reads the environment directly.
package telnyx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/integrations/telephony"
)

// defaultBaseURL is the Telnyx v2 REST root.
const defaultBaseURL = "https://api.telnyx.com/v2"

// Client is a Telnyx CarrierProvider. Construct with New.
type Client struct {
	apiKey string
	// connectionID is the Telnyx Connection (an FQDN/credential SIP
	// connection) that points at our livekit/sip edge. Inbound DIDs are
	// assigned to it so PSTN calls route to the edge. Provisioned once
	// (deploy concern); supplied here so ConfigureInbound can assign numbers.
	connectionID string
	baseURL      string
	http         *http.Client
}

// Options configures a Telnyx Client.
type Options struct {
	// APIKey is the Telnyx v2 API key (required).
	APIKey string
	// ConnectionID is the Telnyx connection id fronting the SIP edge.
	// Optional at construction; required for ConfigureInbound.
	ConnectionID string
	// BaseURL overrides the API root (tests). Defaults to the live API.
	BaseURL string
	// HTTPClient overrides the transport (tests). Defaults to a 30s client.
	HTTPClient *http.Client
}

// New builds a Telnyx Client. It errors when the API key is missing.
func New(opts Options) (*Client, error) {
	if strings.TrimSpace(opts.APIKey) == "" {
		return nil, fmt.Errorf("telnyx: API key is required (set TELNYX_API_KEY)")
	}
	base := opts.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		apiKey:       opts.APIKey,
		connectionID: opts.ConnectionID,
		baseURL:      strings.TrimRight(base, "/"),
		http:         hc,
	}, nil
}

// Name implements telephony.CarrierProvider.
func (c *Client) Name() string { return "telnyx" }

// ---- search ---------------------------------------------------------------

// availableNumbersResponse models GET /available_phone_numbers.
type availableNumbersResponse struct {
	Data []struct {
		PhoneNumber string `json:"phone_number"`
		CostInfo    struct {
			MonthlyCost string `json:"monthly_cost"`
		} `json:"cost_information"`
		Features []struct {
			Name string `json:"name"`
		} `json:"features"`
		RegionInfo []struct {
			RegionType string `json:"region_type"`
			RegionName string `json:"region_name"`
		} `json:"region_information"`
	} `json:"data"`
}

func phoneNumberTypeForQuery(t telephony.NumberType) string {
	switch t {
	case telephony.NumberTypeTollFree:
		return "toll_free"
	case telephony.NumberTypeMobile:
		return "mobile"
	default:
		return "local"
	}
}

// SearchNumbers implements telephony.CarrierProvider.
func (c *Client) SearchNumbers(ctx context.Context, q telephony.NumberQuery) ([]telephony.Number, error) {
	country := q.CountryCode
	if country == "" {
		country = "US"
	}
	vals := url.Values{}
	vals.Set("filter[country_code]", country)
	vals.Set("filter[phone_number_type]", phoneNumberTypeForQuery(q.Type))
	vals.Set("filter[features][]", "voice")
	if q.AreaCode != "" {
		vals.Set("filter[national_destination_code]", q.AreaCode)
	}
	if q.Contains != "" {
		vals.Set("filter[phone_number][contains]", q.Contains)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	vals.Set("filter[limit]", strconv.Itoa(limit))

	var out availableNumbersResponse
	if err := c.do(ctx, http.MethodGet, "/available_phone_numbers?"+vals.Encode(), nil, &out); err != nil {
		return nil, err
	}
	nums := make([]telephony.Number, 0, len(out.Data))
	for _, d := range out.Data {
		region := ""
		if len(d.RegionInfo) > 0 {
			region = d.RegionInfo[len(d.RegionInfo)-1].RegionName
		}
		caps := make([]string, 0, len(d.Features))
		for _, f := range d.Features {
			caps = append(caps, f.Name)
		}
		monthly, _ := strconv.ParseFloat(d.CostInfo.MonthlyCost, 64)
		nums = append(nums, telephony.Number{
			ProviderID:   d.PhoneNumber, // Telnyx purchases by E.164
			E164:         d.PhoneNumber,
			Carrier:      "telnyx",
			Type:         orDefault(q.Type, telephony.NumberTypeLocal),
			Region:       region,
			MonthlyCost:  monthly,
			Capabilities: caps,
		})
	}
	return nums, nil
}

// ---- purchase -------------------------------------------------------------

type numberOrderRequest struct {
	PhoneNumbers []phoneNumberRef `json:"phone_numbers"`
	ConnectionID string           `json:"connection_id,omitempty"`
}

type phoneNumberRef struct {
	PhoneNumber string `json:"phone_number"`
}

type numberOrderResponse struct {
	Data struct {
		ID           string `json:"id"`
		Status       string `json:"status"`
		PhoneNumbers []struct {
			PhoneNumber string `json:"phone_number"`
		} `json:"phone_numbers"`
	} `json:"data"`
}

// BuyNumber implements telephony.CarrierProvider. providerID is the E.164
// returned by SearchNumbers.
func (c *Client) BuyNumber(ctx context.Context, providerID string) (telephony.Number, error) {
	body := numberOrderRequest{PhoneNumbers: []phoneNumberRef{{PhoneNumber: providerID}}}
	if c.connectionID != "" {
		body.ConnectionID = c.connectionID
	}
	var out numberOrderResponse
	if err := c.do(ctx, http.MethodPost, "/number_orders", body, &out); err != nil {
		return telephony.Number{}, err
	}
	e164 := providerID
	if len(out.Data.PhoneNumbers) > 0 && out.Data.PhoneNumbers[0].PhoneNumber != "" {
		e164 = out.Data.PhoneNumbers[0].PhoneNumber
	}
	return telephony.Number{
		ProviderID: e164,
		E164:       e164,
		Carrier:    "telnyx",
		Type:       telephony.NumberTypeLocal,
	}, nil
}

// ---- ownership lookup -----------------------------------------------------

type ownedNumbersResponse struct {
	Data []struct {
		ID          string `json:"id"`
		PhoneNumber string `json:"phone_number"`
		Status      string `json:"status"`
		PhoneNumberType string `json:"phone_number_type"`
	} `json:"data"`
}

// numberID resolves an owned DID's Telnyx id from its E.164 address.
func (c *Client) numberID(ctx context.Context, e164 string) (string, error) {
	vals := url.Values{}
	vals.Set("filter[phone_number]", e164)
	var out ownedNumbersResponse
	if err := c.do(ctx, http.MethodGet, "/phone_numbers?"+vals.Encode(), nil, &out); err != nil {
		return "", err
	}
	for _, d := range out.Data {
		if d.PhoneNumber == e164 {
			return d.ID, nil
		}
	}
	return "", fmt.Errorf("telnyx: owned number %q not found", e164)
}

// ListNumbers implements telephony.CarrierProvider.
func (c *Client) ListNumbers(ctx context.Context) ([]telephony.Number, error) {
	var out ownedNumbersResponse
	if err := c.do(ctx, http.MethodGet, "/phone_numbers", nil, &out); err != nil {
		return nil, err
	}
	nums := make([]telephony.Number, 0, len(out.Data))
	for _, d := range out.Data {
		nums = append(nums, telephony.Number{
			ProviderID: d.ID,
			E164:       d.PhoneNumber,
			Carrier:    "telnyx",
			Type:       numberTypeFromTelnyx(d.PhoneNumberType),
		})
	}
	return nums, nil
}

// ---- release --------------------------------------------------------------

// ReleaseNumber implements telephony.CarrierProvider.
func (c *Client) ReleaseNumber(ctx context.Context, e164 string) error {
	id, err := c.numberID(ctx, e164)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, "/phone_numbers/"+url.PathEscape(id), nil, nil)
}

// ---- inbound routing ------------------------------------------------------

type updateNumberRequest struct {
	ConnectionID string `json:"connection_id"`
}

// ConfigureInbound assigns the DID to the SIP-edge-fronting Telnyx connection
// so inbound calls route to livekit/sip. The carrier's routing target is the
// connection; sipEdgeURI documents/validates the intended edge (the FQDN
// connection itself is provisioned once at deploy time and named by
// ConnectionID).
func (c *Client) ConfigureInbound(ctx context.Context, e164, sipEdgeURI string) error {
	if c.connectionID == "" {
		return fmt.Errorf("telnyx: ConfigureInbound requires a connection id (set TELNYX_CONNECTION_ID for the SIP edge %q)", sipEdgeURI)
	}
	id, err := c.numberID(ctx, e164)
	if err != nil {
		return err
	}
	body := updateNumberRequest{ConnectionID: c.connectionID}
	return c.do(ctx, http.MethodPatch, "/phone_numbers/"+url.PathEscape(id), body, nil)
}

// ---- transport ------------------------------------------------------------

// telnyxError models the v2 error envelope.
type telnyxError struct {
	Errors []struct {
		Code   string `json:"code"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
	} `json:"errors"`
}

// do issues a request, sets auth, and decodes the JSON response into out
// (nil out discards the body). Non-2xx responses become typed errors carrying
// the Telnyx error detail.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("telnyx: marshal request: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("telnyx: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("telnyx: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var te telnyxError
		if json.Unmarshal(raw, &te) == nil && len(te.Errors) > 0 {
			return fmt.Errorf("telnyx: %s %s -> %d: %s (%s)", method, path, resp.StatusCode, te.Errors[0].Title, te.Errors[0].Detail)
		}
		return fmt.Errorf("telnyx: %s %s -> %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("telnyx: decode response: %w", err)
		}
	}
	return nil
}

// ---- helpers --------------------------------------------------------------

func orDefault(t, def telephony.NumberType) telephony.NumberType {
	if t == "" {
		return def
	}
	return t
}

func numberTypeFromTelnyx(s string) telephony.NumberType {
	switch s {
	case "toll_free":
		return telephony.NumberTypeTollFree
	case "mobile":
		return telephony.NumberTypeMobile
	default:
		return telephony.NumberTypeLocal
	}
}

// compile-time assertion that Client satisfies the interface.
var _ telephony.CarrierProvider = (*Client)(nil)
