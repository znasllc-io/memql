package email

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const azureCommunicationManagementVersion = "2026-03-18"
const azureManagementScope = "https://management.azure.com/user_impersonation"

var azureUUID = regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)
var azureResourceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)

type azureProtocol struct {
	clientID string
	client   *http.Client
}

func newAzureProtocol(clientID string) *azureProtocol {
	return &azureProtocol{clientID: clientID, client: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type azureDeviceGrant struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

type azureTokenReply struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
}

// API response bodies can contain message bodies, keys, and tokens. Only
// bounded status information crosses the provider error boundary.
type azureAPIError struct{ Status int }

func (e *azureAPIError) Error() string {
	switch e.Status {
	case 401:
		return "Your Microsoft session expired. Connect again."
	case 403:
		return "Microsoft did not grant access to this resource. Choose a subscription you can manage or ask its administrator."
	case 404:
		return "Microsoft could not find the selected resource. Refresh the choices."
	case 409:
		return "Microsoft is still changing this resource or its name is already in use. Refresh its status."
	case 429:
		return "Microsoft is limiting requests. Wait briefly before trying again."
	default:
		return fmt.Sprintf("Microsoft could not complete this request (HTTP %d).", e.Status)
	}
}

func (a *azureProtocol) request(ctx context.Context, method, target, contentType, token string, body []byte, dest any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("could not create Microsoft request")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := a.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("Microsoft could not be reached; refresh status before repeating a setup change")
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return res.StatusCode, fmt.Errorf("Microsoft returned an unreadable response")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return res.StatusCode, &azureAPIError{Status: res.StatusCode}
	}
	if dest != nil && len(data) > 0 {
		if err = json.Unmarshal(data, dest); err != nil {
			return res.StatusCode, fmt.Errorf("Microsoft returned an invalid response")
		}
	}
	return res.StatusCode, nil
}

func (a *azureProtocol) begin(ctx context.Context, tenant string) (azureDeviceGrant, error) {
	var grant azureDeviceGrant
	if !azureUUID.MatchString(a.clientID) {
		return grant, fmt.Errorf("the MemQL Azure application has not been registered for this installation")
	}
	if tenant != "organizations" && !azureUUID.MatchString(tenant) {
		return grant, fmt.Errorf("choose a valid Microsoft directory")
	}
	form := url.Values{"client_id": {a.clientID}, "scope": {azureManagementScope + " offline_access"}}
	_, err := a.request(ctx, http.MethodPost, "https://login.microsoftonline.com/"+tenant+"/oauth2/v2.0/devicecode", "application/x-www-form-urlencoded", "", []byte(form.Encode()), &grant)
	if err != nil {
		return grant, err
	}
	link, err := url.Parse(grant.VerificationURI)
	if err != nil || link.Scheme != "https" || link.User != nil || link.Port() != "" || (link.Host != "microsoft.com" && link.Host != "www.microsoft.com" && link.Host != "login.microsoftonline.com") || grant.DeviceCode == "" || grant.UserCode == "" || grant.ExpiresIn <= 0 || grant.ExpiresIn > 1800 || grant.Interval < 1 || grant.Interval > 60 {
		return azureDeviceGrant{}, fmt.Errorf("Microsoft returned an invalid sign-in challenge")
	}
	return grant, nil
}

func (a *azureProtocol) token(ctx context.Context, tenant string, form url.Values) (azureTokenReply, error) {
	var reply azureTokenReply
	if !azureUUID.MatchString(a.clientID) || tenant != "organizations" && !azureUUID.MatchString(tenant) {
		return reply, fmt.Errorf("invalid Microsoft sign-in configuration")
	}
	form.Set("client_id", a.clientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://login.microsoftonline.com/"+tenant+"/oauth2/v2.0/token", strings.NewReader(form.Encode()))
	if err != nil {
		return reply, fmt.Errorf("could not create Microsoft sign-in request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := a.client.Do(req)
	if err != nil {
		return reply, fmt.Errorf("Microsoft sign-in could not be reached")
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, (128<<10)+1))
	if err != nil || len(data) > 128<<10 || json.Unmarshal(data, &reply) != nil {
		return azureTokenReply{}, fmt.Errorf("Microsoft returned an invalid sign-in response")
	}
	if res.StatusCode == 400 {
		switch reply.Error {
		case "authorization_pending", "slow_down", "authorization_declined", "expired_token", "access_denied", "invalid_grant":
			return azureTokenReply{Error: reply.Error}, nil
		}
	}
	if res.StatusCode != 200 {
		return azureTokenReply{}, &azureAPIError{Status: res.StatusCode}
	}
	if reply.TokenType != "Bearer" || reply.AccessToken == "" || reply.ExpiresIn <= 0 || reply.ExpiresIn > 86400 {
		return azureTokenReply{}, fmt.Errorf("Microsoft did not return a usable access token")
	}
	return reply, nil
}

// All resource paths are built by the setup workflow after validating each
// segment. A returned nextLink is the only absolute URL we accept, and it must
// remain on ARM. Bearer credentials can never be redirected to another host.
func (a *azureProtocol) arm(ctx context.Context, method, path, token string, payload, dest any) error {
	u, err := url.Parse(path)
	if err != nil {
		return fmt.Errorf("invalid Microsoft resource path")
	}
	if u.IsAbs() {
		if u.Scheme != "https" || u.Host != "management.azure.com" || u.User != nil || u.Fragment != "" {
			return fmt.Errorf("Microsoft resource link left Azure Resource Manager")
		}
	} else {
		if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || u.Host != "" || u.Fragment != "" {
			return fmt.Errorf("invalid Microsoft resource path")
		}
		u.Scheme = "https"
		u.Host = "management.azure.com"
	}
	if !strings.HasPrefix(u.Path, "/subscriptions") && !strings.HasPrefix(u.Path, "/tenants") {
		return fmt.Errorf("unsupported Microsoft resource path")
	}
	var body []byte
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("invalid Microsoft resource settings")
		}
	}
	_, err = a.request(ctx, method, u.String(), "application/json", token, body, dest)
	return err
}

func (a *azureProtocol) list(ctx context.Context, path, token string) ([]map[string]any, error) {
	var all []map[string]any
	for page := 0; path != ""; page++ {
		if page >= 20 {
			return nil, fmt.Errorf("Microsoft returned too many resources; narrow the selection")
		}
		var result struct {
			Value    []map[string]any `json:"value"`
			NextLink string           `json:"nextLink"`
		}
		if err := a.arm(ctx, http.MethodGet, path, token, nil, &result); err != nil {
			return nil, err
		}
		all = append(all, result.Value...)
		if len(all) > 2000 {
			return nil, fmt.Errorf("Microsoft returned too many resources; narrow the selection")
		}
		path = result.NextLink
	}
	return all, nil
}
