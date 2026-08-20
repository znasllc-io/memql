// Package modules is the Go SDK surface for the module registry (epic
// memql#4183) -- the runtime inventory of what an instance runs:
// components, integrations, packs, and node-type modules, plus the one
// write, flipping a pack's per-instance enablement.
//
// The package mirrors sdk/go/pack in shape -- a Client wrapping a
// client.Dispatcher, typed methods over SDK-owned values, and errors that
// bubble up. No memqlv1.* import leaks out of the exported surface.
//
// Authorization is server-side: reads answer only to owner/admin callers,
// SetPackEnabled to owners, and a refusal comes back as a *RefusedError
// carrying the server's status code and message. Secret env vars NEVER
// carry a value -- Set is the whole answer for them; that is an engine
// guarantee, not a client courtesy.
package modules

import (
	"context"
	"fmt"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql/sdk/go/client"
)

// Client exposes the module-registry operations over a Dispatcher. Safe to
// reuse across goroutines once constructed.
type Client struct {
	dispatcher *client.Dispatcher
}

// NewClient wires a modules client to the supplied dispatcher (already
// Run()).
func NewClient(dispatcher *client.Dispatcher) *Client {
	return &Client{dispatcher: dispatcher}
}

// RefusedError is a server-side refusal or failure, carrying the gRPC
// status code the result payload reported.
type RefusedError struct {
	Code    int
	Message string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("modules: server refused (code %d): %s", e.Code, e.Message)
}

// Module is one inventory row. Kind is "component" | "integration" |
// "pack" | "node-type"; Scope says which truth tier the state is ("node" =
// the answering binary's registries and env, "cluster" = the shared
// graph).
type Module struct {
	Kind          string
	Name          string
	Description   string
	State         string
	StateDetail   string
	Scope         string
	EnvComponents []string
	FqnPrefixes   []string
	CodeReference string
}

// EnvVar is one manifest-declared environment variable on a module's
// detail surface, evaluated on the answering node. Secret entries carry
// set/unset and nothing else.
type EnvVar struct {
	Name         string
	Description  string
	Secret       bool
	Scope        string
	RequiredFor  []string
	Set          bool
	Value        string
	DefaultValue string
}

// Inventory is a List result: the rows plus which binary answered them.
type Inventory struct {
	Modules           []Module
	ReportingNodeID   string
	ReportingNodeType string
}

// Detail is one module's row plus its env surface.
type Detail struct {
	Module            Module
	EnvVars           []EnvVar
	ReportingNodeID   string
	ReportingNodeType string
}

// PackFlip is the SetPackEnabled outcome. RestartRequired is always true
// in v1 -- the flip changes what each node reads at its next boot, and the
// server says so rather than implying a live toggle.
type PackFlip struct {
	PackDomain      string
	PriorEnabled    bool
	Enabled         bool
	RestartRequired bool
}

func moduleFromProto(m *memqlv1.ModuleInfo) Module {
	return Module{
		Kind:          m.GetKind(),
		Name:          m.GetName(),
		Description:   m.GetDescription(),
		State:         m.GetState(),
		StateDetail:   m.GetStateDetail(),
		Scope:         m.GetScope(),
		EnvComponents: m.GetEnvComponents(),
		FqnPrefixes:   m.GetFqnPrefixes(),
		CodeReference: m.GetCodeReference(),
	}
}

// List returns the full module inventory as the answering node assembles
// it. Owner/admin only.
func (c *Client) List(ctx context.Context) (*Inventory, error) {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_ModulesList{
			ModulesList: &memqlv1.ModulesListMsg{},
		},
	}
	resp, err := c.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("modules.list: %w", err)
	}
	result := resp.GetModulesListResult()
	if result == nil {
		return nil, fmt.Errorf("modules.list: reply carried no ModulesListResult")
	}
	if result.GetErrorCode() != 0 {
		return nil, &RefusedError{Code: int(result.GetErrorCode()), Message: result.GetErrorMessage()}
	}
	inv := &Inventory{
		ReportingNodeID:   result.GetReportingNodeId(),
		ReportingNodeType: result.GetReportingNodeType(),
	}
	for _, m := range result.GetModules() {
		inv.Modules = append(inv.Modules, moduleFromProto(m))
	}
	return inv, nil
}

// Get returns one module's detail (row + env surface) by (kind, name).
// Owner/admin only; an unknown pair is a *RefusedError with code 5
// (NOT_FOUND).
func (c *Client) Get(ctx context.Context, kind, name string) (*Detail, error) {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_ModuleDetail{
			ModuleDetail: &memqlv1.ModuleDetailMsg{Kind: kind, Name: name},
		},
	}
	resp, err := c.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("modules.get: %w", err)
	}
	result := resp.GetModuleDetailResult()
	if result == nil {
		return nil, fmt.Errorf("modules.get: reply carried no ModuleDetailResult")
	}
	if result.GetErrorCode() != 0 {
		return nil, &RefusedError{Code: int(result.GetErrorCode()), Message: result.GetErrorMessage()}
	}
	d := &Detail{
		Module:            moduleFromProto(result.GetModule()),
		ReportingNodeID:   result.GetReportingNodeId(),
		ReportingNodeType: result.GetReportingNodeType(),
	}
	for _, v := range result.GetEnvVars() {
		d.EnvVars = append(d.EnvVars, EnvVar{
			Name:         v.GetName(),
			Description:  v.GetDescription(),
			Secret:       v.GetSecret(),
			Scope:        v.GetScope(),
			RequiredFor:  v.GetRequiredFor(),
			Set:          v.GetSet(),
			Value:        v.GetValue(),
			DefaultValue: v.GetDefaultValue(),
		})
	}
	return d, nil
}

// SetPackEnabled flips a pack's per-instance enablement. Owner only;
// audited server-side including refusals. The returned PackFlip's
// RestartRequired says when the flip takes effect (each node's next
// boot, in v1 -- always true).
func (c *Client) SetPackEnabled(ctx context.Context, packDomain string, enabled bool, reason string) (*PackFlip, error) {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_SetPackEnabled{
			SetPackEnabled: &memqlv1.SetPackEnabledMsg{
				PackDomain: packDomain,
				Enabled:    enabled,
				Reason:     reason,
			},
		},
	}
	resp, err := c.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("modules.setPackEnabled: %w", err)
	}
	result := resp.GetSetPackEnabledResult()
	if result == nil {
		return nil, fmt.Errorf("modules.setPackEnabled: reply carried no SetPackEnabledResult")
	}
	if result.GetErrorCode() != 0 {
		return nil, &RefusedError{Code: int(result.GetErrorCode()), Message: result.GetErrorMessage()}
	}
	return &PackFlip{
		PackDomain:      result.GetPackDomain(),
		PriorEnabled:    result.GetPriorEnabled(),
		Enabled:         result.GetEnabled(),
		RestartRequired: result.GetRestartRequired(),
	}, nil
}
