package memoryNodes

import (
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

const (
	MemoryNodesTable       = "MemoryNodes"
	SecretMemoryNodesTable = "SecretMemoryNodes"
)

type (
	MemoryNode struct {
		bun.BaseModel `bun:"table:MemoryNodes,alias:mn"`
		Partition     string          `bun:",pk,notnull,default:'default'" json:"partition"`
		ID            string          `bun:",pk" json:"id"`
		CreatedAt     time.Time       `bun:"\"createdAt\",pk,type:TIMESTAMPTZ" json:"createdAt"`
		CreatedBy     string          `bun:"\"createdBy\",notnull" json:"createdBy"`
		Concept       string          `bun:",notnull" json:"concept"`
		Type          string          `bun:"type,notnull" json:"type"`
		Schema        json.RawMessage `bun:"type:JSONB,notnull" json:"schema"`
		Payload       json.RawMessage `bun:"type:JSONB,notnull" json:"payload"`
		Metadata      json.RawMessage `bun:"type:JSONB,notnull,default:'{}'" json:"metadata,omitempty"`
	}

	SecretMemoryNode struct {
		bun.BaseModel `bun:"table:SecretMemoryNodes,alias:smn"`
		Partition     string          `bun:",pk,notnull,default:'default'" json:"partition"`
		ID            string          `bun:",pk" json:"id"`
		CreatedAt     time.Time       `bun:"\"createdAt\",pk,type:TIMESTAMPTZ" json:"createdAt"`
		CreatedBy     string          `bun:"\"createdBy\",notnull" json:"createdBy"`
		Concept       string          `bun:",notnull" json:"concept"`
		Type          string          `bun:"type,notnull" json:"type"`
		Schema        json.RawMessage `bun:"type:JSONB,notnull" json:"schema"`
		Payload       json.RawMessage `bun:"type:JSONB,notnull" json:"payload"`
		Metadata      json.RawMessage `bun:"type:JSONB,notnull,default:'{}'" json:"metadata,omitempty"`
	}
)
