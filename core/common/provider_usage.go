package common

import (
	"context"
	"sync"

	"github.com/znasllc-io/memql/core/costestimate"
)

// ProviderUsage belongs to one attempt, not a shared provider instance. An
// attempt-scoped cell prevents concurrent calls and fallbacks from borrowing
// each other's model or cached-token receipts. Counts are cumulative snapshots.
type ProviderUsage struct {
	Model  string
	Vendor string
	Usage  costestimate.Usage
}
type usageCell struct {
	sync.Mutex
	report ProviderUsage
}
type usageKey struct{}

func WithProviderUsage(ctx context.Context) context.Context {
	return context.WithValue(ctx, usageKey{}, &usageCell{})
}
func ReportProviderUsage(ctx context.Context, report ProviderUsage) {
	if cell, ok := ctx.Value(usageKey{}).(*usageCell); ok {
		cell.Lock()
		cell.report = report
		cell.Unlock()
	}
}
func ReadProviderUsage(ctx context.Context) ProviderUsage {
	if cell, ok := ctx.Value(usageKey{}).(*usageCell); ok {
		cell.Lock()
		defer cell.Unlock()
		return cell.report
	}
	return ProviderUsage{}
}
