package releasecatalog

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

type integration struct{ reader *Reader }

func init() {
	memql.RegisterPlugin("releaseDiscovery", func(p memql.PluginContext) (memql.IntegrationProvider, error) {
		return &integration{reader: New(p.ResolveSystemVariable, p.ResolveSystemSecret)}, nil
	})
}

func (i *integration) IntegrationName() string { return "releaseDiscovery" }
func (i *integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{Name: "sources", Description: "Read configured release publishers without credentials.", Handler: i.sources},
		{Name: "discover", Description: "Read one bounded page of independently verified remote releases.", Handler: i.discover},
		{Name: "get", Description: "Reverify one exact release under current publisher trust; grants no installation authority.", Handler: i.get},
	}
}

func result(value any) ([]memorynodes.MemoryNode, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("release discovery result could not be encoded")
	}
	return []memorynodes.MemoryNode{{ID: "releaseDiscovery:result", Concept: "integration:releaseDiscovery:result", Type: memorynodes.NodeTypeObject, CreatedAt: time.Now().UTC(), Payload: body}}, nil
}

func (i *integration) sources(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	sources, err := i.reader.Sources(ctx)
	if err != nil {
		return nil, err
	}
	return result(map[string]any{"sources": sources})
}

func (i *integration) discover(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	sourceID, _ := args["sourceId"].(string)
	cursor := ""
	if raw, found := args["cursor"]; found && raw != nil {
		var ok bool
		cursor, ok = raw.(string)
		if !ok {
			return nil, errors.New("release cursor must be a string")
		}
	}
	limit := maxPage
	if raw, found := args["limit"]; found && raw != nil {
		body, err := json.Marshal(raw)
		if err != nil || json.Unmarshal(body, &limit) != nil {
			return nil, errors.New("release page limit must be an integer")
		}
	}
	page, err := i.reader.List(ctx, sourceID, cursor, limit)
	if err != nil {
		return nil, err
	}
	return result(page)
}

func (i *integration) get(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	sourceID, _ := args["sourceId"].(string)
	candidateID, _ := args["candidateId"].(string)
	digest, _ := args["catalogDigest"].(string)
	verified, err := i.reader.Get(ctx, sourceID, candidateID, digest)
	if err != nil {
		return nil, err
	}
	release, err := verified.Release()
	if err != nil {
		return nil, err
	}
	return result(map[string]any{"sourceId": sourceID, "candidateId": release.CandidateID, "catalogDigest": verified.Digest(), "release": release})
}
