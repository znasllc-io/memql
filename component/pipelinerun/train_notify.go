package pipelinerun

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelinenotify"
)

// train_notify.go -- how a release train reaches Discord (design record,
// section 9): through the pipeline owner's named channel and the cluster's
// outbound worker, exactly as a notify stage does, so delivery, retries and
// the receipt are the worker's and no webhook value passes through here.
//
// It holds a channel to the same rules as the notify stage's targetOf and
// discordTarget (notify.go) -- the owner's, active, Discord, accepting this
// pipeline, naming a secret by an allowed name -- with the package-level
// pieces those use. It does not resolve the secret's value: the stage does
// that to fail a run step with a precise reason, and a train has no step to
// fail; the worker refuses a value that is not a webhook, and the delivery's
// row says so.

const trainRequestedByPrefix = "pipelines:train:"

// stageTrainMessage stages one train message for channel. dedupeKey is the
// train id and the message's <event>@<headSha> key, so a message staged twice
// -- two replicas, or a write that failed after the staging -- is one
// delivery.
func (d Deps) stageTrainMessage(ctx context.Context, p Pipeline, channel, trainID, dedupeKey string, m pipelinenotify.TrainMessage) error {
	name := strings.TrimSpace(channel)
	ch, err := d.Store.ChannelForOwnerByName(memql.ContextWithFreshRead(ctx), p.OwnerUserID, name)
	if err != nil {
		return fmt.Errorf("pipelines: train channel %s could not be read: %w", name, err)
	}
	if ch == nil || !sameID(ch.OwnerUserID, p.OwnerUserID) {
		return fmt.Errorf("pipelines: no channel named %s belongs to %s's owner", name, p.Repository)
	}
	current, err := d.Store.PipelineByID(memql.ContextWithFreshRead(ctx), p.ID)
	if err != nil {
		return fmt.Errorf("pipelines: train channel %s: the pipeline's channel permission could not be read: %w", name, err)
	}
	switch {
	case current == nil || !sameID(current.OwnerUserID, p.OwnerUserID):
		return fmt.Errorf("pipelines: train channel %s: the pipeline's channel permission is unavailable", name)
	case ch.Status != channelActive:
		return fmt.Errorf("pipelines: train channel %s is %s, not active", name, ch.Status)
	case ch.Kind != channelDiscord:
		return fmt.Errorf("pipelines: train channel %s is %s; trains post to Discord", name, ch.Kind)
	case !slices.ContainsFunc(current.ChannelIDs, func(id string) bool { return sameID(id, ch.ID) }):
		return fmt.Errorf("pipelines: channel %s does not accept pipeline %s; allow it in Deployables > Settings > Channels", name, p.Name)
	}
	secret := strings.TrimSpace(ch.SecretRef)
	if secret == "" || !channelSecretNameRe.MatchString(secret) || strings.HasPrefix(secret, reservedSecretPrefix) {
		return fmt.Errorf("pipelines: train channel %s names no usable webhook secret", name)
	}
	body, err := pipelinenotify.TrainDiscordMessage(m)
	if err != nil {
		return err
	}
	id, err := newNotifyRequestID()
	if err != nil {
		return err
	}
	return d.Store.StageNotification(ctx, NotificationRequest{
		RequestID:    id,
		DedupeKey:    trainID + ":" + dedupeKey,
		RequestedBy:  trainRequestedByPrefix + trainID,
		Medium:       secretTargetMedium,
		TargetSecret: secret,
		Body:         string(body),
	})
}
