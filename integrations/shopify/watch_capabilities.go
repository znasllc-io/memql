package shopify

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/num"
)

// watch_capabilities.go -- the watch's builtins: the pass the automation
// runs, and the alert wait read and set. The pass and what it decides are
// watch.go's.

func (i *Integration) watchCapabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "watchStores",
			Description: "One pass of the store watch: compute every store's verdict, record it on the store's health, and email the store's owner when it has needed attention for the alert wait and when it is recording again. Driven by the watchShopifyStores automation; a call without internal origin is refused.",
			Handler:     i.handleWatchStores,
		},
		{
			Name:        "storeAlertWait",
			Description: "The minutes a store needs attention before its owner is emailed: {minutes, isDefault, maySet}. maySet says whether the caller, a cluster owner, may change it.",
			Handler:     i.handleStoreAlertWait,
		},
		{
			Name:        "storeAlertWaitSet",
			Description: "Set the minutes a store needs attention before its owner is emailed, from 1 to 1440. For a cluster owner.",
			Handler:     i.handleStoreAlertWaitSet,
			ArgsSchema:  map[string]string{"minutes": "int - 1 to 1440"},
		},
	}
}

// requireInternalOrigin refuses a call that is not the engine's own: a
// builtin takes no @serverOnly, so the handler is where a builtin only an
// automation may call says no.
func requireInternalOrigin(ctx context.Context, builtin string) error {
	if !auth.OriginFromContext(ctx).IsInternal() {
		return fmt.Errorf("shopify: %s is driven by the engine's own automation and refuses a client-origin call", builtin)
	}
	return nil
}

func (i *Integration) handleWatchStores(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireInternalOrigin(ctx, "shopifyWatchStores"); err != nil {
		return nil, err
	}
	if i.connector == nil {
		return resultNode("shopify", map[string]any{"status": "unconfigured"})
	}
	if err := i.connector.WatchStores(ctx); err != nil {
		return nil, err
	}
	return resultNode("shopify", map[string]any{"status": "watched"})
}

func isClusterOwner(ctx context.Context) bool {
	ac, ok := auth.AccessFromContext(ctx)
	return ok && ac != nil && ac.IsClusterOwner()
}

func alertWaitReply(wait time.Duration, isDefault, maySet bool) ([]memorynodes.MemoryNode, error) {
	return resultNode("shopify", map[string]any{"minutes": int(wait / time.Minute), "isDefault": isDefault, "maySet": maySet})
}

func (i *Integration) handleStoreAlertWait(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i.connector == nil {
		return alertWaitReply(defaultStoreAlertWait, true, false)
	}
	wait, isDefault := i.connector.alertWaitSetting(ctx)
	return alertWaitReply(wait, isDefault, isClusterOwner(ctx))
}

// handleStoreAlertWaitSet refuses anybody but a cluster owner, and any value
// that is not a whole number of minutes from 1 to 1440, before it reads or
// writes anything.
func (i *Integration) handleStoreAlertWaitSet(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if !isClusterOwner(ctx) {
		return nil, fmt.Errorf("shopify: shopifyStoreAlertWaitSet is reserved to a cluster owner")
	}
	minutes, ok := wholeMinutes(args["minutes"])
	if !ok || !validAlertMinutes(minutes) {
		return nil, fmt.Errorf("shopify: the alert wait is a whole number of minutes from 1 to 1440")
	}
	c := i.connector
	if c == nil {
		return resultNode("shopify", map[string]any{"status": "unconfigured"})
	}
	opCtx := operatorContext(ctx)
	id, err := namedRowID(opCtx, c.engine, conceptGlobalVariable, storeAlertWaitVariable, variableRowID(storeAlertWaitVariable))
	if err != nil {
		return nil, err
	}
	if _, err := c.engine.Execute(opCtx, renderCall("setGlobalVariable", map[string]any{
		"id": id, "name": storeAlertWaitVariable, "value": strconv.Itoa(minutes),
		"description": "Minutes a Shopify store needs attention before its owner is emailed (1 to 1440).", "active": true,
	})); err != nil {
		return nil, fmt.Errorf("shopify: write %s: %w", storeAlertWaitVariable, err)
	}
	return alertWaitReply(time.Duration(minutes)*time.Minute, false, true)
}

// wholeMinutes reads a whole number from a builtin's argument; a fraction,
// text or nothing is not one.
func wholeMinutes(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return num.Int64OrZero(n), true
	case float64:
		whole, ok := num.WholeInt64(n)
		if !ok {
			return 0, false
		}
		return num.Int64OrZero(whole), true
	}
	return 0, false
}
