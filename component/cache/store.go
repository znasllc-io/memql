package cache

import (
	"context"
	"fmt"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

type MemoryNodeRepository interface {
	CreateMemoryNode(ctx context.Context, node *memoryNodes.MemoryNode) error
	ListRecentMemoryNodes(ctx context.Context, createdBy string, limit int) ([]memoryNodes.MemoryNode, error)
	LoadMemoryNode(ctx context.Context, id string) (*memoryNodes.MemoryNode, error)
	FindMemoryNodes(ctx context.Context, params memoryNodes.QueryParams) ([]memoryNodes.MemoryNode, error)
}

type CachedMemoryNodeStore struct {
	repo  MemoryNodeRepository
	cache *Cache
}

func NewCachedMemoryNodeStore(repo MemoryNodeRepository, cache *Cache) (*CachedMemoryNodeStore, error) {
	if repo == nil {
		return nil, fmt.Errorf("memory node repository is nil")
	}

	if cache == nil {
		return nil, fmt.Errorf("memory node cache is nil")
	}

	return &CachedMemoryNodeStore{repo: repo, cache: cache}, nil
}

func (s *CachedMemoryNodeStore) InsertMemoryNode(ctx context.Context, node *memoryNodes.MemoryNode) error {
	if s == nil {
		return fmt.Errorf("cached memory node store is nil")
	}

	if err := s.repo.CreateMemoryNode(ctx, node); err != nil {
		return err
	}

	if s.cache != nil && node != nil {
		s.cache.Set(node)
		if node.CreatedBy != "" {
			s.cache.InvalidateListsFor(node.CreatedBy)
		}
	}

	return nil
}

func (s *CachedMemoryNodeStore) ListMemoryNodes(ctx context.Context, createdBy string, limit int) ([]memoryNodes.MemoryNode, error) {
	if s == nil {
		return nil, fmt.Errorf("cached memory node store is nil")
	}

	if s.cache != nil && limit > 0 {
		if nodes, ok := s.cache.GetList(createdBy, limit); ok {
			return nodes, nil
		}
	}

	nodes, err := s.repo.ListRecentMemoryNodes(ctx, createdBy, limit)
	if err != nil {
		return nil, err
	}

	if s.cache != nil && limit > 0 {
		s.cache.SetList(createdBy, limit, nodes)
	}

	return nodes, nil
}

func (s *CachedMemoryNodeStore) GetMemoryNode(ctx context.Context, id string) (*memoryNodes.MemoryNode, error) {
	if s == nil {
		return nil, fmt.Errorf("cached memory node store is nil")
	}

	if s.cache != nil {
		if node, ok := s.cache.Get(id); ok {
			return node, nil
		}
	}

	node, err := s.repo.LoadMemoryNode(ctx, id)
	if err != nil {
		return nil, err
	}

	if s.cache != nil && node != nil {
		s.cache.Set(node)
	}

	return node, nil
}

func (s *CachedMemoryNodeStore) QueryMemoryNodes(ctx context.Context, params memoryNodes.QueryParams) ([]memoryNodes.MemoryNode, error) {
	if s == nil {
		return nil, fmt.Errorf("cached memory node store is nil")
	}
	return s.repo.FindMemoryNodes(ctx, params)
}
