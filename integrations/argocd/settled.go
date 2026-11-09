package argocd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// OperationSettled reports whether the live Application holds no operation this
// intent requested that is still waiting or running. A journal asks it before
// it stops owning an intent whose completion can no longer be proven: a write
// still being acted on would otherwise finish with no owner.
//
// It deliberately does not require the Application to sit at the intent. An
// operator may have moved it on by hand, or the intent's write may never have
// landed; neither leaves an operation of this intent to finish. Ownership is
// read from the operation's info entry, which every Apply writes with the
// intent's digest, without the exact sync-field match observe demands, so a
// look-alike carrying this digest counts as this intent's and the answer errs
// towards "not settled". An owned operation is settled only at Argo's terminal
// phases, Succeeded, Failed or Error; Running, Terminating or no phase yet is
// not.
//
// An Application under the same name with a different UID replaced the one the
// intent wrote, and nothing of the intent survives on it. A read failure,
// including a deleted Application, is an error: an absent operation and an
// unreadable one look the same from here. It performs no write and grants
// nothing.
func (c *Client) OperationSettled(ctx context.Context, intent Intent) (bool, error) {
	_, _, digest, err := intent.validate()
	if err != nil {
		return false, err
	}
	if c == nil || c.api == nil {
		return false, errors.New("ArgoCD cluster API is unavailable")
	}
	path, _ := intent.Target.path()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body, err := c.api.Do(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return false, fmt.Errorf("read ArgoCD Application: %w", err)
	}
	obj, err := decodeObject(body)
	if err != nil {
		return false, err
	}
	meta := object(obj, "metadata")
	if stringAt(obj, "apiVersion") != "argoproj.io/v1alpha1" || stringAt(obj, "kind") != "Application" ||
		stringAt(meta, "namespace") != intent.Target.Namespace || stringAt(meta, "name") != intent.Target.Name || stringAt(meta, "uid") == "" {
		return false, errors.New("ArgoCD returned a different object for the intent's Application")
	}
	if stringAt(meta, "uid") != intent.Target.UID {
		return true, nil
	}
	if carriesIntent(object(obj, "operation"), digest) {
		return false, nil
	}
	state := object(object(obj, "status"), "operationState")
	if carriesIntent(object(state, "operation"), digest) {
		switch stringAt(state, "phase") {
		case "Succeeded", "Failed", "Error":
		default:
			return false, nil
		}
	}
	return true, nil
}

// carriesIntent reports whether an operation names the intent's digest in its
// info, the entry Apply writes beside the sync request.
func carriesIntent(op map[string]any, digest string) bool {
	info, _ := op["info"].([]any)
	for _, raw := range info {
		entry, _ := raw.(map[string]any)
		if stringAt(entry, "name") == operationInfoName && stringAt(entry, "value") == digest {
			return true
		}
	}
	return false
}
