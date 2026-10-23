package models

import (
	"context"
	"encoding/json"
	"testing"
)

func TestOllamaActiveParametersPreserveSharedWeightsAndTotal(t *testing.T) {
	srv, _ := ollamaStub(t, []string{"mixture"}, map[string]string{"mixture": `{"model_info":{"general.architecture":"qwen35moe","general.parameter_count":35000000000,"qwen35moe.expert_count":256,"qwen35moe.expert_used_count":8},"tensors":[{"name":"blk.0.ffn_up_exps.weight","shape":[1000000,129,256]},{"name":"token_embd.weight","shape":[1976000000]}]}`})
	inv := discovererFor(srv.URL, nil).Discover(context.Background(), Request{})
	if len(inv.Models) != 1 {
		t.Fatalf("inventory: %+v", inv)
	}
	got := ParseAttributes(inv.Models[0].Attributes.String())
	if got.Params != 35000000000 || got.ActiveParams != 3008000000 {
		t.Fatalf("total/active: %+v", got)
	}
}

func TestOllamaActiveParametersRefuseIncompleteOrMalformedEvidence(t *testing.T) {
	for _, raw := range []string{
		`{"model_info":{"general.parameter_count":35000000000}}`,
		`{"model_info":{"general.architecture":"x","general.parameter_count":35000000000,"x.expert_count":256,"x.expert_used_count":8}}`,
		`{"model_info":{"general.architecture":"x","general.parameter_count":35000000000,"x.expert_count":256,"x.expert_used_count":300},"tensors":[{"name":"blk.0.ffn_up_exps.weight","shape":[1000,1000,256]}]}`,
		`{"model_info":{"general.architecture":"x","general.parameter_count":35000000000,"x.expert_count":256,"x.expert_used_count":8},"tensors":[{"name":"blk.0.ffn_up_exps.weight","shape":[1000,1000,128]}]}`,
		`{"model_info":{"general.architecture":"x","general.parameter_count":35000000000,"x.expert_count":256,"x.expert_used_count":8},"tensors":[{"name":"blk.0.ffn_up_exps.weight","shape":[1000000000000000,1000000000000000,256]}]}`,
	} {
		var show ollamaShowResponse
		if err := json.Unmarshal([]byte(raw), &show); err != nil {
			t.Fatal(err)
		}
		if got := ParseAttributes(Attributes{ActiveParams: ollamaActiveParams(show)}.String()).ActiveParams; got != 0 {
			t.Errorf("active=%d for %s", got, raw)
		}
	}
}

func TestActiveParameterLabelRoundTripAndInvalidValues(t *testing.T) {
	a := Attributes{Params: 35000000000, ActiveParams: 3000000000}
	if got := ParseAttributes(a.String()); got.Params != a.Params || got.ActiveParams != a.ActiveParams {
		t.Fatalf("roundtrip %+v", got)
	}
	for _, label := range []string{"activeparams=0", "activeparams=-1", "activeparams=3B", "activeparams=1e10", "activeparams=99999999999999999999"} {
		if got := ParseAttributes(label); got.ActiveParams != 0 {
			t.Errorf("accepted %q", label)
		}
	}
}
