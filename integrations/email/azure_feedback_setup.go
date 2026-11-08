package email

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
	"github.com/znasllc-io/memql/core/env"
)

const azureEventGridVersion = "2025-02-15"

type azureFeedbackConnection struct {
	AccountID     string    `json:"accountId"`
	PlanID        string    `json:"planId"`
	Topic         string    `json:"topic"`
	Subscription  string    `json:"subscription"`
	Endpoint      string    `json:"endpoint"`
	Secret        string    `json:"secret"`
	Status        string    `json:"status"`
	Applied       bool      `json:"applied"`
	NextAttemptAt time.Time `json:"nextAttemptAt"`
}

func azureFeedbackURL() (string, error) {
	domain, _ := env.NewEnvReader("").String(DomainEnv)
	domain = strings.TrimSpace(domain)
	endpoint, err := url.Parse("https://api." + domain + AzureFeedbackPath)
	if err != nil || domain == "" || endpoint.User != nil || endpoint.Port() != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != AzureFeedbackPath || endpoint.Hostname() != "api."+domain {
		return "", fmt.Errorf("the instance needs a valid public domain for Azure email feedback")
	}
	return endpoint.String(), nil
}

// This is one bounded provider resource operation. Its receiver credential is
// durable before ARM asks the live endpoint to validate the subscription. No
// organization lock is held during that handshake. Repeated calls reuse the
// same credential and named subscription; they never create another resource.
func (a *azureSetup) ensureDeliveryFeedback(ctx context.Context, account, planID string) (map[string]any, error) {
	var connection azureConnection
	found, _, err := a.store.read(ctx, "organization:"+account, &connection)
	if err != nil {
		return nil, err
	}
	if !found || connection.Status != "ready" || connection.AccountID != account {

		return nil, fmt.Errorf("finish this organization's email sender before enabling delivery feedback")
	}
	actualPlan := azurePlanID(connection)
	if planID != "" && planID != actualPlan {
		return nil, fmt.Errorf("the email resource plan changed; reopen the connection")
	}
	endpoint, err := azureFeedbackURL()
	if err != nil {
		return nil, err
	}
	var configured azureFeedbackConnection
	key := "azure-feedback:" + account
	err = a.store.change(ctx, key, &configured, func(found bool) error {
		if found && configured.PlanID == actualPlan && configured.Endpoint == endpoint && configured.Secret != "" {
			if configured.Status != "ready" {
				configured.Status = "pending"
				configured.NextAttemptAt = a.clock().Add(5 * time.Minute)
			}
			return nil
		}
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return err
		}
		configured = azureFeedbackConnection{AccountID: account, PlanID: actualPlan, Topic: connection.Plan.communicationPath(), Endpoint: endpoint, Subscription: "memqlemail" + emailStateID(account)[:48], Secret: base64.RawURLEncoding.EncodeToString(secret), Status: "pending", NextAttemptAt: a.clock().Add(5 * time.Minute)}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return a.withCluster(ctx, account, func(session azureSession) (map[string]any, error) {
		if session.ClusterID != connection.Plan.ClusterID {
			return nil, fmt.Errorf("the cluster email connection changed")
		}
		path := connection.Plan.communicationPath() + "/providers/Microsoft.EventGrid/eventSubscriptions/" + configured.Subscription + "?api-version=" + azureEventGridVersion
		// The service itself is exclusively claimed by this organization. Refuse
		// replacing any foreign event subscription at the deterministic name.
		var existing struct {
			Properties struct {
				ProvisioningState string `json:"provisioningState"`
				Destination       struct {
					EndpointType string `json:"endpointType"`
					Properties   struct {
						EndpointBaseURL string `json:"endpointBaseUrl"`
					} `json:"properties"`
				} `json:"destination"`
			} `json:"properties"`
		}
		err := a.protocol.arm(ctx, http.MethodGet, path, session.AccessToken, nil, &existing)
		if err != nil && !azureNotFound(err) {
			return nil, err
		}
		if err == nil && (existing.Properties.Destination.EndpointType != "WebHook" || existing.Properties.Destination.Properties.EndpointBaseURL != endpoint) {
			return nil, fmt.Errorf("the Azure feedback subscription belongs to a different endpoint")
		}
		finish := func(ready bool) (map[string]any, error) {
			state := "pending"
			if ready {
				state = "ready"
			}
			err := a.store.change(ctx, key, &configured, func(found bool) error {
				if !found || configured.PlanID != actualPlan || configured.Endpoint != endpoint {
					return fmt.Errorf("the feedback configuration changed")
				}
				configured.Status = state
				configured.Applied = true
				return nil
			})
			return map[string]any{"deliveryFeedback": state}, err
		}
		// A prior PUT may have finished after its response was lost. Observe
		// completion without restarting provisioning. A newly minted credential
		// must be installed at least once, even when a named resource exists.
		if err == nil && (configured.Applied || configured.Status == "ready") {
			switch existing.Properties.ProvisioningState {
			case "Succeeded":
				return finish(true)
			case "Creating", "Updating", "AwaitingManualAction":
				return finish(false)
			}
		}
		body := map[string]any{"properties": map[string]any{
			"destination": map[string]any{"endpointType": "WebHook", "properties": map[string]any{"endpointUrl": endpoint, "maxEventsPerBatch": 1, "preferredBatchSizeInKilobytes": 64, "deliveryAttributeMappings": []map[string]any{
				{"name": "Authorization", "type": "Static", "properties": map[string]any{"value": "Bearer " + configured.Secret, "isSecret": true}},
				{"name": "X-Memql-Email-Account", "type": "Static", "properties": map[string]any{"value": account, "isSecret": false}},
			}}},
			"filter":              map[string]any{"includedEventTypes": []string{azureDeliveryEvent}},
			"eventDeliverySchema": "EventGridSchema",
			"retryPolicy":         map[string]any{"maxDeliveryAttempts": 30, "eventTimeToLiveInMinutes": 1440},
		}}
		// Do not let a successful GET survive an empty asynchronous PUT body.
		existing.Properties.ProvisioningState = ""
		if err = a.protocol.arm(ctx, http.MethodPut, path, session.AccessToken, body, &existing); err != nil {
			return nil, err
		}
		return finish(existing.Properties.ProvisioningState == "Succeeded")
	})
}

func (a *azureSetup) feedbackStatus(ctx context.Context, account string) (string, error) {
	var configured azureFeedbackConnection
	found, _, err := a.store.read(ctx, "azure-feedback:"+account, &configured)
	if err != nil {
		return "", err
	}
	if !found {
		return "not-configured", nil
	}
	var connection azureConnection
	active, _, err := a.store.read(ctx, "organization:"+account, &connection)
	if err != nil {
		return "", err
	}
	var cluster azureClusterConnection
	connected, _, err := a.store.read(ctx, azureClusterKey, &cluster)
	if err != nil {
		return "", err
	}
	if !active || connection.Status != "ready" || connection.AccountID != account || configured.PlanID != azurePlanID(connection) || !sameAzureResource(configured.Topic, connection.Plan.communicationPath()) || !connected || (cluster.Status != "connected" && cluster.Status != "reauthorize") || cluster.ID != connection.Plan.ClusterID {
		return "not-configured", nil
	}
	return configured.Status, nil
}

// Every ready organization is durable work, including imported and installed
// senders. Existing sealed rows are classified in bounded pages; credentials
// remain encrypted and are never projected to a caller.
func (i *Integration) PollFeedbackConnections(ctx context.Context) error {
	if i == nil || i.azure == nil {
		return nil
	}
	a := i.azure
	store := &storedACSOperations{connection: a.store}
	old, err := store.rows(ctx, "emailUnindexedConnections", map[string]any{"dueBefore": a.clock().Format(time.RFC3339Nano)})
	if err != nil {
		return err
	}
	for _, row := range old {
		if _, err := a.indexFeedbackConnection(ctx, memql.BareShortId(fmt.Sprint(row["id"])), false); err != nil && i.logger != nil {
			i.logger.Warn("A sealed email connection could not be classified; it will retry")
		}
	}
	rows, err := store.rows(ctx, "emailReadyFeedbackOrganizations", map[string]any{"dueBefore": a.clock().Format(time.RFC3339Nano)})
	if err != nil {
		return err
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		claimed, err := a.indexFeedbackConnection(ctx, memql.BareShortId(fmt.Sprint(row["id"])), true)
		if err != nil {
			if i.logger != nil {
				i.logger.Warn("A sealed email connection could not be scheduled; it will retry")
			}
			continue
		}
		if !claimed {
			// Another replica took this check since the list was read.
			continue
		}
		account, _ := row["accountId"].(string)
		if account == "" {
			continue
		}
		if _, err := a.ensureDeliveryFeedback(ctx, account, ""); err != nil && i.logger != nil {
			i.logger.Warn("Azure email feedback setup will retry", "error", err)
		}
	}
	return nil
}

// The intervals the feedback indexer schedules a sealed row's next look at.
// Each scheduling is a new version of the row (indexEmailConnectionState), so
// the interval is what the row costs: at five minutes, 288 versions a day for
// every ready organization, re-checking a subscription Azure already confirmed
// (about 451 a day on production on 2026-11-06).
const (
	// feedbackSetupRetry is how soon an organization whose delivery feedback is
	// not yet confirmed -- never configured, provisioning, or a confirmation
	// that did not land -- is looked at again: the pace setup completes at.
	feedbackSetupRetry = 5 * time.Minute
	// feedbackRecheck is how soon a CONFIRMED subscription (status ready, its
	// credential applied) is checked again. The check only re-reads it and
	// re-creates it if it was deleted from Azure, which this bounds to an hour
	// without delivery feedback.
	feedbackRecheck = time.Hour
	// unreadableRetry is how soon a row that would not decrypt is tried again.
	// It stays unreadable until an operator restores the key it was sealed
	// under, so trying every five minutes only added versions.
	unreadableRetry = time.Hour
)

// Classifying or scheduling a sealed row holds its same connection gate and
// re-reads it. It updates metadata alone: a concurrent disconnect cannot be
// replaced with the older credential that the scan observed.
//
// It answers whether THIS call took the row's due look. A row already
// classified whose look is no longer due under the gate was rescheduled by
// another replica since the list was read; it is left as it stands, nothing is
// written, and the caller does not provision it a second time. Every replica's
// poller lists due rows every 30 seconds, so without this check two replicas
// listing one row each wrote it and each called Azure.
func (a *azureSetup) indexFeedbackConnection(ctx context.Context, rowID string, attempt bool) (bool, error) {
	db, err := a.store.gates()
	if err != nil {
		return false, err
	}
	hold, err := memql.AcquireGateHold(context.WithoutCancel(ctx), db, 0, "email-connection:"+rowID)
	if err != nil {
		return false, err
	}
	defer hold.Release()
	rows, err := (&storedACSOperations{connection: a.store}).rows(ctx, "emailConnectionStateById", map[string]any{"stateId": rowID})
	if err != nil || len(rows) == 0 {
		return false, err
	}
	row := rows[0]
	if purpose, _ := row["purpose"].(string); purpose != "" {
		if next, ok := rowInstant(row["nextAttemptAt"]); ok && next.After(a.clock()) {
			return false, nil
		}
	}
	plain, decryptErr := secret.Decrypt(fmt.Sprint(row["encryptedValue"]))
	var connection azureConnection
	if decryptErr == nil {
		decryptErr = json.Unmarshal([]byte(plain), &connection)
	}
	purpose, account := "internal", ""
	due := a.clock()
	if decryptErr != nil {
		purpose = "unreadable"
		due = due.Add(unreadableRetry)
	} else if connection.Plan.ClusterID != "" && connection.AccountID != "" {
		purpose, account = "azure-organization-"+connection.Status, connection.AccountID
		if attempt {
			due = due.Add(a.feedbackInterval(ctx, connection.AccountID))
		}
	}
	prior, _ := rowInstant(row["createdAt"])
	values := map[string]string{"id": rowID, "purpose": purpose, "accountId": account, "nextAttemptAt": due.Format(time.RFC3339Nano), "versionTime": memql.VersionTimeAfter(prior, a.clock()).Format(time.RFC3339Nano)}
	if err := hold.Verify(ctx); err != nil {
		return false, err
	}
	write, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, err = a.store.engine.Execute(emailStateContext(write), renderConfigCall("indexEmailConnectionState", values))
	if err == nil && decryptErr != nil {
		return false, fmt.Errorf("could not classify a sealed email connection")
	}
	return err == nil, err
}

// feedbackInterval is how soon an organization's delivery feedback is looked
// at again after this look: an hour once Azure has confirmed its subscription
// and the credential is applied, five minutes otherwise. Read before this
// look runs, so the look that confirms a subscription is followed by one more
// at five minutes before the hourly pace begins; a state that cannot be read
// is not a confirmed one.
func (a *azureSetup) feedbackInterval(ctx context.Context, account string) time.Duration {
	var configured azureFeedbackConnection
	found, _, err := a.store.read(ctx, "azure-feedback:"+account, &configured)
	if err == nil && found && configured.Status == "ready" && configured.Applied {
		return feedbackRecheck
	}
	return feedbackSetupRetry
}

// rowInstant reads a stored time the engine hands back as a time.Time or as
// RFC 3339 text.
func rowInstant(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, !t.IsZero()
	case string:
		at, err := time.Parse(time.RFC3339Nano, t)
		return at, err == nil && !at.IsZero()
	}
	return time.Time{}, false
}
