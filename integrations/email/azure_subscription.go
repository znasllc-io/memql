package email

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

func (a *azureProtocol) ensureSubscription(ctx context.Context, token, clusterID string, plan azurePlan) (bool, error) {
	providerPath := "/subscriptions/" + plan.SubscriptionID + "/providers/Microsoft.Communication"
	var provider struct {
		RegistrationState string `json:"registrationState"`
	}
	if err := a.arm(ctx, http.MethodGet, providerPath+"?api-version=2021-04-01", token, nil, &provider); err != nil {
		return false, err
	}
	if provider.RegistrationState != "Registered" {
		if provider.RegistrationState != "Registering" {
			if err := a.arm(ctx, http.MethodPost, providerPath+"/register?api-version=2021-04-01", token, nil, nil); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	groupPath := "/subscriptions/" + plan.SubscriptionID + "/resourceGroups/" + url.PathEscape(plan.ResourceGroup) + "?api-version=2021-04-01"
	var group struct {
		Location string            `json:"location"`
		Tags     map[string]string `json:"tags"`
	}
	err := a.arm(ctx, http.MethodGet, groupPath, token, nil, &group)
	if azureNotFound(err) && plan.CreateResourceGroup {
		err = a.arm(ctx, http.MethodPut, groupPath, token, map[string]any{"location": plan.ResourceGroupLocation, "tags": map[string]string{"memql-cluster": clusterID}}, &group)
	}
	if err != nil {
		return false, err
	}
	if plan.CreateResourceGroup && (group.Tags["memql-cluster"] != clusterID || group.Location != plan.ResourceGroupLocation) {
		return false, fmt.Errorf("this resource group already exists with different settings; select it as an existing group or choose another name")
	}
	return true, nil
}
