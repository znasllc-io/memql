package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

const maxCapabilityInstallDomains = 16
const maxCapabilityInstallChunks = 256

type capabilityInstallReceipt struct {
	DomainID       string `json:"domainId"`
	Chunks         int    `json:"chunks"`
	Ready          int    `json:"ready"`
	Preserved      int    `json:"preserved"`
	Embedded       int    `json:"embedded"`
	OlderPublished int    `json:"olderPublished"`
}

// selectCapabilityChunks accepts IDs only: neither caller text nor a trusted-
// looking source label can manufacture publication authority. Pack domains
// qualify only when registered as operational knowledge required by a tool.
func selectCapabilityChunks(args map[string]any) ([]string, map[string]canonicalSeedChunk, error) {
	for key := range args {
		if key != "domainIds" {
			return nil, nil, fmt.Errorf("capability installation accepts only domainIds")
		}
	}
	var domains []string
	switch values := args["domainIds"].(type) {
	case []string:
		domains = append(domains, values...)
	case []any:
		for _, value := range values {
			text, ok := value.(string)
			if !ok {
				return nil, nil, fmt.Errorf("domainIds must contain only strings")
			}
			domains = append(domains, text)
		}
	default:
		return nil, nil, fmt.Errorf("domainIds must be a nonempty list")
	}
	if len(domains) == 0 || len(domains) > maxCapabilityInstallDomains {
		return nil, nil, fmt.Errorf("select between 1 and %d capability domains", maxCapabilityInstallDomains)
	}
	registered := map[string]bool{}
	for _, domain := range standardDomains {
		if len(domain.RequiredByToolSlugs) > 0 {
			registered[domain.ID] = true
		}
	}
	for _, reg := range RegisteredSeedDomains() {
		if reg.Domain.Source == "appStructure" && len(reg.Domain.RequiredByToolSlugs) > 0 {
			registered[reg.Domain.ID] = true
		}
	}
	selected := map[string]bool{}
	for _, domain := range domains {
		if !registered[domain] {
			return nil, nil, fmt.Errorf("domain %q is not a registered capability corpus", domain)
		}
		if selected[domain] {
			return nil, nil, fmt.Errorf("domainIds contains a duplicate")
		}
		selected[domain] = true
	}
	chunks := map[string]canonicalSeedChunk{}
	counts := map[string]int{}
	for key, seed := range canonicalSeedChunks() {
		if selected[seed.Domain] {
			chunks[key] = seed
			counts[seed.Domain]++
		}
	}
	for _, domain := range domains {
		if counts[domain] == 0 {
			return nil, nil, fmt.Errorf("domain %q has no registered capability corpus", domain)
		}
	}
	if len(chunks) > maxCapabilityInstallChunks {
		return nil, nil, fmt.Errorf("selected capability corpora exceed %d chunks", maxCapabilityInstallChunks)
	}
	sort.Strings(domains)
	return domains, chunks, nil
}

func (i *Integration) installCapabilityCorporaHandler(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	access, _ := auth.AccessFromContext(ctx)
	if !access.IsClusterOwner() || access.UserId == "" {
		return nil, fmt.Errorf("capability installation requires the cluster owner")
	}
	if run, ok := common.RunFromContext(ctx); ok && run.RunId != "" {
		return nil, fmt.Errorf("capability installation requires an explicit operator request outside a work run")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	domains, chunks, err := selectCapabilityChunks(args)
	if err != nil {
		return nil, err
	}
	if i.engine == nil || i.db() == nil || i.embeddingProvider == nil {
		return nil, fmt.Errorf("capability installation requires engine, database and embedder")
	}
	binding, err := memql.CurrentEmbedderBinding(ctx)
	if err != nil {
		return nil, err
	}
	provider, err := i.embeddingProvider(ctx, binding.ProviderRef)
	if err != nil {
		return nil, err
	}
	if provider == nil || binding.Dimensions <= 0 {
		return nil, fmt.Errorf("capability installation embedder dimensions do not match the active binding")
	}
	// Fleet models may report an unknown width. The active binding is the
	// authority in that case; an explicit conflicting width still refuses.
	if declared := provider.Dimensions(); declared > 0 && declared != binding.Dimensions {
		return nil, fmt.Errorf("capability installation embedder dimensions do not match the active binding")
	}
	receipts := map[string]*capabilityInstallReceipt{}
	for _, domain := range domains {
		receipts[domain] = &capabilityInstallReceipt{DomainID: domain}
	}
	keys := make([]string, 0, len(chunks))
	for key, seed := range chunks {
		keys = append(keys, key)
		receipts[seed.Domain].Chunks++
	}
	sort.Strings(keys)
	_, err = workflowhost.Run(ctx, "knowledgeInstallCapabilityCorporaWorkflow", map[string]any{"chunkIds": keys}, workflowhost.Options{Logger: i.Logger, Operations: map[string]workflowhost.Operation{
		"knowledgeInstallCapabilityChunk": func(ctx context.Context, args map[string]any) (any, error) {
			seed, ok := chunks[stringArg(args, "chunkId")]
			if !ok {
				return nil, fmt.Errorf("chunk is outside selected capability corpora")
			}
			embedded, err := i.installCapabilityChunk(ctx, seed, binding, provider)
			if err != nil {
				return nil, err
			}
			if embedded {
				receipts[seed.Domain].Embedded++
			}
			return nil, nil
		},
	}})
	if err != nil {
		return nil, err
	}
	// Receipts describe fresh review/vector state, including a peer review that
	// happened during indexing. They do not claim that installation retires any
	// previously published source revision.
	for _, key := range keys {
		seed := chunks[key]
		row, err := i.seedChunkSnapshot(ctx, key)
		if err != nil {
			return nil, err
		}
		if row == nil || !seed.matches(row) {
			return nil, fmt.Errorf("installed capability chunk changed during installation")
		}
		if row.Payload["validationStatus"] != "validated" || row.Payload["superseded"] == true {
			receipts[seed.Domain].Preserved++
			continue
		}
		indexed, err := i.hasVector(ctx, id.BuildNodeId(conceptKnowledgeDocumentChunk, key), binding.ProviderRef, binding.Dimensions)
		if err != nil {
			return nil, err
		}
		if !indexed {
			return nil, fmt.Errorf("installed capability chunk has no active-embedder vector; retry installation")
		}
		receipts[seed.Domain].Ready++
	}
	current, err := memql.CurrentEmbedderBinding(ctx)
	if err != nil {
		return nil, err
	}
	if current.ProviderRef != binding.ProviderRef || current.Dimensions != binding.Dimensions {
		return nil, fmt.Errorf("active embedder changed during capability installation; retry installation")
	}
	result := make([]capabilityInstallReceipt, 0, len(domains))
	for _, domain := range domains {
		count, err := i.olderCapabilityPublications(ctx, domain, chunks)
		if err != nil {
			return nil, err
		}
		receipts[domain].OlderPublished = count
		result = append(result, *receipts[domain])
	}
	payload, err := json.Marshal(map[string]any{"domains": result, "provider": binding.ProviderRef})
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: "knowledge-capability-install-result", Concept: "integration:knowledge:installCapabilityCorpora", Type: memorynodes.NodeTypeObject, CreatedAt: time.Now().UTC(), Payload: payload}}, nil
}

func (i *Integration) installCapabilityChunk(ctx context.Context, seed canonicalSeedChunk, binding memql.EmbedderBinding, provider memql.EmbeddingAIProvider) (bool, error) {
	row, err := i.seedChunkSnapshot(ctx, seed.ID)
	if err != nil {
		return false, err
	}
	if row != nil {
		if !seed.matches(row) {
			return false, fmt.Errorf("stored capability chunk does not match registered content")
		}
		status, decided := row.Payload["validationStatus"]
		if row.Payload["superseded"] == true || (decided && status != "unvalidated" && status != "validated") {
			return false, nil
		}
	}
	indexed, err := i.hasVector(ctx, id.BuildNodeId(conceptKnowledgeDocumentChunk, seed.ID), binding.ProviderRef, binding.Dimensions)
	if err != nil {
		return false, err
	}
	var vector []float32
	if !indexed {
		vector, err = provider.Embed(ctx, seed.Text)
		if err != nil {
			return false, fmt.Errorf("embed registered capability chunk: %w", err)
		}
		// Validate before publication, against both this installation's selected
		// width and the current binding. Storage checks alone happen too late.
		if len(vector) != binding.Dimensions {
			return false, fmt.Errorf("embedding model returned %d dimensions, expected %d", len(vector), binding.Dimensions)
		}
		if err = memql.ValidateEmbeddingVector(ctx, binding.ProviderRef, binding.Dimensions, vector); err != nil {
			return false, err
		}
	}
	query := fmt.Sprintf(`mutation createDocumentChunk(chunkId:%s,domainId:%s,text:%s,source:"appStructure",sourceRef:%s,seq:%d,tokenCount:%d)`, quoteString(seed.ID), quoteString(seed.Domain), quoteString(seed.Text), quoteString(seed.SourceRef), seed.Seq, approxTokens(seed.Text))
	if err = i.writeCanonicalSeed(ctx, seed.ID, query); err != nil {
		return false, err
	}
	if !indexed {
		if err = i.storeVector(ctx, binding.ProviderRef, seed.ID, conceptKnowledgeDocumentChunk, vector); err != nil {
			return false, err
		}
	}
	return !indexed, nil
}

// staged-data: MUST-NOT-GATE -- owner-only publication bookkeeping returns a
// count, never content. It observes latest review state before counting older
// published sources; ordinary retrieval retains its own visibility gates.
func (i *Integration) olderCapabilityPublications(ctx context.Context, domain string, chunks map[string]canonicalSeedChunk) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	refs, keys := map[string]bool{}, []string{}
	for key, seed := range chunks {
		if seed.Domain == domain {
			refs[seed.SourceRef] = true
			keys = append(keys, id.BuildNodeId(conceptKnowledgeDocumentChunk, key))
		}
	}
	sources := make([]string, 0, len(refs))
	for ref := range refs {
		sources = append(sources, ref)
	}
	sourceJSON, _ := json.Marshal(sources)
	keyJSON, _ := json.Marshal(keys)
	var count int
	err := i.db().QueryRowContext(ctx, `SELECT count(*) FROM (
 SELECT DISTINCT ON (id) id,payload FROM "MemoryNodes" WHERE concept='v1:knowledge:documentChunk' ORDER BY id,"createdAt" DESC
 ) latest WHERE payload->>'domainId'=$1 AND payload->>'source'='appStructure'
 AND payload->>'sourceRef' IN (SELECT jsonb_array_elements_text($2::jsonb))
 AND payload->>'validationStatus'='validated' AND COALESCE(payload->>'superseded','false')!='true'
 AND id NOT IN (SELECT jsonb_array_elements_text($3::jsonb))`, domain, string(sourceJSON), string(keyJSON)).Scan(&count)
	return count, err
}
