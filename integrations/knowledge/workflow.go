package knowledge

import (
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
)

func knowledgeWorkflowCapabilities() []memql.IntegrationCapability {
	return workflowhost.ScopedCapabilities(map[string]workflowhost.Operation{
		"knowledgeCatalogDomainExists": nil,
		"knowledgeCreateCatalogDomain": nil,
		"knowledgeSeedCorpusExists":    nil,
		"knowledgePurgeSeedCorpus":     nil,
		"knowledgeIngestSeedCorpus":    nil,
		"knowledgePurgeRetiredCorpus":  nil,
		"knowledgeSeedSelectedDomain":  nil,

		"knowledgeBridgeExists": nil, "knowledgeBridgeInputs": nil, "knowledgeStampBridge": nil,
		"knowledgeGenerateSeedChunks":  nil,
		"knowledgeResolveSeedEmbedder": nil,
		"knowledgeStoreSeedChunk":      nil,
		"knowledgeStampSeed":           nil,
		"knowledgeFetchSeedArticle":    nil,
		"knowledgeSplitSeedText":       nil,

		"knowledgeWriteIndexedChunk":      nil,
		"knowledgeEmbedSelectedChunk":     nil,
		"knowledgeRollupSelectedDocument": nil,
		"knowledgeEmbeddingReceipts":      nil,
		"knowledgeTrainingCorpus":         nil,
		"knowledgeTrainingSpecialist":     nil,
		"knowledgeTrainingTurn":           nil,
		"knowledgeTrainingReset":          nil,
	})
}
