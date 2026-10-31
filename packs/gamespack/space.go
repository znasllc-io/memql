package gamespack

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
)

// space.go -- the cluster's embedder space, as this pack sees it.
//
// THE BINDING DECIDES, NEVER THIS PACK. A vector is only comparable with
// vectors of the same model and width, so every read and every write goes
// through the cluster's embedder binding (memql.CurrentEmbedderBinding) and
// its own vector table (memql.EmbeddingVectorTable). With nothing bound,
// reads degrade to no relevance and writes leave rows pending; neither picks
// a model for the owner. A binding that cannot be READ is not "nothing
// bound": it is logged at Warn and answers the same way, so no write fails on
// it.
//
// READS EMBED DIRECTLY, WRITES DISPATCH. The query vector is needed in Go, to
// bind into a statement, so a read resolves the provider and embeds itself,
// as integrations/harnessrecall does. A write goes through
// integration.embedding.store (builtin gamesEmbedStore), the one embedding
// write path, which caches and creates the table.

// embeddingSpace is the seam the recall, lookup and pending paths use.
type embeddingSpace interface {
	// table names the current space's vector table when it exists, or ""
	// when nothing is bound or nothing was ever stored in it.
	table(ctx context.Context) (string, error)
	// embedQuery embeds a read's text on the caller's own lane.
	embedQuery(ctx context.Context, text string) ([]float32, error)
	// store embeds text and writes it as nodeID's vector.
	store(ctx context.Context, nodeID, concept, text string) error
}

// clusterSpace is the production embeddingSpace.
type clusterSpace struct {
	resolve func(ctx context.Context, name string) (memql.EmbeddingAIProvider, error)
	rows    rowStore
	db      func() *sql.DB
	log     *slog.Logger
}

// logger never returns nil.
func (c clusterSpace) logger() *slog.Logger {
	if c.log == nil {
		return slog.Default()
	}
	return c.log
}

func (c clusterSpace) table(ctx context.Context) (string, error) {
	binding, err := memql.CurrentEmbedderBinding(ctx)
	if err != nil {
		// Nothing bound is the ordinary state of a fresh install and says
		// nothing. A binding that could not be read is different: the space
		// is not known to be empty, only unavailable to this call, and an
		// operator should be able to find out why. Either way the answer is
		// "no table" with no error, so a write never fails on it.
		if !errors.Is(err, memql.ErrNoEmbedderBound) {
			c.logger().Warn("games: the embedder binding could not be read; this call treats the embedder space as unavailable", "error", err)
		}
		return "", nil
	}
	table, err := memql.EmbeddingVectorTable(binding.ProviderRef, binding.Dimensions)
	if err != nil {
		return "", err
	}
	db := c.db()
	if db == nil {
		return "", fmt.Errorf("games: no database handle on this node")
	}
	tables, err := memql.EmbeddingVectorTables(ctx, db)
	if err != nil {
		return "", err
	}
	for _, t := range tables {
		if t == table {
			return table, nil
		}
	}
	return "", nil
}

func (c clusterSpace) embedQuery(ctx context.Context, text string) ([]float32, error) {
	binding, err := memql.CurrentEmbedderBinding(ctx)
	if err != nil {
		return nil, err
	}
	if c.resolve == nil {
		return nil, fmt.Errorf("games: no embedding resolver on this node")
	}
	provider, err := c.resolve(ctx, binding.ProviderRef)
	if err != nil {
		return nil, err
	}
	vec, err := provider.Embed(ctx, text)
	if err != nil {
		return nil, err
	}
	if err := memql.ValidateEmbeddingVector(ctx, binding.ProviderRef, provider.Dimensions(), vec); err != nil {
		return nil, err
	}
	return vec, nil
}

func (c clusterSpace) store(ctx context.Context, nodeID, concept, text string) error {
	if c.rows == nil {
		return fmt.Errorf("games: no store on this node")
	}
	return c.rows.embedStore(ctx, nodeID, concept, text)
}

// vectorTableName is the shape of the name a vector table is given, the same as
// the pattern component/memql holds the names it reads from the catalogue to
// (embeddingTableName).
var vectorTableName = regexp.MustCompile(`^node_vectors(?:_[0-9]+(?:_[0-9a-f]{16})?)?$`)

// isVectorTable reports whether name is the name of a vector table.
//
// IT IS THE ONE DEFINITION OF WHAT SUCH A NAME LOOKS LIKE. An identifier cannot be
// a parameter, so a statement that names a vector table builds its text with the
// name in it, and it takes a name that passes this and nothing else, whichever way
// the name reached it: a name that fails is not put into a statement. Recall's join
// and forget's deletes both ask here, and a statement that is added should too.
func isVectorTable(name string) bool { return vectorTableName.MatchString(name) }

// vectorLiteral formats a vector as a pgvector literal: [0.1,0.2,...].
func vectorLiteral(vec []float32) string {
	parts := make([]string, len(vec))
	for i, v := range vec {
		parts[i] = strconv.FormatFloat(float64(v), 'g', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
