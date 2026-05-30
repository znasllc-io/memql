package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGroundingContext_IsEmpty(t *testing.T) {
	assert.True(t, GroundingContext{}.IsEmpty())
	assert.True(t, GroundingContext{Facts: []GroundingFact{{Text: "   "}}}.IsEmpty())
	assert.False(t, GroundingContext{Facts: []GroundingFact{{Text: "hi"}}}.IsEmpty())
}

func TestBuildGroundingMessage_Empty(t *testing.T) {
	assert.Equal(t, "", BuildGroundingMessage(GroundingContext{}))
	assert.Equal(t, "", BuildGroundingMessage(GroundingContext{Facts: []GroundingFact{{Text: " "}}}))
}

func TestBuildGroundingMessage_NumbersAndLabels(t *testing.T) {
	g := GroundingContext{Facts: []GroundingFact{
		{Text: "Plan A costs $10/mo.", DomainName: "Pricing", Source: "pricing.md"},
		{Text: "Plan B costs $20/mo.", DomainName: "Pricing"},
		{Text: "No attribution fact."},
	}}
	msg := BuildGroundingMessage(g)

	assert.Contains(t, msg, "Relevant context for the current topic")
	assert.Contains(t, msg, "[1] [Pricing / pricing.md] Plan A costs $10/mo.")
	assert.Contains(t, msg, "[2] [Pricing] Plan B costs $20/mo.")
	assert.Contains(t, msg, "[3] No attribution fact.")
}

func TestBuildGroundingMessage_SkipsBlankFactsAndRenumbers(t *testing.T) {
	g := GroundingContext{Facts: []GroundingFact{
		{Text: "first"},
		{Text: "   "}, // skipped, does not consume an index
		{Text: "second"},
	}}
	msg := BuildGroundingMessage(g)
	assert.Contains(t, msg, "[1] first")
	assert.Contains(t, msg, "[2] second")
	assert.NotContains(t, msg, "[3]")
}

func TestBuildGroundingItems_Empty(t *testing.T) {
	assert.Nil(t, BuildGroundingItems(GroundingContext{}))
}

func TestBuildGroundingItems_SystemMessage(t *testing.T) {
	g := GroundingContext{Facts: []GroundingFact{{Text: "fact one", DomainName: "Docs"}}}
	items := BuildGroundingItems(g)

	require.Len(t, items, 1)
	assert.Equal(t, "message", items[0].Type)
	assert.Equal(t, "system", items[0].Role)
	assert.Equal(t, BuildGroundingMessage(g), items[0].Text)
	assert.Contains(t, items[0].Text, "[1] [Docs] fact one")
}
