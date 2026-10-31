package gamespack

import "github.com/znasllc-io/memql/component/memql"

// Capabilities implements memql.IntegrationProvider. The DSL declarations in
// dsl/builtins.memql are the contract; ArgsSchema here is advisory, and
// capabilities_test.go holds the two lists equal.
func (p *Provider) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{Name: "recordEpisodes", Description: "Record episodes and embed their summaries on the background lane.", Handler: p.recordEpisodes,
			ArgsSchema: map[string]string{"episodes": "[]object (required)"}},
		{Name: "embedPending", Description: "Embed the caller's episodes and skills that have no vector in the current embedder space.", Handler: p.embedPending,
			ArgsSchema: map[string]string{"limit": "int (optional)"}},
		{Name: "recall", Description: "Rank one character's episodes by relevance, recency and importance, then append preferences, open promises and places.", Handler: p.recall, PreserveOrder: true,
			ArgsSchema: map[string]string{"characterId": "string (required)", "playerId": "string", "worldId": "string (required)", "text": "string (required)", "k": "int"}},
		{Name: "forgetPlayer", Description: "Delete every row the caller holds about one player.", Handler: p.forgetPlayer,
			ArgsSchema: map[string]string{"playerId": "string (required)"}},
		{Name: "recordTrace", Description: "Record an executed plan and learn a skill when two successful traces agree.", Handler: p.recordTrace,
			ArgsSchema: map[string]string{"traceId": "string (required)", "goalText": "string (required)", "steps": "[]object (required)"}},
		{Name: "shadowCompare", Description: "Compare a model's plan with the shadow skills of its goal.", Handler: p.shadowCompare,
			ArgsSchema: map[string]string{"goalSignature": "string (required)", "plan": "[]object (required)"}},
		{Name: "reportSkillOutcome", Description: "Report one canary or trusted skill run.", Handler: p.reportSkillOutcome,
			ArgsSchema: map[string]string{"skillId": "string (required)", "ok": "bool (required)", "binding": "object"}},
		{Name: "decidePromotion", Description: "The owner's decision on a skill's promotion.", Handler: p.decidePromotion,
			ArgsSchema: map[string]string{"skillId": "string (required)", "templateDigest": "string (required)", "approved": "bool (required)"}},
		{Name: "sweepSkills", Description: "Re-apply demotion and retirement to every skill.", Handler: p.sweepSkills,
			ArgsSchema: map[string]string{}},
		{Name: "findSkill", Description: "Find the skill to run by signature, trigger or meaning.", Handler: p.findSkill,
			ArgsSchema: map[string]string{"goalSignature": "string", "trigger": "string", "text": "string", "minSimilarity": "number"}},
	}
}
