package email

import (
	"encoding/json"
	"os"
	"testing"
)

func TestAzureRegistrationRequestsOnlyDelegatedResourceManagement(t *testing.T) {
	if !azureUUID.MatchString(defaultAzureApplicationID) {
		t.Fatal("installations must ship MemQL's registered public application ID")
	}
	// This is the manifest reviewed before creating MemQL's first-party
	// application. It must not acquire Graph/mailbox/application permissions
	// or introduce a client secret to the device sign-in flow.
	data, err := os.ReadFile("azure-application.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest) != 4 || manifest["displayName"] != "MemQL Azure Connection" || manifest["signInAudience"] != "AzureADMultipleOrgs" || manifest["isFallbackPublicClient"] != true {
		t.Fatal("unexpected application configuration")
	}
	permissions, _ := json.Marshal(manifest["requiredResourceAccess"])
	var resources []struct {
		ResourceAppID string `json:"resourceAppId"`
		Access        []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"resourceAccess"`
	}
	if err := json.Unmarshal(permissions, &resources); err != nil {
		t.Fatal(err)
	}
	if len(resources) != 1 || resources[0].ResourceAppID != "797f4846-ba00-4fd7-ba43-dac1f8f63013" || len(resources[0].Access) != 1 || resources[0].Access[0].ID != "41094075-9dad-400e-a0bd-54e686782033" || resources[0].Access[0].Type != "Scope" {
		t.Fatal("registration must request only Azure Resource Manager user_impersonation")
	}
}
