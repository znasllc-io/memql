package gamespack

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/embedding"
)

// lane_db_test.go -- what episodes_db_test.go cannot see, because the harness
// swaps the cluster's embedder space for testSpace: a vector reaches its table
// through the one embedding write path (builtin gamesEmbedStore, then
// integration.embedding.store), every embedding call a WRITE causes rides the
// background lane, and a READ's does not.
//
// The pack stamps the lane in three places, and each has a test that reads the
// lane off the context the model is handed, so none can be taken out unseen:
// recording an episode (gamesRecordEpisodes), back-filling a vector that is
// still pending (gamesEmbedPending), and embedding a skill as it is learned
// (the learner). Every test below uses text that is unique to its run, because
// integration.embedding.store serves a repeated text from embedding_cache
// without asking the model, and the cache outlives the test.

// laneSpy is the model: it embeds like testSpace and records which lane each
// call arrived on. memql.BackgroundLaneFromContext is the reader the engine
// exports for exactly this -- a fake that cannot see the mark cannot tell a
// promise of the background lane from its absence. While it is away it records
// the call and then fails it, which is the model being gone: the attempt was
// made, on whatever lane, and nothing came back.
type laneSpy struct {
	mu     sync.Mutex
	away   bool
	onLane []bool
}

func (s *laneSpy) setAway(away bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.away = away
}

func (s *laneSpy) Embed(ctx context.Context, text string) ([]float32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onLane = append(s.onLane, memql.BackgroundLaneFromContext(ctx))
	if s.away {
		return nil, errModelAway
	}
	return bagOfWords(text), nil
}

func (s *laneSpy) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := s.Embed(ctx, t)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func (s *laneSpy) Dimensions() int { return testDims }

func (s *laneSpy) calls() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.onLane...)
}

// useLaneSpy puts the production embedding path in front of spy: the cluster's
// real clusterSpace over the pack's real store, the real embedding integration
// registered on the engine, and the harness's test embedder bound as the
// cluster's. It is what ships with only the model replaced.
func (lg *liveGames) useLaneSpy(t *testing.T, spy *laneSpy) clusterSpace {
	t.Helper()
	resolve := func(context.Context, string) (memql.EmbeddingAIProvider, error) { return spy, nil }
	integ := embedding.New(lg.eng.Logger)
	integ.SetDBGetter(func() *sql.DB { return lg.db.DB })
	integ.SetEmbeddingProvider(resolve)
	integ.SetStagedConceptPredicate(func(string) bool { return false })
	if err := lg.eng.RegisterIntegration(integ); err != nil {
		t.Fatalf("RegisterIntegration(embedding): %v", err)
	}
	space := clusterSpace{resolve: resolve, rows: lg.provider.store, db: func() *sql.DB { return lg.db.DB }}
	lg.provider.space = space
	memql.SetActiveEmbedderBinding(memql.EmbedderBinding{ProviderRef: testProvider, Dimensions: testDims, ActivatedAt: rfc3339(time.Now())})
	t.Cleanup(memql.ClearActiveEmbedderBinding)
	return space
}

// laneSkillVectors counts the vectors the test space holds for one skill.
func (lg *liveGames) laneSkillVectors(t *testing.T, skillID string) int {
	t.Helper()
	var n int
	if err := lg.db.DB.QueryRow(`SELECT count(*) FROM `+lg.table+` WHERE id = $1 AND vector_field = 'content'`,
		canonical(conceptSkill, skillID)).Scan(&n); err != nil {
		t.Fatalf("count skill vectors: %v", err)
	}
	return n
}

// laneTorchSkill teaches the torch skill from two agreeing successful traces
// of a goal worded as given, and returns the skill's id. learnTorchSkill's goal
// is the same text on every run, which the embedding cache would answer.
func laneTorchSkill(t *testing.T, lg *liveGames, ctx context.Context, prefix, goal string) string {
	t.Helper()
	record := func(suffix string, count float64) map[string]any {
		tr := trace(prefix+suffix, torchPlan(count), "success", "neutral")
		tr["goalText"] = goal
		return lg.must(t, ctx, "builtin", "gamesRecordTrace", tr)[0]
	}
	record("-a", 4)
	res := record("-b", 6)
	skillID, _ := res["learnedSkillId"].(string)
	if skillID == "" {
		t.Fatalf("two agreeing successful traces taught no skill: %v", res)
	}
	return skillID
}

func TestLiveEpisodeEmbeddingsRideTheBackgroundLane(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-lane-" + n)
	now := time.Now().UTC()

	// The production path end to end: clusterSpace, the gamesEmbedStore
	// builtin, and the real embedding integration over the spy model.
	spy := &laneSpy{}
	space := lg.useLaneSpy(t, spy)

	// The summaries are unique to this run, as the file's header says.
	res := lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-lane-a-"+n, "wren", "", "wren dug a well "+n, now, 4),
		episode("e-lane-b-"+n, "wren", "", "wren found a key "+n, now, 6),
	)})
	if asInt(res[0]["embedded"], 0) != 2 || asInt(res[0]["pending"], -1) != 0 {
		t.Fatalf("answer = %v, want 2 embedded, 0 pending", res[0])
	}
	if got := lg.vectorsFor(t, "e-lane-a-"+n, "e-lane-b-"+n); got != 2 {
		t.Fatalf("%d vectors stored through gamesEmbedStore, want 2", got)
	}
	calls := spy.calls()
	if len(calls) != 2 {
		t.Fatalf("the model was asked %d times, want once per episode", len(calls))
	}
	for i, onLane := range calls {
		if !onLane {
			t.Errorf("write embedding %d did not ride the background lane: a burst of memories would spend the interactive budget", i)
		}
	}

	// A read's query vector is what a decision is waiting on: interactive.
	if _, err := space.embedQuery(ctx, "wren dug a well "+n); err != nil {
		t.Fatalf("embedQuery: %v", err)
	}
	if calls := spy.calls(); len(calls) != 3 || calls[2] {
		t.Fatalf("a read's embedding must stay on the interactive lane, got %v", calls)
	}
}

// THE BACKLOG IS NOBODY'S DECISION EITHER. Episodes recorded while the model
// is away are written without a vector; when it is back, gamesEmbedPending asks
// it for each one, and a runtime that drains a long backlog must not spend the
// interactive budget a decision is waiting on. Only the calls made once the
// model is back are counted, so the stamp under test is gamesEmbedPending's own
// and not the one gamesRecordEpisodes put on the attempts that failed.
func TestLivePendingEmbeddingsRideTheBackgroundLane(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-lane-pending-" + n)
	now := time.Now().UTC()
	spy := &laneSpy{}
	lg.useLaneSpy(t, spy)

	// The model is away: both episodes are recorded and neither is embedded.
	spy.setAway(true)
	res := lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-pend-a-"+n, "wren", "", "wren mended a fence "+n, now, 4),
		episode("e-pend-b-"+n, "wren", "", "wren drew a map "+n, now, 6),
	)})
	if asInt(res[0]["embedded"], -1) != 0 || asInt(res[0]["pending"], -1) != 2 {
		t.Fatalf("answer = %v, want 0 embedded, 2 pending while the model is away", res[0])
	}
	if got := lg.vectorsFor(t, "e-pend-a-"+n, "e-pend-b-"+n); got != 0 {
		t.Fatalf("%d vectors stored while the model was away, want 0", got)
	}

	// The model is back: the backlog is embedded, every call on the lane.
	spy.setAway(false)
	seen := len(spy.calls())
	res = lg.must(t, ctx, "builtin", "gamesEmbedPending", nil)
	if asInt(res[0]["embedded"], 0) != 2 || asInt(res[0]["failed"], -1) != 0 {
		t.Fatalf("answer = %v, want the 2 pending episodes embedded", res[0])
	}
	if got := lg.vectorsFor(t, "e-pend-a-"+n, "e-pend-b-"+n); got != 2 {
		t.Fatalf("%d vectors after gamesEmbedPending, want 2", got)
	}
	calls := spy.calls()[seen:]
	if len(calls) != 2 {
		t.Fatalf("the model was asked %d times for the backlog, want once per pending episode", len(calls))
	}
	for i, onLane := range calls {
		if !onLane {
			t.Errorf("pending embedding %d did not ride the background lane: draining a backlog would spend the interactive budget", i)
		}
	}
}

// A SKILL IS EMBEDDED AS IT IS LEARNED, AND THAT CALL RIDES THE LANE TOO. The
// trace that completes the learning is a game runtime's report, and the skill's
// vector is nobody's decision. The model is away for the first skill: learning
// does not wait on it, the skill is written without a vector, and the attempt
// to embed it still reaches the model on the background lane -- the lane is
// stamped before the model answers, not because it did. Then the model is back,
// and the next owner's skill is embedded as it is learned, on the lane.
func TestLiveASkillsEmbeddingRidesTheBackgroundLane(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	spy := &laneSpy{}
	lg.useLaneSpy(t, spy)

	spy.setAway(true)
	away := laneTorchSkill(t, lg, asOwner("user-games-lane-skill-"+n), "lane-away-"+n, "Light the lamps "+n)
	attempts := spy.calls()
	if len(attempts) == 0 {
		t.Fatal("learning a skill never asked the model for its vector, so the lane it asks on was not exercised")
	}
	for i, onLane := range attempts {
		if !onLane {
			t.Errorf("skill embedding attempt %d did not ride the background lane: learning would spend the interactive budget", i)
		}
	}
	if got := lg.laneSkillVectors(t, away); got != 0 {
		t.Fatalf("a skill learned while the model was away has %d vectors", got)
	}

	spy.setAway(false)
	seen := len(attempts)
	here := laneTorchSkill(t, lg, asOwner("user-games-lane-skill-b-"+n), "lane-here-"+n, "Light the lanterns "+n)
	calls := spy.calls()[seen:]
	if len(calls) == 0 {
		t.Fatal("learning a skill with the model present never asked it for the skill's vector")
	}
	for i, onLane := range calls {
		if !onLane {
			t.Errorf("skill embedding %d did not ride the background lane: learning would spend the interactive budget", i)
		}
	}
	if got := lg.laneSkillVectors(t, here); got != 1 {
		t.Fatalf("a skill learned with the model present has %d vectors, want 1", got)
	}
}
