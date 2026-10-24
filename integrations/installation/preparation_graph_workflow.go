package installation

import (
	"context"
	"errors"
)

func (s *preparationWorkflowScope) sourceRequirementsOperation(ctx context.Context, args map[string]any) (any, error) {
	root, err := s.record.Scope.sourceKey(args)
	if err != nil {
		return nil, err
	}
	requirements, err := s.host.journal.sourceRequirements(ctx, s.request.InstallationID, s.record.ID, s.host.digest, root, s.host.files)
	if err != nil {
		return nil, err
	}
	record, err := s.host.journal.get(ctx, s.request.InstallationID, s.record.ID)
	if err != nil {
		return nil, err
	}
	s.record = record
	out := make([]any, 0, len(requirements))
	for _, requirement := range requirements {
		out = append(out, map[string]any{"key": requirement.Key})
	}
	return out, nil
}

// A DSL value can select only a dependency already derived from this exact
// application's verified root. No argument can supply clone or render inputs.
func (s *preparationWorkflowScope) dependencyOperationKey(ctx context.Context, args map[string]any) (string, error) {
	key, ok := args["key"].(string)
	if !ok || !captureStepKey.MatchString(key) {
		return "", errors.New("source dependency requires an exact native capture key")
	}
	rootArgs := map[string]any{}
	for name, value := range args {
		switch name {
		case "key":
		case "application", "role":
			rootArgs[name] = value
		default:
			return "", errors.New("source dependency operation accepts no supplied evidence")
		}
	}
	root, err := s.record.Scope.sourceKey(rootArgs)
	if err != nil {
		return "", err
	}
	record, err := s.host.journal.get(ctx, s.request.InstallationID, s.record.ID)
	if err != nil {
		return "", err
	}
	entry, found := record.Captures[key]
	if !found || entry.Parent != root || entry.Spec == nil {
		return "", errors.New("source dependency is outside the selected application and role")
	}
	if _, err = preparationCaptureSpec(record, key); err != nil {
		return "", err
	}
	s.record = record
	return key, nil
}
func (s *preparationWorkflowScope) captureDependency(ctx context.Context, args map[string]any) (any, error) {
	key, err := s.dependencyOperationKey(ctx, args)
	if err != nil {
		return nil, err
	}
	record, err := s.host.journal.capture(ctx, s.request.InstallationID, s.record.ID, s.host.digest, key, s.host.executor, s.configuration.pullCredential)
	if err == nil {
		s.record = record
	}
	return nil, err
}
func (s *preparationWorkflowScope) verifyDependency(ctx context.Context, args map[string]any) (any, error) {
	key, err := s.dependencyOperationKey(ctx, args)
	if err != nil {
		return nil, err
	}
	proof, err := s.host.journal.verifyCapture(ctx, s.request.InstallationID, s.record.ID, s.host.digest, key, s.host.files)
	if err != nil {
		return nil, err
	}
	if proof.fragment.Digest() == "" || proof.source.Digest() != "" {
		return nil, errors.New("source dependency did not produce a partial verified fragment")
	}
	return nil, nil
}
func (s *preparationWorkflowScope) acknowledgeDependency(ctx context.Context, args map[string]any) (any, error) {
	key, err := s.dependencyOperationKey(ctx, args)
	if err != nil {
		return nil, err
	}
	record, err := s.host.journal.acknowledgeCapture(ctx, s.request.InstallationID, s.record.ID, s.host.digest, key, s.host.executor)
	if err == nil {
		s.record = record
	}
	return nil, err
}
func (s *preparationWorkflowScope) verifyGraph(ctx context.Context, args map[string]any) (any, error) {
	root, err := s.record.Scope.sourceKey(args)
	if err != nil {
		return nil, err
	}
	delete(s.sources, root)
	proof, err := s.host.journal.verifySourceGraph(ctx, s.request.InstallationID, s.record.ID, s.host.digest, root, s.host.files)
	if err != nil {
		return nil, err
	}
	if proof.source.Digest() == "" || proof.fragment.Digest() != "" {
		return nil, errors.New("source graph is incomplete")
	}
	record, err := s.host.journal.get(ctx, s.request.InstallationID, s.record.ID)
	if err != nil {
		return nil, err
	}
	s.record = record
	s.sources[root] = proof
	return nil, nil
}
