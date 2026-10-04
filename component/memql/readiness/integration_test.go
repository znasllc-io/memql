package readiness

import (
	"reflect"
	"testing"
)

// The pipelines report's envelope as component/pipelinerun/status.go writes
// it: three settings and no credentials (pipelines program design record,
// D15 -- the item's three sub-steps are these three facts).
const pipelinesEnvelope = `{"checkedAt":"2026-10-04T12:00:00Z","probed":false,"integrations":[
 {"name":"pipelines","state":"needs_configuration","detail":"Not set up yet",
  "settings":[
   {"name":"githubApp","source":"configured","present":true,"purpose":"p"},
   {"name":"repository","source":"unset","present":false,"purpose":"p"},
   {"name":"runner","source":"configured","present":true,"purpose":"p"}],
  "credentials":[{"name":"token","present":true,"purpose":"never a slot"}]}]}`

func TestAnIntegrationsSettingsBecomeOneLaneOfSlots(t *testing.T) {
	lane := IntegrationLane([]byte(pipelinesEnvelope), "pipelines")
	if lane == nil {
		t.Fatal("a report naming settings answers a lane")
	}
	if lane.Name != IntegrationLaneName || lane.ConfigurableFrom != "os" || lane.Complete || lane.Scope != "" {
		t.Errorf("lane = %+v: a node-scoped lane, incomplete while the report needs configuration", lane)
	}
	want := []SlotReport{
		{Name: "githubApp", Present: true, Source: "configured"},
		{Name: "repository", Present: false, Source: ""},
		{Name: "runner", Present: true, Source: "configured"},
	}
	if !reflect.DeepEqual(lane.Slots, want) {
		t.Errorf("slots = %+v, want %+v: the settings in order, credentials left out", lane.Slots, want)
	}

	configured := []byte(`{"integrations":[{"name":"pipelines","state":"configured","settings":[{"name":"runner","source":"configured"}]}]}`)
	if got := IntegrationLane(configured, "pipelines"); got == nil || !got.Complete {
		t.Errorf("a configured report's lane is complete: %+v", got)
	}
	for name, payload := range map[string][]byte{
		"another integration's report": []byte(pipelinesEnvelope),
		"a report naming no setting":   []byte(`{"integrations":[{"name":"email","state":"configured","settings":[]}]}`),
		"a payload that is no envelope": []byte(`not json`),
	} {
		if got := IntegrationLane(payload, "email"); got != nil {
			t.Errorf("%s answers no lane: %+v", name, got)
		}
	}
}
