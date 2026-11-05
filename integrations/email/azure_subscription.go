package email

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// azureWrites bounds how many Azure creates or updates one provision call may
// start. Each is a slow management request, so a call that did one stops and
// lets the page's next check do the next. A nil budget is unlimited. Reads
// never spend it.
type azureWrites struct{ left int }

func (w *azureWrites) take() bool {
	if w == nil {
		return true
	}
	if w.left <= 0 {
		return false
	}
	w.left--
	return true
}

func (a *azureProtocol) ensureSubscription(ctx context.Context, token, clusterID string, plan azurePlan, writes *azureWrites, workload ...bool) (bool, error) {
	// A workload identity has rights only on the email resource group.
	// The installer registers subscription providers under operator authority.
	if len(workload) == 0 || !workload[0] {
		providerPath := "/subscriptions/" + plan.SubscriptionID + "/providers/Microsoft.Communication"
		var provider struct {
			RegistrationState string `json:"registrationState"`
		}
		if err := a.arm(ctx, http.MethodGet, providerPath+"?api-version=2021-04-01", token, nil, &provider); err != nil {
			return false, err
		}
		if provider.RegistrationState != "Registered" {
			if provider.RegistrationState != "Registering" && writes.take() {
				if err := a.arm(ctx, http.MethodPost, providerPath+"/register?api-version=2021-04-01", token, nil, nil); err != nil {
					return false, err
				}
			}
			return false, nil
		}
	}
	groupPath := "/subscriptions/" + plan.SubscriptionID + "/resourceGroups/" + url.PathEscape(plan.ResourceGroup) + "?api-version=2021-04-01"
	var group struct {
		Location string            `json:"location"`
		Tags     map[string]string `json:"tags"`
	}
	err := a.arm(ctx, http.MethodGet, groupPath, token, nil, &group)
	if azureNotFound(err) && plan.CreateResourceGroup {
		if !writes.take() {
			return false, nil
		}
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
