package main

import (
	"strings"
	"testing"
	"time"
)

func baseOptions() options {
	return options{cluster: "memql-znas-io", profile: "cockpit", timeout: time.Minute}
}
func TestComponentTagAndTargetBoundaries(t *testing.T) {
	for _, tc := range []struct {
		profile, tag string
		want         bool
	}{
		{"engine", "v0.31.8", true}, {"cockpit", "cockpit/v0.21.0", true},
		{"engine", "cockpit/v0.21.0", false}, {"cockpit", "v0.31.8", false},
		{"engine", "v0.31.8-rc1", false}, {"cockpit", "cockpit/v00.21.0", false},
	} {
		t.Run(tc.profile+tc.tag, func(t *testing.T) {
			o := baseOptions()
			o.op = "run-release"
			o.profile = tc.profile
			o.tag = tc.tag
			_, _, write, err := selectCall(o)
			if (err == nil) != tc.want || (err == nil && !write) {
				t.Fatalf("err=%v write=%v", err, write)
			}
		})
	}
	o := baseOptions()
	o.op = "publish"
	o.tag = "cockpit/v0.21.0"
	o.commit = strings.Repeat("a", 40)
	o.candidate = "candidate"
	o.approval = "approval"
	o.target = "engine-docs"
	o.artifact = "docs"
	if _, _, _, err := selectCall(o); err == nil {
		t.Fatal("cross-component target admitted")
	}
	o.profile = "engine"
	o.tag = "v0.31.8"
	if _, _, _, err := selectCall(o); err != nil {
		t.Fatal(err)
	}
	o.artifact = "sdk"
	if _, _, _, err := selectCall(o); err == nil {
		t.Fatal("mismatched artifact admitted")
	}
	o.op = "promote"
	o.target = "engine-agent"
	if _, _, _, err := selectCall(o); err == nil {
		t.Fatal("OCI draft admitted")
	}
}
func TestSealRequiresTagCommitAndApproval(t *testing.T) {
	o := baseOptions()
	o.op = "seal"
	o.candidate = "candidate"
	if _, _, _, err := selectCall(o); err == nil {
		t.Fatal("missing source admitted")
	}
	o.tag = "cockpit/v0.21.0"
	o.commit = strings.Repeat("a", 40)
	if _, _, _, err := selectCall(o); err == nil {
		t.Fatal("missing approval admitted")
	}
	o.approval = "approval"
	name, call, write, err := selectCall(o)
	if err != nil || name != "releaseSealPublishedCandidate" || !write || !strings.Contains(call, `approvalId: "approval"`) {
		t.Fatalf("%s %s %v %v", name, call, write, err)
	}
}
func TestCandidatePreflightRejectsWrongSourceAndIncompleteEvidence(t *testing.T) {
	o := baseOptions()
	o.op = "approve"
	o.tag = "cockpit/v0.21.0"
	o.commit = strings.Repeat("a", 40)
	o.candidate = "candidate"
	var c candidateView
	c.CandidateID = o.candidate
	c.State = "ready"
	if err := checkCandidate(o, c, nil); err == nil {
		t.Fatal("missing components accepted")
	}
	// Shape is constructed through the same JSON form returned by the SDK.
	raw := map[string]any{"data": []any{map[string]any{"result": map[string]any{"payload": map[string]any{"candidateId": "candidate", "state": "ready", "manifest": map[string]any{"components": []any{map[string]any{"name": "engine", "version": "0.21.0", "repository": "znasllc-io/memql", "commit": o.commit}}}}}}}}
	if err := decodePayload(raw, &c); err != nil {
		t.Fatal(err)
	}
	if err := checkCandidate(o, c, nil); err == nil {
		t.Fatal("wrong component accepted")
	}
	c.Manifest.Components[0].Name = "cockpit"
	if err := checkCandidate(o, c, nil); err == nil {
		t.Fatal("incomplete evidence accepted")
	}
}

func TestCompleteCandidateDestinationValidation(t *testing.T) {
	for _, profile := range []string{"engine", "cockpit"} {
		t.Run(profile, func(t *testing.T) {
			o := baseOptions()
			o.profile = profile
			o.op = "approve"
			o.candidate = "candidate"
			o.commit = strings.Repeat("a", 40)
			o.tag = "v0.31.8"
			version := "0.31.8"
			if profile == "cockpit" {
				o.tag = "cockpit/v0.21.0"
				version = "0.21.0"
			}
			artifacts := []any{}
			destinations := []any{}
			targets := []targetView{}
			for id, w := range fixedTargets {
				if w.Component == profile {
					artifacts = append(artifacts, map[string]any{"name": w.Artifact, "kind": w.Kind})
					destinations = append(destinations, map[string]any{"targetId": id, "component": profile, "artifact": w.Artifact})
					w.TargetID = id
					w.Tag = o.tag
					if w.Kind == "file" {
						w.SourceCommit = o.commit
					}
					targets = append(targets, w)
				}
			}
			raw := map[string]any{"data": []any{map[string]any{"result": map[string]any{"payload": map[string]any{"candidateId": o.candidate, "state": "ready", "manifest": map[string]any{"components": []any{map[string]any{"name": profile, "version": version, "repository": "znasllc-io/memql", "commit": o.commit, "artifacts": artifacts}}, "destinations": destinations}}}}}}
			var c candidateView
			if err := decodePayload(raw, &c); err != nil {
				t.Fatal(err)
			}
			if err := checkCandidate(o, c, targets); err != nil {
				t.Fatal(err)
			}
			targets[0].Repository = "unapproved/repository"
			if err := checkCandidate(o, c, targets); err == nil {
				t.Fatal("unapproved repository accepted")
			}
		})
	}
}
