package release

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/installation"
)

const InstallationChoicesVariable = "MEMQL_RELEASE_INSTALLATION_CHOICES"

var installationCommit = regexp.MustCompile(`^[a-f0-9]{40}$`)
var installationIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// This inventory names reviewed immutable overlay commits. It contains no
// permission, target connection or credentials. The native preparer verifies
// the selected commit independently against the signed published candidate.
type installationChoice struct {
	CandidateID     string `json:"candidateId"`
	CatalogDigest   string `json:"catalogDigest"`
	OverlayRevision string `json:"overlayRevision"`
	Label           string `json:"label"`
}
type installationChoices struct {
	FormatVersion  int                  `json:"formatVersion"`
	InstallationID string               `json:"installationId"`
	Choices        []installationChoice `json:"choices"`
}

func (i *Integration) ConfigureInstallation(p *installation.Preparer) error {
	if i == nil || p == nil {
		return errors.New("installation selection requires a native receiving host")
	}
	if !i.installation.CompareAndSwap(nil, p) {
		return errors.New("installation receiving host already configured")
	}
	return nil
}
func (i *Integration) selectedInstallation(ctx context.Context) (*installation.Preparer, installationChoices, error) {
	var cfg installationChoices
	if err := catalogReader(ctx); err != nil {
		return nil, cfg, err
	}
	if i == nil || i.installation.Load() == nil || i.resolver.systemVariable == nil {
		return nil, cfg, errors.New("installation selection is not configured on this node")
	}
	raw, err := i.resolver.systemVariable(memql.ContextWithFreshRead(ctx), InstallationChoicesVariable)
	if err != nil && !memql.IsVariableNotFound(err) {
		return nil, cfg, errors.New("installation choices could not be read")
	}
	if raw == "" {
		overlay, e := i.overlayConfig(ctx)
		if e != nil {
			return nil, cfg, e
		}
		cfg = installationChoices{FormatVersion: 1, InstallationID: overlay.InstallationID, Choices: []installationChoice{}}
	} else if decodeCandidateObject([]byte(raw), &cfg) != nil || cfg.FormatVersion != 1 || !installationIdentifier.MatchString(cfg.InstallationID) || len(cfg.Choices) > 100 {
		return nil, cfg, errors.New("installation choices require explicit bounded operator configuration")
	}
	seen := map[string]bool{}
	for _, c := range cfg.Choices {
		if !strings.HasPrefix(c.CandidateID, "sha256:") || !candidateArtifactDigest(strings.TrimPrefix(c.CandidateID, "sha256:")) || !strings.HasPrefix(c.CatalogDigest, "sha256:") || !candidateArtifactDigest(strings.TrimPrefix(c.CatalogDigest, "sha256:")) || !installationCommit.MatchString(c.OverlayRevision) || len(c.Label) == 0 || len(c.Label) > 200 || seen[c.CandidateID] {
			return nil, cfg, errors.New("installation choices contain invalid or duplicate release identities")
		}
		seen[c.CandidateID] = true
	}
	if i.installationOverlay.Load() != nil {
		proposals, e := i.installationProposals(ctx)
		if e != nil {
			return nil, cfg, e
		}
		for _, p := range proposals {
			if p.State == "merged" && p.InstallationID == cfg.InstallationID && !seen[p.CandidateID] {
				cfg.Choices = append(cfg.Choices, installationChoice{p.CandidateID, p.CatalogDigest, p.OverlayRevision, "Reviewed installation update"})
				seen[p.CandidateID] = true
			}
		}
	}
	return i.installation.Load(), cfg, nil
}
func (i *Integration) handleInstallationStatus(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := catalogReader(ctx); err != nil {
		return nil, err
	}
	if i == nil || i.installation.Load() == nil {
		return nil, errors.New("installation selection is not configured")
	}
	status, err := i.installation.Load().SelectionStatus(ctx)
	if err != nil {
		return nil, err
	}
	return resultNode("installation", status.InstallationID, i.installationStatusView(ctx, status))
}

// Native recovery is independent of optional release inventory and repository
// discovery. A missing grant, broken proposal policy or slow provider cannot
// turn a saved preparation or plan into an unavailable installation.
func (i *Integration) installationStatusView(ctx context.Context, status installation.SelectionStatus) map[string]any {
	out := map[string]any{"installationId": status.InstallationID, "currentRevision": status.CurrentRevision, "choices": []installationChoice{}, "active": status.Active, "latest": status.Latest, "preparation": status.Preparation}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, cfg, err := i.selectedInstallation(ctx)
	if err != nil || cfg.InstallationID != status.InstallationID {
		out["choicesWarning"] = "Reviewed release choices could not be read for this installation."
	} else if cfg.Choices != nil {
		out["choices"] = cfg.Choices
	}
	if i.installationOverlay.Load() == nil {
		return out
	}
	configuration, err := i.overlayConfig(ctx)
	if err != nil || configuration.InstallationID != status.InstallationID {
		out["proposalWarning"] = "Update proposal configuration could not be read for this installation."
		return out
	}
	proposals, err := i.installationProposals(ctx)
	if err != nil {
		out["proposalWarning"] = "Saved update proposals could not be read."
		return out
	}
	out["proposals"] = proposals
	s := &overlayScope{integration: i, configuration: configuration, owner: configuration.OwnerUserID}
	token, err := s.token(ctx, false)
	if err != nil {
		out["proposalWarning"] = "The installation repository connection is unavailable."
		return out
	}
	remote, err := s.client()
	if err != nil {
		out["proposalWarning"] = "The installation repository connection is unavailable."
		return out
	}
	parts := strings.Split(configuration.Repository, "/")
	head, _, err := remote.BranchHead(ctx, token, parts[0], parts[1], configuration.BaseBranch)
	if err != nil || !installationCommit.MatchString(head) {
		out["proposalWarning"] = "The installation repository's current revision could not be read."
		return out
	}
	out["proposalBaseRevision"] = head
	return out
}
func (i *Integration) handleInstallationPrepare(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	p, cfg, err := i.selectedInstallation(ctx)
	if err != nil {
		return nil, err
	}
	request := installation.PrepareSelection{InstallationID: asString(args["installationId"]), RequestID: asString(args["requestId"]), CandidateID: asString(args["candidateId"]), CatalogDigest: asString(args["catalogDigest"]), OverlayRevision: asString(args["overlayRevision"]), ExpectedCurrentRevision: asString(args["expectedCurrentRevision"])}
	matched := false
	for _, c := range cfg.Choices {
		if request.InstallationID == cfg.InstallationID && request.CandidateID == c.CandidateID && request.CatalogDigest == c.CatalogDigest && request.OverlayRevision == c.OverlayRevision {
			matched = true
		}
	}
	if !matched {
		return nil, errors.New("release selection no longer matches the installation's reviewed choices")
	}
	result, err := p.PrepareSelected(ctx, request)
	if err != nil {
		return nil, err
	}
	return resultNode("installation", result.PlanID, result)
}
func (i *Integration) handleInstallationApply(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	p, err := i.receivingSelection(ctx, asString(args["installationId"]))
	if err != nil {
		return nil, err
	}
	result, err := p.StartSelected(ctx, asString(args["installationId"]), asString(args["planId"]))
	if err != nil {
		return nil, err
	}
	return resultNode("installation", result.PlanID, result)
}
func (i *Integration) handleInstallationObserve(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	p, err := i.receivingSelection(ctx, asString(args["installationId"]))
	if err != nil {
		return nil, err
	}
	result, err := p.ObserveSelected(ctx, asString(args["installationId"]), asString(args["planId"]))
	if err != nil {
		return nil, err
	}
	return resultNode("installation", result.PlanID, result)
}

// Recovery does not depend on a publisher or GitHub being reachable. The exact
// receiving installation and persisted operator still gate every native call.
func (i *Integration) receivingSelection(ctx context.Context, installationID string) (*installation.Preparer, error) {
	if err := catalogReader(ctx); err != nil {
		return nil, err
	}
	if i == nil || i.installation.Load() == nil {
		return nil, errors.New("installation selection is not configured")
	}
	p := i.installation.Load()
	status, err := p.SelectionStatus(ctx)
	if err != nil {
		return nil, err
	}
	if status.InstallationID != installationID {
		return nil, errors.New("selection targets another receiving installation")
	}
	return p, nil
}
func (i *Integration) handleInstallationResume(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	p, err := i.receivingSelection(ctx, asString(args["installationId"]))
	if err != nil {
		return nil, err
	}
	r, err := p.ResumeSelected(ctx, asString(args["installationId"]), asString(args["preparationId"]))
	if err != nil {
		return nil, err
	}
	return resultNode("installation", r.PlanID, r)
}
func (i *Integration) handleInstallationCancel(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	p, err := i.receivingSelection(ctx, asString(args["installationId"]))
	if err != nil {
		return nil, err
	}
	r, err := p.CancelSelected(ctx, asString(args["installationId"]), asString(args["planId"]))
	if err != nil {
		return nil, err
	}
	return resultNode("installation", r.PlanID, r)
}
