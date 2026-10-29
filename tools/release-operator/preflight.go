package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	client "github.com/znasllc-io/memql/sdk/go/client"
)

// Fixed public destinations captured from the installed native profiles. No
// credential references or values are stored. Live authority is checked again.
//
//go:embed targets.json
var targetJSON []byte

type targetView struct {
	Component    string `json:"component"`
	Artifact     string `json:"artifact"`
	Kind         string `json:"kind"`
	Origin       string `json:"origin"`
	Repository   string `json:"repository"`
	TargetID     string `json:"targetId"`
	Tag          string `json:"tag"`
	SourceCommit string `json:"sourceCommit"`
}

var fixedTargets = func() map[string]targetView {
	var m map[string]targetView
	if err := json.Unmarshal(targetJSON, &m); err != nil {
		panic("invalid fixed public target map")
	}
	return m
}()

func validTag(profile, tag string) bool {
	prefix := "v"
	if profile == "cockpit" {
		prefix = "cockpit/v"
	}
	return regexp.MustCompile(`^` + prefix + `(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`).MatchString(tag)
}

// Builtin results contain one result node. Refuse ambiguous or unexpected shapes.
func decodePayload(raw any, out any) error {
	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	var result struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err = json.Unmarshal(b, &result); err != nil {
		return err
	}
	if len(result.Data) != 1 || len(result.Data[0]) != 1 {
		return errors.New("native result has unexpected shape")
	}
	for _, body := range result.Data[0] {
		var node struct {
			Payload json.RawMessage `json:"payload"`
		}
		if err = json.Unmarshal(body, &node); err != nil {
			return err
		}
		if len(node.Payload) == 0 {
			return errors.New("native result has no payload")
		}
		return json.Unmarshal(node.Payload, out)
	}
	return errors.New("empty native result")
}

type candidateView struct {
	CandidateID string `json:"candidateId"`
	State       string `json:"state"`
	ApprovalID  string `json:"approvalId"`
	Manifest    struct {
		Components []struct {
			Name       string `json:"name"`
			Version    string `json:"version"`
			Repository string `json:"repository"`
			Commit     string `json:"commit"`
			Artifacts  []struct {
				Name string `json:"name"`
				Kind string `json:"kind"`
			} `json:"artifacts"`
		} `json:"components"`
		Destinations []struct {
			TargetID  string `json:"targetId"`
			Component string `json:"component"`
			Artifact  string `json:"artifact"`
		} `json:"destinations"`
	} `json:"manifest"`
}

func checkCandidate(o options, c candidateView, targets []targetView) error {
	if c.CandidateID != o.candidate || len(c.Manifest.Components) != 1 {
		return errors.New("candidate identity/component count mismatch")
	}
	comp := c.Manifest.Components[0]
	prefix := "v"
	if o.profile == "cockpit" {
		prefix = "cockpit/v"
	}
	if comp.Name != o.profile || comp.Repository != "znasllc-io/memql" || comp.Commit != o.commit || prefix+comp.Version != o.tag {
		return errors.New("candidate does not match exact reviewed profile, tag, repository and commit")
	}
	if o.op == "approve" {
		if c.State != "ready" && c.State != "approved" {
			return errors.New("candidate is not ready for approval")
		}
	} else if c.State != "approved" || c.ApprovalID != o.approval {
		return errors.New("candidate is not approved under the supplied approval")
	}
	wanted := map[string]targetView{}
	for id, t := range fixedTargets {
		if t.Component == o.profile {
			wanted[id] = t
		}
	}
	if len(targets) != len(wanted) || len(c.Manifest.Destinations) != len(wanted) || len(comp.Artifacts) != len(wanted) {
		return errors.New("complete configured target/artifact set is required")
	}
	seen := map[string]bool{}
	for _, t := range targets {
		want, ok := wanted[t.TargetID]
		if !ok || seen[t.TargetID] || t.Component != want.Component || t.Artifact != want.Artifact || t.Kind != want.Kind || t.Origin != want.Origin || t.Repository != want.Repository || t.Tag != o.tag {
			return fmt.Errorf("live destination mismatch: %s", t.TargetID)
		}
		if t.Kind == "file" && t.SourceCommit != o.commit {
			return errors.New("file target source commit mismatch")
		}
		seen[t.TargetID] = true
	}
	seen = map[string]bool{}
	for _, d := range c.Manifest.Destinations {
		w, ok := wanted[d.TargetID]
		if !ok || seen[d.TargetID] || d.Component != o.profile || d.Artifact != w.Artifact {
			return errors.New("manifest destination mismatch")
		}
		seen[d.TargetID] = true
	}
	seen = map[string]bool{}
	for _, a := range comp.Artifacts {
		w, ok := wanted[o.profile+"-"+a.Name]
		if !ok || seen[a.Name] || a.Kind != w.Kind {
			return errors.New("manifest artifact mismatch")
		}
		seen[a.Name] = true
	}
	return nil
}

func preflightCandidate(ctx context.Context, qc *client.QueryClient, o options) error {
	r, err := qc.ReleaseGetCandidate(ctx, client.ReleaseGetCandidateArgs{CandidateId: o.candidate})
	if err != nil {
		return err
	}
	var c candidateView
	if err = decodePayload(r.Raw(), &c); err != nil {
		return err
	}
	r, err = qc.ReleaseCandidateTargets(ctx, client.ReleaseCandidateTargetsArgs{CandidateId: o.candidate})
	if err != nil {
		return err
	}
	var t struct {
		CandidateID string       `json:"candidateId"`
		Targets     []targetView `json:"targets"`
	}
	if err = decodePayload(r.Raw(), &t); err != nil {
		return err
	}
	if t.CandidateID != o.candidate {
		return errors.New("target candidate identity mismatch")
	}
	if err = checkCandidate(o, c, t.Targets); err != nil {
		return err
	}
	if o.op != "seal" {
		return nil
	}
	r, err = qc.ReleaseCandidatePublications(ctx, client.ReleaseCandidatePublicationsArgs{CandidateId: o.candidate})
	if err != nil {
		return err
	}
	var pubs struct {
		CandidateID  string `json:"candidateId"`
		Publications []struct {
			CandidateID string `json:"candidateId"`
			ApprovalID  string `json:"approvalId"`
			State       string `json:"state"`
			Target      struct {
				TargetID string `json:"targetId"`
			} `json:"target"`
		} `json:"publications"`
	}
	if err = decodePayload(r.Raw(), &pubs); err != nil {
		return err
	}
	if pubs.CandidateID != o.candidate || len(pubs.Publications) != len(t.Targets) {
		return errors.New("seal requires complete publication evidence")
	}
	seen := map[string]bool{}
	for _, p := range pubs.Publications {
		expected, ok := fixedTargets[p.Target.TargetID]
		if p.CandidateID != o.candidate || p.ApprovalID != o.approval || p.State != "complete" || !ok || expected.Component != o.profile || seen[p.Target.TargetID] {
			return errors.New("seal requires each exact approved publication to be complete")
		}
		seen[p.Target.TargetID] = true
	}
	r, err = qc.ReleaseCandidateDrafts(ctx, client.ReleaseCandidateDraftsArgs{CandidateId: o.candidate})
	if err != nil {
		return err
	}
	var drafts struct {
		CandidateID string `json:"candidateId"`
		Drafts      []struct {
			CandidateID string   `json:"candidateId"`
			ApprovalID  string   `json:"approvalId"`
			State       string   `json:"state"`
			Targets     []string `json:"targets"`
		} `json:"drafts"`
	}
	if err = decodePayload(r.Raw(), &drafts); err != nil {
		return err
	}
	if drafts.CandidateID != o.candidate || len(drafts.Drafts) != 1 {
		return errors.New("seal requires one fully promoted component GitHub release")
	}
	d := drafts.Drafts[0]
	if d.CandidateID != o.candidate || d.ApprovalID != o.approval || d.State != "published" {
		return errors.New("seal requires promoted draft under exact approval")
	}
	fileCount := 0
	for _, t := range t.Targets {
		if t.Kind == "file" {
			fileCount++
		}
	}
	if len(d.Targets) != fileCount {
		return errors.New("promoted draft does not cover every file target")
	}
	seen = map[string]bool{}
	for _, id := range d.Targets {
		w, ok := fixedTargets[id]
		if !ok || w.Component != o.profile || w.Kind != "file" || seen[id] {
			return errors.New("promoted draft target mismatch")
		}
		seen[id] = true
	}
	// The native seal performs final immutable receipt/signature validation in
	// its own transaction after these read-only evidence checks.
	return nil
}
