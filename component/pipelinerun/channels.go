package pipelinerun

import (
	"context"
	"fmt"
	"slices"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
)

// Channel names are stable: the manifest refers to the name, and configuration
// is an idempotent upsert of that owner's name. Changing a name means creating a
// different channel and explicitly allowing its pipelines. This avoids a rename
// moving delivery authority between destinations.
type ChannelSaveRequest struct {
	Name, Kind, SecretRef string
	Recipients            []string
}

func channelGateKey(owner, name string) string {
	return "pipelines.channel:" + bareID(owner) + ":" + name
}

func channelAnswer(ch Channel) map[string]any {
	return map[string]any{"channelId": bareID(ch.ID), "name": ch.Name, "kind": ch.Kind, "status": ch.Status}
}

func (i *Integration) handleChannelSave(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	var recipients []string
	switch raw := args["recipients"].(type) {
	case nil:
	case []string:
		recipients = slices.Clone(raw)
	case []any:
		for _, item := range raw {
			value, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("pipelines: recipients must be a list of addresses")
			}
			recipients = append(recipients, value)
		}
	default:
		return nil, fmt.Errorf("pipelines: recipients must be a list of addresses")
	}
	ch, err := i.SaveChannel(handlerContext(ctx), ChannelSaveRequest{Name: stringArg(args, "name"), Kind: stringArg(args, "kind"), SecretRef: stringArg(args, "secretRef"), Recipients: recipients})
	if err != nil {
		return nil, err
	}
	return resultNode(channelAnswer(ch)), nil
}

// SaveChannel changes configuration only. It neither resolves a secret nor
// sends a message. Pipeline grants are independent, bounded row mutations.
func (i *Integration) SaveChannel(ctx context.Context, req ChannelSaveRequest) (Channel, error) {
	owner, err := personFrom(ctx)
	if err != nil {
		return Channel{}, err
	}
	d := i.snapshot()
	if d.Store == nil {
		return Channel{}, errNoStore
	}
	ch := Channel{OwnerUserID: owner, Name: strings.TrimSpace(req.Name), Kind: strings.TrimSpace(req.Kind), SecretRef: strings.TrimSpace(req.SecretRef), Status: channelActive, Recipients: []string{}}
	if !pipelines.ValidName(ch.Name) {
		return Channel{}, fmt.Errorf("pipelines: channel name must use lower-case letters, digits and hyphens, starting with a letter or digit, at most 40 characters")
	}
	switch ch.Kind {
	case channelDiscord:
		if !channelSecretNameRe.MatchString(ch.SecretRef) || strings.HasPrefix(ch.SecretRef, reservedSecretPrefix) || len(req.Recipients) != 0 {
			return Channel{}, fmt.Errorf("pipelines: a Discord channel needs a non-reserved secret name and no recipients")
		}
	case channelEmail:
		if ch.SecretRef != "" || len(req.Recipients) == 0 || len(req.Recipients) > maxChannelRecipients {
			return Channel{}, fmt.Errorf("pipelines: an email channel needs 1 to %d addresses and no secret", maxChannelRecipients)
		}
		seen := map[string]bool{}
		for n, raw := range req.Recipients {
			address := strings.TrimSpace(raw)
			if !bareMailbox(address) {
				return Channel{}, fmt.Errorf("pipelines: recipient %d must be a bare email address", n+1)
			}
			key := strings.ToLower(address)
			if !seen[key] {
				seen[key] = true
				ch.Recipients = append(ch.Recipients, address)
			}
		}
	default:
		return Channel{}, fmt.Errorf("pipelines: channel kind must be discord or email")
	}
	err = d.gate(ctx, channelGateKey(owner, ch.Name), func(gctx context.Context) error {
		existing, err := d.Store.ChannelForOwnerByName(memql.ContextWithFreshRead(gctx), owner, ch.Name)
		if err != nil {
			return err
		}
		if existing == nil {
			ch.ID = deriveID("pipelines.channel", bareID(owner)+"\x00"+ch.Name)
			return d.Store.CreateChannel(gctx, ch)
		}
		if !sameID(existing.OwnerUserID, owner) {
			return ErrNotOwner
		}
		ch.ID, ch.AccountID = existing.ID, existing.AccountID
		return d.Store.UpdateChannel(gctx, owner, ch.ID, ChannelPatch{Kind: &ch.Kind, SecretRef: &ch.SecretRef, Recipients: &ch.Recipients, Status: &ch.Status})
	})
	return ch, err
}

func (i *Integration) handleChannelArchive(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ch, err := i.ArchiveChannel(handlerContext(ctx), stringArg(args, "name"))
	if err != nil {
		return nil, err
	}
	return resultNode(channelAnswer(ch)), nil
}

// ArchiveChannel preserves the row and existing run history. Saving the same
// name explicitly resumes it. Already staged outbound messages retain their
// delivery receipts; archiving prevents subsequent notification admission.
func (i *Integration) ArchiveChannel(ctx context.Context, name string) (Channel, error) {
	owner, err := personFrom(ctx)
	if err != nil {
		return Channel{}, err
	}
	d := i.snapshot()
	if d.Store == nil {
		return Channel{}, errNoStore
	}
	name = strings.TrimSpace(name)
	if !pipelines.ValidName(name) {
		return Channel{}, fmt.Errorf("pipelines: invalid channel name")
	}
	var ch Channel
	err = d.gate(ctx, channelGateKey(owner, name), func(gctx context.Context) error {
		existing, err := d.Store.ChannelForOwnerByName(memql.ContextWithFreshRead(gctx), owner, name)
		if err != nil {
			return err
		}
		if existing == nil {
			return fmt.Errorf("pipelines: you have no channel %q", name)
		}
		if !sameID(existing.OwnerUserID, owner) {
			return ErrNotOwner
		}
		ch = *existing
		ch.Status = channelArchived
		return d.Store.UpdateChannel(gctx, owner, ch.ID, ChannelPatch{Status: &ch.Status})
	})
	return ch, err
}

func (i *Integration) handleChannelAllow(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	allowed, ok := args["allowed"].(bool)
	if !ok {
		return nil, fmt.Errorf("pipelines: allowed must be true or false")
	}
	ids, err := i.AllowChannel(handlerContext(ctx), stringArg(args, "pipelineId"), stringArg(args, "name"), allowed)
	if err != nil {
		return nil, err
	}
	return resultNode(map[string]any{"pipelineId": bareID(stringArg(args, "pipelineId")), "channelIds": ids}), nil
}

// AllowChannel changes one pipeline under its existing repository gate, with
// fresh owner-scoped reads. No gate is nested; concurrent connect, grants and
// driver timing writes therefore retain each other's fields. It spends no
// compute and sends nothing.
func (i *Integration) AllowChannel(ctx context.Context, pipelineID, name string, allowed bool) ([]string, error) {
	owner, err := personFrom(ctx)
	if err != nil {
		return nil, err
	}
	d := i.snapshot()
	if d.Store == nil {
		return nil, errNoStore
	}
	pipelineID, name = strings.TrimSpace(pipelineID), strings.TrimSpace(name)
	if pipelineID == "" || !pipelines.ValidName(name) {
		return nil, fmt.Errorf("pipelines: a pipeline and valid channel name are required")
	}
	p, err := d.Store.PipelineForOwner(memql.ContextWithFreshRead(ctx), pipelineID)
	if err != nil {
		return nil, err
	}
	if p == nil || !sameID(p.OwnerUserID, owner) {
		return nil, ErrNotOwner
	}
	var ids []string
	err = d.gate(ctx, RepositoryGateKey(p.Repository), func(gctx context.Context) error {
		fresh := memql.ContextWithFreshRead(gctx)
		current, err := d.Store.PipelineForOwner(fresh, pipelineID)
		if err != nil {
			return err
		}
		if current == nil || !sameID(current.OwnerUserID, owner) {
			return ErrNotOwner
		}
		if current.Repository != p.Repository {
			return fmt.Errorf("pipelines: the repository changed; refresh before changing channel access")
		}
		ch, err := d.Store.ChannelForOwnerByName(fresh, owner, name)
		if err != nil {
			return err
		}
		if ch == nil {
			return fmt.Errorf("pipelines: you have no channel %q", name)
		}
		if !sameID(ch.OwnerUserID, owner) {
			return ErrNotOwner
		}
		if allowed && ch.Status != channelActive {
			return fmt.Errorf("pipelines: save the archived channel before allowing it")
		}
		ids = make([]string, 0, len(current.ChannelIDs)+1)
		for _, id := range current.ChannelIDs {
			if !sameID(id, ch.ID) {
				ids = append(ids, id)
			}
		}
		if allowed {
			ids = append(ids, bareID(ch.ID))
		}
		slices.Sort(ids)
		ids = slices.Compact(ids)
		return d.Store.UpdatePipeline(gctx, owner, current.ID, PipelinePatch{ChannelIDs: &ids})
	})
	return ids, err
}
