package worker

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/types/known/structpb"
)

// validDescriptorJSON mirrors exactly what the cockpit's
// ComputeCapabilities emits (memql-cockpit#169).
const validDescriptorJSON = `{"platform":"darwin","displayServer":"quartz","computerUseAvailable":true,"actions":["screenshot","mouse_move","mouse_click","key_type"],"schemaVersion":1}`

func registerMsg(caps []string, descriptorJSON string) *memqlv1.Register {
	return &memqlv1.Register{
		Name:                     "test-worker",
		Capabilities:             caps,
		CapabilityDescriptorJson: descriptorJSON,
	}
}

func TestValidateRegister(t *testing.T) {
	tooManyActions := make([]string, maxCapabilityDescriptorActions+1)
	for i := range tooManyActions {
		tooManyActions[i] = "action_" + strings.Repeat("x", 3) + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	tooManyJSON, _ := json.Marshal(map[string]any{
		"platform": "linux", "displayServer": "x11", "computerUseAvailable": true,
		"actions": tooManyActions, "schemaVersion": 1,
	})

	cases := []struct {
		name           string
		caps           []string
		descriptorJSON string
		wantErr        string
		wantDescriptor bool
	}{
		// Forward compatibility: old-style registrations (no
		// descriptor) keep working exactly as before.
		{name: "headless only no descriptor", caps: []string{"HEADLESS"}},
		{name: "headless plus computeruse no descriptor", caps: []string{"HEADLESS", "COMPUTERUSE"}},
		{name: "no capabilities rejected", caps: nil, wantErr: "at least one capability"},
		{name: "unknown capability string rejected", caps: []string{"HEADLESS", "computer:screenshot"}, wantErr: "unknown capability"},
		{name: "computeruse without headless rejected", caps: []string{"COMPUTERUSE"}, wantErr: "HEADLESS capability is mandatory"},

		// Structured descriptor admission.
		{name: "valid descriptor accepted", caps: []string{"HEADLESS", "COMPUTERUSE"}, descriptorJSON: validDescriptorJSON, wantDescriptor: true},
		{name: "valid headless descriptor accepted", caps: []string{"HEADLESS"},
			descriptorJSON: `{"platform":"linux","displayServer":"none","computerUseAvailable":false,"actions":[],"schemaVersion":1}`,
			wantDescriptor: true},
		{name: "unknown extra json fields tolerated", caps: []string{"HEADLESS"},
			descriptorJSON: `{"platform":"linux","displayServer":"none","computerUseAvailable":false,"actions":[],"schemaVersion":1,"futureField":"ok"}`,
			wantDescriptor: true},
		{name: "garbage json rejected", caps: []string{"HEADLESS"}, descriptorJSON: `{not json`, wantErr: "invalid JSON"},
		{name: "wrong schema version rejected", caps: []string{"HEADLESS"},
			descriptorJSON: `{"platform":"darwin","displayServer":"quartz","computerUseAvailable":true,"actions":[],"schemaVersion":2}`,
			wantErr:        "unsupported schemaVersion"},
		{name: "missing schema version rejected", caps: []string{"HEADLESS"},
			descriptorJSON: `{"platform":"darwin","displayServer":"quartz","computerUseAvailable":true,"actions":[]}`,
			wantErr:        "unsupported schemaVersion"},
		{name: "unknown display server rejected", caps: []string{"HEADLESS"},
			descriptorJSON: `{"platform":"darwin","displayServer":"mir","computerUseAvailable":true,"actions":[],"schemaVersion":1}`,
			wantErr:        "unknown displayServer"},
		{name: "empty platform rejected", caps: []string{"HEADLESS"},
			descriptorJSON: `{"platform":"","displayServer":"none","computerUseAvailable":false,"actions":[],"schemaVersion":1}`,
			wantErr:        "invalid platform"},
		{name: "action with whitespace rejected", caps: []string{"HEADLESS"},
			descriptorJSON: `{"platform":"darwin","displayServer":"quartz","computerUseAvailable":true,"actions":["mouse move"],"schemaVersion":1}`,
			wantErr:        "invalid action name"},
		{name: "duplicate action rejected", caps: []string{"HEADLESS"},
			descriptorJSON: `{"platform":"darwin","displayServer":"quartz","computerUseAvailable":true,"actions":["screenshot","screenshot"],"schemaVersion":1}`,
			wantErr:        "duplicate action"},
		{name: "too many actions rejected", caps: []string{"HEADLESS"}, descriptorJSON: string(tooManyJSON), wantErr: "exceeds cap"},
		{name: "oversized raw json rejected", caps: []string{"HEADLESS"},
			descriptorJSON: `{"platform":"` + strings.Repeat("a", maxCapabilityDescriptorBytes) + `"}`,
			wantErr:        "byte cap"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			desc, err := validateRegister(registerMsg(tc.caps, tc.descriptorJSON))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %q", tc.wantErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantDescriptor && desc == nil {
				t.Fatalf("expected a parsed descriptor, got nil")
			}
			if !tc.wantDescriptor && desc != nil {
				t.Fatalf("expected nil descriptor, got %+v", desc)
			}
		})
	}
}

func TestParseCapabilityDescriptor_RoundTripAndNullActions(t *testing.T) {
	desc, err := ParseCapabilityDescriptor(validDescriptorJSON)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if desc.Platform != "darwin" || desc.DisplayServer != "quartz" || !desc.ComputerUseAvailable {
		t.Fatalf("unexpected descriptor: %+v", desc)
	}
	if len(desc.Actions) != 4 || desc.Actions[0] != "screenshot" {
		t.Fatalf("unexpected actions: %v", desc.Actions)
	}

	// Absent actions normalize to an empty (non-nil) slice so the
	// downstream JSON shape is stable ("actions": [] never null).
	noActions, err := ParseCapabilityDescriptor(`{"platform":"linux","displayServer":"none","computerUseAvailable":false,"schemaVersion":1}`)
	if err != nil {
		t.Fatalf("parse without actions: %v", err)
	}
	if noActions.Actions == nil || len(noActions.Actions) != 0 {
		t.Fatalf("expected empty non-nil actions, got %#v", noActions.Actions)
	}

	// AsMap -> capabilityDescriptorFromMap is the persistence round
	// trip (engine payload object in both directions).
	back := capabilityDescriptorFromMap(desc.AsMap())
	if back == nil {
		t.Fatalf("round trip returned nil")
	}
	if back.Platform != desc.Platform || back.DisplayServer != desc.DisplayServer ||
		back.ComputerUseAvailable != desc.ComputerUseAvailable || back.SchemaVersion != desc.SchemaVersion ||
		len(back.Actions) != len(desc.Actions) {
		t.Fatalf("round trip mismatch: %+v vs %+v", back, desc)
	}

	// Nil-safety on both helpers.
	if (*CapabilityDescriptor)(nil).AsMap() != nil {
		t.Fatalf("nil AsMap must return nil")
	}
	if capabilityDescriptorFromMap(nil) != nil {
		t.Fatalf("empty map must decode to nil")
	}
	// Corrupted stored payloads degrade to nil rather than erroring.
	if capabilityDescriptorFromMap(map[string]any{"schemaVersion": 99}) != nil {
		t.Fatalf("corrupted stored descriptor must decode to nil")
	}
}

func TestRegistryCarriesCapabilityDescriptor(t *testing.T) {
	desc, err := ParseCapabilityDescriptor(validDescriptorJSON)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r := NewRegistry(nil, func() time.Time { return time.Unix(0, 0).UTC() })
	r.Add(&Worker{
		RegistrationId:       "reg-1",
		OwnerUserId:          "user-1",
		Name:                 "mbp",
		Capabilities:         []string{CapabilityHeadless, CapabilityComputerUse},
		CapabilityDescriptor: desc,
	})
	r.Add(&Worker{
		RegistrationId: "reg-2",
		OwnerUserId:    "user-1",
		Name:           "old-build",
		Capabilities:   []string{CapabilityHeadless},
	})

	workers := r.WorkersForUser("user-1")
	if len(workers) != 2 {
		t.Fatalf("expected 2 workers, got %d", len(workers))
	}
	byId := map[string]*Worker{}
	for _, w := range workers {
		byId[w.RegistrationId] = w
	}
	got := byId["reg-1"].CapabilityDescriptor
	if got == nil || got.DisplayServer != "quartz" || len(got.Actions) != 4 {
		t.Fatalf("descriptor not carried through registry: %+v", got)
	}
	if byId["reg-2"].CapabilityDescriptor != nil {
		t.Fatalf("old-style worker must carry nil descriptor")
	}
}

func TestDecodeRegistrationCapabilityDescriptor(t *testing.T) {
	payload, err := structpb.NewStruct(map[string]any{
		"ownerUserId":  "user-1",
		"identityId":   "ident-1",
		"name":         "mbp",
		"capabilities": []any{"HEADLESS", "COMPUTERUSE"},
		"capabilityDescriptor": map[string]any{
			"platform":             "darwin",
			"displayServer":        "quartz",
			"computerUseAvailable": true,
			"actions":              []any{"screenshot", "key_type"},
			"schemaVersion":        1,
		},
	})
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}
	row := decodeRegistration(&memqlv1.MemoryNode{Id: "reg-1", Payload: payload})
	if row == nil {
		t.Fatalf("decode returned nil")
	}
	if row.CapabilityDescriptor == nil {
		t.Fatalf("expected decoded descriptor")
	}
	if row.CapabilityDescriptor.Platform != "darwin" || len(row.CapabilityDescriptor.Actions) != 2 {
		t.Fatalf("unexpected decoded descriptor: %+v", row.CapabilityDescriptor)
	}

	// Empty-object persistence (older rows / no descriptor sent)
	// decodes to nil.
	emptyPayload, err := structpb.NewStruct(map[string]any{
		"ownerUserId":          "user-1",
		"identityId":           "ident-1",
		"name":                 "old",
		"capabilities":         []any{"HEADLESS"},
		"capabilityDescriptor": map[string]any{},
	})
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}
	oldRow := decodeRegistration(&memqlv1.MemoryNode{Id: "reg-2", Payload: emptyPayload})
	if oldRow.CapabilityDescriptor != nil {
		t.Fatalf("empty stored object must decode to nil, got %+v", oldRow.CapabilityDescriptor)
	}
}
