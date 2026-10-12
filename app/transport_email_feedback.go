//go:build bff

package app

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/campaigns"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/email"
)

// Azure Event Grid requires an HTTP webhook. The owner approved this exact
// exception on 2026-10-12; authorization lives in the protocol handler.
func (a *App) mountAzureEmailFeedback() {
	handler := email.NewAzureFeedbackHandler(a.emailIntegration, func(ctx context.Context, report email.AzureDeliveryReport) error {
		provider := a.engine.Integrations().Provider("campaigns")
		worker, ok := provider.(*campaigns.Worker)
		if !ok || worker == nil {
			return fmt.Errorf("campaign feedback is unavailable")
		}
		return worker.ApplyAzureDelivery(ctx, report)
	}, func(ctx context.Context, deliveryID string) (string, error) {
		worker, ok := a.engine.Integrations().Provider("campaigns").(*campaigns.Worker)
		if !ok || worker == nil {
			return "", fmt.Errorf("campaign feedback is unavailable")
		}
		return worker.AzureDeliveryGateKey(ctx, deliveryID)
	})
	for _, path := range server.AzureEmailFeedbackPaths() {
		a.handleRoute("POST "+path, handler)
	}
}
