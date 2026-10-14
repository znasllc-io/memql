package email

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAzureFeedbackClassifiesOnlyProviderStatuses(t *testing.T) {
	want := map[string]string{"Delivered": "delivered", "Bounced": "bounced", "Suppressed": "suppressed", "Failed": "failed", "FilteredSpam": "filtered-spam", "Quarantined": "quarantined", "Expanded": "expanded"}
	for status, value := range want {
		if got := azureDeliveryStatus(status); got != value {
			t.Fatalf("%s=%s", status, got)
		}
	}
	for _, status := range []string{"Succeeded", "Running", "Complaint", "SoftBounce", "delivered", ""} {
		if azureDeliveryStatus(status) != "" {
			t.Fatalf("invented outcome for %s", status)
		}
	}
}
func TestAzureMessageIdentityStaysOutOfTheWireAndMatchesMailboxCase(t *testing.T) {
	ctx := withACSMessage(context.Background(), Message{DeliveryID: "delivery-1", To: "Person <Receiver@Example.test>"}, "Sender@example.test")
	got := ctx.Value(acsMessageKey{}).(acsMessageIdentity)
	if got.RecipientDigest != azureMailboxDigest("receiver@example.test") || got.SenderDigest != azureMailboxDigest("sender@example.test") || got.DeliveryID != "delivery-1" {
		t.Fatal(got)
	}
	if azureMailboxDigest("Person <receiver@example.test>") != "" || azureMailboxDigest("bad") != "" {
		t.Fatal("provider addresses must be plain mailboxes")
	}
}
func TestAzureFeedbackRefusesBeforeReadingAnUnauthenticatedBody(t *testing.T) {
	called := false
	handler := NewAzureFeedbackHandler(func() *Integration { return &Integration{azure: &azureSetup{store: &connectionStore{}}} }, func(context.Context, AzureDeliveryReport) error { called = true; return nil })
	request := httptest.NewRequest(http.MethodPost, AzureFeedbackPath, strings.NewReader(strings.Repeat("x", 2<<20)))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 401 || called {
		t.Fatalf("code=%d applied=%v", response.Code, called)
	}
}
func TestAzureDeliveryReceiptRequiresAHeldGate(t *testing.T) {
	if (AzureDeliveryReport{}).Verify(context.Background()) == nil {
		t.Fatal("ungated native report accepted")
	}
}
