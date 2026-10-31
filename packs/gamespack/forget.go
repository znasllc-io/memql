package gamespack

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/gatecontext"
	"github.com/znasllc-io/memql/core/id"
)

// forget.go -- gamesForgetPlayer: delete, outright, what this owner holds
// about one player.
//
// A DELETE, NOT A SOFT DELETE OR A SCRUB. The obligation "forget this player"
// is that the rows are gone, in every version, together with the vectors
// computed from them. MemQL has no delete mutation, so this is raw SQL, the
// way the Shopify connector's shop/redact purge is (integrations/shopify
// compliance.go), and every statement is scoped to the CALLER'S ownerUserId in
// its canonical spelling (ownerKey). The Go check that refuses a player who is
// not the caller's is not the only thing between a call and another owner's
// rows: a cluster owner can READ every owner's players, and each statement
// refuses them by itself, which a test shows by running the statements as
// someone else.
//
// ONE TRANSACTION, ALL OR NOTHING. A forget that stopped half way would leave
// the caller believing a child was forgotten while their words were still
// stored. Every statement runs in one transaction, and the vectors go in it
// too; a failure anywhere undoes everything before it. That includes a vector
// table the forget cannot name (deleteVectorsIn): it is an error and not a table
// to skip, because a forget that skipped it would report the child forgotten with
// their vectors still there. The caches are evicted only once the transaction has
// committed; evicted before, a reader could refill them from rows that are about
// to go, and they would keep them until they expire.
//
// WHAT GOES: the player row; every episode, preference and promise whose
// playerId is the player; everything else of the owner that names the player's
// gamertag as a whole name -- the episodes of anyone, the preferences, promises and
// places, the plan traces and the skills (below); and the vectors of every deleted
// id, in every embedder space. WHAT STAYS: a skill that names nobody. A skill is a
// procedure, not a memory of a person, and one that keeps nothing of the player is
// kept, with everything the owner taught it.
//
// THE EPISODES, PREFERENCES, PROMISES AND PLACES THAT NAME THE PLAYER are found by the
// traces' rule (nameBefore and nameAfter, any case), in the one free text a person or a
// character wrote in each:
//
//	an episode           its summary
//	a preference         its subject
//	a promise            its text
//	a place              its name
//
// A row is about the player by its playerId, but the words about them stand in other
// rows too: an episode that is about someone else and tells what the player did, a
// promise made to another player that names them, a place called for them (a place is
// about nobody). A row is deleted in every version when any version names the player,
// as a skill is (below), and a row found by its playerId and by its words is one row.
//
// THE REST OF A ROW IS NOT READ. These are identifiers and enums, by contract and in
// every producer: an id; a character (characterId); a world (worldId); a kind; a
// player's id; the character that found a place (discoveredBy); the ids of the episodes
// a preference was inferred from (evidence); a time; a status. A gamertag that equals
// one, or begins one, would delete every row that has it: a player tagged like a pal
// would take every place the pal found (discoveredBy holds pal ids, and the runtime
// leaves a player's name out of it), and a player tagged like the prefix of the ids the
// runtime makes (evidence holds ids such as ses-<32 hex>, in which a name before a dash
// is a whole name) would take every preference that has any evidence. A reading that
// can only over-delete by collision is not an erasure's safe side, it is a way to
// delete the owner's learning; so the concepts say these fields must not hold a
// player's name, and a forget does not read them.
//
// THE KEYS OF THE ANSWER. The rows that name the player are counted in the keys the
// rows about the player already have (episodes, preferences, promises), each row once,
// and the places in a key of their own, places. A key for each rule was the other
// choice; a caller who asks how many episodes went does not care by which rule one
// did, and an answer that said "episodes: 3, namedEpisodes: 2" would have it add them
// up. The builtin's description and the design record (5.4) say so.
//
// A SKILL IS FOUND BY THE SAME NAME, in the words it keeps: the goal, the line the
// character says when it runs the skill, the constants of its template (the
// arguments every demonstration agreed on) and the values of its last binding (the
// hole values of its newest matching comparison or clean run). The rule is the
// traces', whole name and any case (nameBefore and nameAfter), on VALUES ONLY: the
// names of the keys of those objects, the names of the actions, the ids of the holes
// and the trigger a skill was learned under are the runtime's own words and never a
// person's, and a gamertag that equals one of them would otherwise delete every skill
// that has the key. A skill is a row of many versions, one for every move of its
// ladder, and the name may be in any of them (a binding is overwritten by the next
// one), so a skill is deleted whole, every version, when any version names the
// player: leaving the newest version alone would leave the name in the table, and
// leaving an older one alone would bring its stale ladder back as the skill's current
// state. Its vector goes with it.
//
// SKILLS ARE FENCED AGAINST THEIR LADDER MOVES. A move reads a skill and writes its next
// version inside the write gate games-skill:<id>, and a version that commits after the
// delete statement began is a version the delete never sees: the skill would survive with
// the words that name the player, and its vector would be gone, so they would be embedded
// again. So forget lists the skills it is about to delete, takes the gate of each on its own
// transaction and keeps it until that ends, and only then deletes (fenceSkills). A move in
// flight finishes first and the delete sees what it wrote; a move that starts meanwhile
// waits for the commit and finds no skill.
//
// A TRACE IS FOUND BY THE GAMERTAG AS A WHOLE NAME, in any case. Matched as a
// substring, the gamertag Tag would take "Tagger", "stage" and "tags" with it,
// and delete traces about other players, other characters and nothing at all.
// The gamertag has to stand between characters that cannot continue a name (a
// letter, a digit, an underscore), the pattern is built in Go with every
// character that the pattern language, JSON text and a SQL string treat
// specially made literal (traceMentionPattern), and it is a parameter of the
// statement, never a part of its text.
//
//   - ANY GAMERTAG THE PLAYER HAS HELD. A player renamed by an upsert has a version of
//     the row for each name, and a trace written before the change names the old one.
//   - NOT THE PARTICULARS OF A TRACE. Every trace carries its owner's id, its
//     character, the skill it ran, its outcome and reaction and its time, and none of
//     them names a player; a child called Wren in a world where the pal is called wren
//     would otherwise match every trace there is. They are left out of what is matched,
//     and everything a character did or said is in.
//
// KNOWN LIMITS:
//
//   - A WRITE ABOUT THE PLAYER THAT COMMITS AT THE SAME MOMENT AS THE FORGET CAN SURVIVE
//     IT. The engine has no gate per player, and of the skills only those that name the
//     player when the forget lists them are fenced. An episode inserted after the statement
//     that deletes the player's episodes ran; a skill learned from traces read before the
//     forget deleted them and created after the forget listed the skills it holds; a skill
//     that comes to name the player after it was listed, through a comparison in flight or
//     a run report in flight (gamesShadowCompare and gamesReportSkillOutcome write the
//     plan's or the run's arguments as the skill's lastBinding); a promise whose status is
//     set (setGamesPromiseStatus reads the row before the delete and writes the merged row
//     after it) -- each commits a version the forget never saw. The runtime that calls this
//     marks the player and drains its own queue of writes about them before it asks, so that
//     the forget is the last write; a caller that does not has not been promised more than
//     that.
//   - A trace or a skill that names the player only by the name the characters call
//     them (displayName) is not found; the gamertag is what a runtime writes into a
//     step's arguments.
//   - A skill that was LEARNED from the player's traces but keeps none of their words
//     (every demonstration agreed on arguments that say nothing of them) is kept: it
//     is a procedure the owner taught, and nothing in it is the player.
//   - The digests in a skill's distinctBindings are one-way hashes of the hole values its
//     comparisons matched, kept so that the ladder counts distinct values without keeping
//     them. They are not words and are not searched. A match writes its binding as the
//     skill's lastBinding in the same version, so while that version stands the skill is
//     found by the value itself; once retention has shed it (after 30 days) a skill that is
//     still compared keeps the digest of a value it no longer holds, and a digest names
//     nobody to anyone who does not already have the name to hash.
//   - A gamertag that is also a word the characters use ("Success", "Stage") matches
//     the traces and skills that use the word, and deletes them. That loses a little
//     learning, which is the safe direction for the player. The names of the fields
//     of a trace (steps, error, ok, ...) are in what is matched too, so a gamertag
//     equal to one of them matches every trace that has the field; a skill's keys are
//     not.
//   - The embedding cache (embedding_cache) keeps the vector it computed for a
//     summary under a key that is a digest of the text, for thirty days; nothing
//     links it to an id, so it is not deleted here.
//
// NO graph.node.deleted EVENT IS PUBLISHED, as for every delete in this
// engine; a live view drops the rows when it re-reads, which a client does
// after calling this.

// The deletes, each scoped to (concept, owner) and RETURNING the ids gone.
//
//	forgetPlayerRowSQL   $1 canonical player id  $2 canonical owner
//	forgetByPlayerSQL    $1 concept  $2 canonical owner  $3 canonical player id
//	forgetTracesSQL      $1 canonical owner  $2 the pattern of traceMentionPattern
//	forgetNamed...SQL    $1 canonical owner  $2 the pattern of traceMentionPattern (episodes,
//	                     preferences, promises and places)
//	forgetSkillIDsSQL    $1 canonical owner  $2 the pattern of traceMentionPattern
//	forgetSkillsSQL      $1 canonical owner  $2 the pattern of traceMentionPattern
//
// The player row comes back with the gamertag of each version it deleted, which
// is the set of names the other rows are looked for under. A trace is matched in
// the JSON text of its payload less the fields that name no one (see the header). A skill
// is matched in the JSON text of each STRING VALUE it keeps (jsonb_path_query's recursive
// accessor yields values and never keys), and the ids of the skills any version of which
// matches are deleted in every version. The paths are lax so that a template argument that
// is a hole, which has no const, is passed over and not an error, and so that a skill with
// no template or no binding yet has nothing to match.
//
// staged-data: MUST-NOT-GATE -- a row a staged-data gate hid would SURVIVE
// the forget, and the obligation is that the player's data is gone (the
// argument component/identity/authactivity's prune and the Shopify purge make).
const (
	forgetPlayerRowSQL = `DELETE FROM "MemoryNodes"
WHERE concept = 'v1:games:player' AND id = $1 AND (payload->>'ownerUserId') = $2
RETURNING id, COALESCE(payload->>'gamertag', '')`
	forgetByPlayerSQL = `DELETE FROM "MemoryNodes"
WHERE concept = $1 AND (payload->>'ownerUserId') = $2 AND (payload->>'playerId') = $3
RETURNING id`
	forgetTracesSQL = `DELETE FROM "MemoryNodes"
WHERE concept = 'v1:games:planTrace' AND (payload->>'ownerUserId') = $1
  AND (payload - ARRAY['ownerUserId', 'characterId', 'skillId', 'at', 'outcome', 'reaction'])::text ~* $2
RETURNING id`
	// The rows of the four memory concepts that name the player ($1 canonical owner, $2 the
	// pattern): found in the one free text a person or a character wrote in each -- an
	// episode's summary, a preference's subject, a promise's text, a place's name -- and
	// deleted in every version when any version names (a row rewritten once names the player
	// in the version that was deleted from view too). The rest of a row is identifiers and
	// enums and is NOT read: ids, a character, a world, a kind, the character that found a
	// place, the ids a preference was inferred from, a time, a status. A gamertag that equals
	// or begins one of those (a player tagged like a pal, or like the prefix of every id of a
	// kind) would otherwise delete every row of that pal, or every row with such an id.
	forgetNamedEpisodesSQL = `DELETE FROM "MemoryNodes"
WHERE concept = 'v1:games:episode' AND (payload->>'ownerUserId') = $1
  AND id IN (SELECT id FROM "MemoryNodes" WHERE concept = 'v1:games:episode' AND (payload->>'ownerUserId') = $1 AND (payload->'summary')::text ~* $2)
RETURNING id`
	forgetNamedPreferencesSQL = `DELETE FROM "MemoryNodes"
WHERE concept = 'v1:games:preference' AND (payload->>'ownerUserId') = $1
  AND id IN (SELECT id FROM "MemoryNodes" WHERE concept = 'v1:games:preference' AND (payload->>'ownerUserId') = $1 AND (payload->'subject')::text ~* $2)
RETURNING id`
	forgetNamedPromisesSQL = `DELETE FROM "MemoryNodes"
WHERE concept = 'v1:games:promise' AND (payload->>'ownerUserId') = $1
  AND id IN (SELECT id FROM "MemoryNodes" WHERE concept = 'v1:games:promise' AND (payload->>'ownerUserId') = $1 AND (payload->'text')::text ~* $2)
RETURNING id`
	forgetNamedPlacesSQL = `DELETE FROM "MemoryNodes"
WHERE concept = 'v1:games:place' AND (payload->>'ownerUserId') = $1
  AND id IN (SELECT id FROM "MemoryNodes" WHERE concept = 'v1:games:place' AND (payload->>'ownerUserId') = $1 AND (payload->'name')::text ~* $2)
RETURNING id`
	// skillNamesSQL is what makes a skill one that names the player, for the statement
	// that lists such skills and the one that deletes them; $2 is the pattern.
	skillNamesSQL = `((payload->'goalText')::text ~* $2
        OR (payload->'confirmation')::text ~* $2
        OR EXISTS (SELECT 1 FROM jsonb_path_query(payload, 'lax $.template.steps[*].args.*.const.**', '{}', true) AS v
                   WHERE jsonb_typeof(v) = 'string' AND v::text ~* $2)
        OR EXISTS (SELECT 1 FROM jsonb_path_query(payload, 'lax $.lastBinding.**', '{}', true) AS v
                   WHERE jsonb_typeof(v) = 'string' AND v::text ~* $2))`
	forgetSkillIDsSQL = `SELECT DISTINCT id FROM "MemoryNodes"
WHERE concept = 'v1:games:skill' AND (payload->>'ownerUserId') = $1
  AND ` + skillNamesSQL + `
ORDER BY id`
	forgetSkillsSQL = `DELETE FROM "MemoryNodes"
WHERE concept = 'v1:games:skill' AND (payload->>'ownerUserId') = $1
  AND id IN (
    SELECT id FROM "MemoryNodes"
    WHERE concept = 'v1:games:skill' AND (payload->>'ownerUserId') = $1
      AND ` + skillNamesSQL + `)
RETURNING id`
)

// forgottenConcepts are the concepts a forget deletes rows of, and so the ones whose
// result caches it evicts once the deletes are committed.
var forgottenConcepts = []string{conceptPlayer, conceptEpisode, conceptPreference, conceptPromise, conceptPlace, conceptPlanTrace, conceptSkill}

// The statements of a forget, by the name Provider.afterForgetStep is called with, which
// is also the key the answer counts what the statement deleted under. The exceptions are the
// three that look for a name in rows of the kinds the by-player statements delete: each of
// those counts into the key of its kind, once a row however many statements found it.
const (
	stepPlayers          = "players"
	stepEpisodes         = "episodes"
	stepPreferences      = "preferences"
	stepPromises         = "promises"
	stepNamedEpisodes    = "namedEpisodes"
	stepNamedPreferences = "namedPreferences"
	stepNamedPromises    = "namedPromises"
	stepPlaces           = "places"
	stepPlanTraces       = "planTraces"
	stepSkills           = "skills"
	stepVectors          = "vectors"
)

func (p *Provider) forgetPlayer(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	owner, err := requireActor(ctx)
	if err != nil {
		return nil, err
	}
	given := trimmed(args["playerId"])
	if given == "" {
		return nil, fmt.Errorf("games_invalid_argument: playerId is required")
	}
	// bare() takes the short id out of any canonical id, so the id of an episode
	// would be read as the id of a player; only the player's own spellings pass.
	if id.ValidateShortId(conceptPlayer, given) != nil || strings.ContainsRune(given, 0) {
		return nil, fmt.Errorf("games_invalid_argument: playerId must be a bare id with no colons (or %s:<id>) and no NUL character, got %.40q", conceptPlayer, given)
	}
	playerBare := bare(given)

	// The player is read under the caller, fresh: a cached answer would show a
	// player a sibling replica has already forgotten. A cluster owner reads other
	// owners' players too, so the owner is compared here as well as in every statement.
	rows, err := p.store.query(memql.ContextWithFreshRead(ctx), "gamesPlayerById", map[string]any{"playerId": playerBare})
	if err != nil {
		return nil, err
	}
	var player map[string]any
	for _, r := range rows {
		if ownerKey(trimmed(r["ownerUserId"])) == ownerKey(owner) {
			player = r
			break
		}
	}
	if player == nil {
		return nil, fmt.Errorf("games_player_not_found: player %q is not one of this owner's players", playerBare)
	}

	db := p.db()
	if db == nil {
		return nil, fmt.Errorf("games: no database handle on this node")
	}
	// READ COMMITTED, named because the fence below depends on it: every statement sees
	// what had committed when IT began, so the delete that follows the skills' gates sees
	// the versions of the moves that finished before they were taken.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("games: forget: %w", err)
	}
	defer tx.Rollback()

	ownerID, playerID := ownerKey(owner), canonical(conceptPlayer, playerBare)
	// The answer names what went in the words a person reads, not concept ids, and
	// has every key whether or not a statement ran.
	deleted := map[string]int{stepPlayers: 0, stepEpisodes: 0, stepPreferences: 0, stepPromises: 0, stepPlaces: 0, stepPlanTraces: 0, stepSkills: 0, stepVectors: 0}
	counted := map[string]map[string]bool{} // by the answer's key, the rows counted
	gone := map[string]bool{}
	after := func(step string) error {
		if p.afterForgetStep == nil {
			return nil
		}
		if err := p.afterForgetStep(step); err != nil {
			return fmt.Errorf("games: forget stopped after %s: %w", step, err)
		}
		return nil
	}
	// removed takes in what one statement deleted: counted under the answer's key (a row two
	// statements both found is one), its vectors to be deleted, and the hook told.
	removed := func(step, key string, ids map[string]bool, err error) error {
		if err != nil {
			return fmt.Errorf("games: forget %s: %w", step, err)
		}
		if counted[key] == nil {
			counted[key] = map[string]bool{}
		}
		for rowID := range ids {
			counted[key][rowID] = true
			gone[rowID] = true
		}
		deleted[key] = len(counted[key])
		return after(step)
	}

	// The player first: its versions say which gamertags the traces are found by.
	ids, gamertags, err := deletePlayerRows(ctx, tx, playerID, ownerID)
	if err := removed(stepPlayers, stepPlayers, ids, err); err != nil {
		return nil, err
	}
	for _, c := range []struct{ step, concept string }{
		{stepEpisodes, conceptEpisode}, {stepPreferences, conceptPreference}, {stepPromises, conceptPromise},
	} {
		ids, err := deleteIDs(ctx, tx, forgetByPlayerSQL, c.concept, ownerID, playerID)
		if err := removed(c.step, c.step, ids, err); err != nil {
			return nil, err
		}
	}
	if pattern := traceMentionPattern(append(gamertags, trimmed(player["gamertag"]))...); pattern != "" {
		// What names the player, whoever it is about: episodes, preferences and promises of
		// anyone's, and the places, which are about nobody.
		for _, c := range []struct{ step, key, query string }{
			{stepNamedEpisodes, stepEpisodes, forgetNamedEpisodesSQL},
			{stepNamedPreferences, stepPreferences, forgetNamedPreferencesSQL},
			{stepNamedPromises, stepPromises, forgetNamedPromisesSQL},
			{stepPlaces, stepPlaces, forgetNamedPlacesSQL},
		} {
			ids, err := deleteIDs(ctx, tx, c.query, ownerID, pattern)
			if err := removed(c.step, c.key, ids, err); err != nil {
				return nil, err
			}
		}
		ids, err := deleteIDs(ctx, tx, forgetTracesSQL, ownerID, pattern)
		if err := removed(stepPlanTraces, stepPlanTraces, ids, err); err != nil {
			return nil, err
		}
		// The skills that keep the name in their words, with every version of their ladder,
		// once nothing can write one more.
		if err := p.fenceSkills(ctx, db, tx, ownerID, pattern); err != nil {
			return nil, err
		}
		ids, err = deleteIDs(ctx, tx, forgetSkillsSQL, ownerID, pattern)
		if err := removed(stepSkills, stepSkills, ids, err); err != nil {
			return nil, err
		}
	}
	if len(gone) > 0 {
		n, err := p.deleteVectors(ctx, tx, gone)
		if err != nil {
			return nil, fmt.Errorf("games: forget vectors: %w", err)
		}
		deleted[stepVectors] = n
		if err := after(stepVectors); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("games: forget: %w", err)
	}

	// The result caches of the seven concepts hold what was deleted, on this replica
	// and (the engine publishes the eviction) on the others.
	if p.invalidate != nil {
		for _, concept := range forgottenConcepts {
			p.invalidate(concept)
		}
	}
	// What is logged is what was done and for whom to see it was done, and nothing
	// about the player: neither the id, which a runtime may derive from a name, nor
	// the gamertag.
	p.logger().Info("games: forgot a player", "owner", ownerID, "deleted", deleted)

	out := make(map[string]any, len(deleted))
	for k, v := range deleted {
		out[k] = v
	}
	return answer("forgetPlayer", map[string]any{"playerId": playerBare, "deleted": out})
}

// fenceSkills holds, until tx ends, the write gate of every skill forget is about to delete.
//
// A ladder move holds the gate games-skill:<id> from its fresh read of the skill to its
// write. Without the same gate here, a move that had read a skill before forget's delete
// began, and wrote after it, committed a new version that the delete's snapshot never
// saw: the skill survived with the words that name the player while forget answered that it
// had deleted it, and its vector was gone, so gamesEmbedPending would embed those words
// again. With it, a move in flight finishes first (forget waits for its gate, and the
// delete that follows sees what it wrote), and a move that starts meanwhile waits for the
// commit, and then finds no skill and passes it over.
//
// THE LOCK IS THE WRITE GATE'S, AND IT IS TAKEN ON FORGET'S OWN TRANSACTION. memql's gate is
// a transaction-scoped advisory lock (pg_advisory_xact_lock on the key's hash), and
// AcquireWriteGatesWithin takes its locks on a transaction lent to it
// (gatecontext.WithTransaction) instead of opening one on the gate pool. So no connection
// of the pool of four is held while forget works, the lock ends with the transaction,
// committed or rolled back, and the key is the moves' own. The wait is the gate's
// (six seconds, enforced by the server): a forget that cannot have a skill's gate in time
// is refused whole, nothing deleted, and may be sent again.
//
// The ids are listed first (the statement forgetSkillsSQL shares its condition with) and
// taken in order, so two forgets of one owner can only wait for each other, never deadlock.
// ONLY THE SKILLS THAT NAME THE PLAYER WHEN THE LIST IS MADE ARE HELD. A skill that came to
// name the player after that -- through a comparison or a run report in flight whose
// arguments carry the gamertag, or by being learned -- is deleted by the statement that
// follows if it has committed by then (it looks again), but it is not held: that is a write
// about the player that commits at the same moment as the forget, and it can survive it (the
// design record's honest limits).
// The server's lock table bounds how many skills one forget can hold (a handful of
// thousand); a forget that names more fails whole.
func (p *Provider) fenceSkills(ctx context.Context, db *sql.DB, tx *sql.Tx, ownerID, pattern string) error {
	ids, err := selectIDs(ctx, tx, forgetSkillIDsSQL, ownerID, pattern)
	if err != nil {
		return fmt.Errorf("games: forget %s: %w", stepSkills, err)
	}
	if len(ids) == 0 {
		return nil
	}
	keys := make([]string, len(ids))
	for i, rowID := range ids {
		keys[i] = "games-skill:" + bare(rowID)
	}
	// The gate sets the transaction's lock_timeout to what is left of its wait, and the
	// statements after it would keep it; they get the value they had.
	var prior string
	if err := tx.QueryRowContext(ctx, `SELECT current_setting('lock_timeout')`).Scan(&prior); err != nil {
		return fmt.Errorf("games: forget %s: %w", stepSkills, err)
	}
	if _, err := memql.AcquireWriteGatesWithin(gatecontext.WithTransaction(ctx, db, tx), db, p.forgetGateWait, keys...); err != nil {
		return fmt.Errorf("games: forget could not hold the skills it deletes against their ladder moves, so nothing was deleted; send it again: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('lock_timeout', $1, true)`, prior); err != nil {
		return fmt.Errorf("games: forget %s: %w", stepSkills, err)
	}
	return nil
}

// selectIDs runs a select of ids and returns them in the order it gave them.
func selectIDs(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var rowID string
		if err := rows.Scan(&rowID); err != nil {
			return nil, err
		}
		ids = append(ids, rowID)
	}
	return ids, rows.Err()
}

// deleteIDs runs one of the deletes and returns the ids of the rows it deleted:
// one for each row, so a row that had several versions is one id.
func deleteIDs(ctx context.Context, tx *sql.Tx, query string, args ...any) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := map[string]bool{}
	for rows.Next() {
		var rowID string
		if err := rows.Scan(&rowID); err != nil {
			return nil, err
		}
		ids[rowID] = true
	}
	return ids, rows.Err()
}

// deletePlayerRows deletes every version of the player row and returns its id and
// the gamertag of each version.
func deletePlayerRows(ctx context.Context, tx *sql.Tx, playerID, ownerID string) (map[string]bool, []string, error) {
	rows, err := tx.QueryContext(ctx, forgetPlayerRowSQL, playerID, ownerID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	ids := map[string]bool{}
	var gamertags []string
	for rows.Next() {
		var rowID, gamertag string
		if err := rows.Scan(&rowID, &gamertag); err != nil {
			return nil, nil, err
		}
		ids[rowID] = true
		gamertags = append(gamertags, gamertag)
	}
	return ids, gamertags, rows.Err()
}

// What may stand on either side of a gamertag in a trace for it to be a name of its
// own and not part of a longer one: the start or the end of the text, or anything
// but a letter, a digit (in whatever script the database's locale knows) or an
// underscore.
//
// THE TEXT IS JSON, and that changes what stands before a name. A control character
// is spelled there as a backslash and one of b, f, n, r, t, or as a backslash, a u and
// four hexadecimal digits. So after a newline the character before a name is the
// letter n, and after U+0001 it is the digit 1, and by the rule above the name would
// be the tail of a longer word: a name right after a newline, a tab, a carriage return
// or any other control character was not found, and the trace that named the player
// survived the forget. The two spellings are boundaries of their own below (a
// backslash is two in a raw string, because that is how the pattern language names
// one). A literal backslash followed by an n is written in the text as two
// backslashes and an n, and passes for the spelling of a newline as well, so a
// name after it is found too: for an erasure that is the safe way to be wrong.
// After a name nothing needs saying, because the backslash that begins any such
// spelling is itself no name character.
const (
	nameBefore = `(^|[^[:alnum:]_]|\\[bfnrt]|\\u[0-9a-f]{4})`
	nameAfter  = `([^[:alnum:]_]|$)`
)

// traceMentionPattern is the POSIX advanced regular expression (what the ~*
// operator reads) that finds any of the gamertags as a whole name in the JSON text
// of a trace. It is "" when there is no gamertag to look for, and then no trace is
// looked for.
//
// A name is trimmed, and a name that is blank or repeats an earlier one is left
// out. The boundaries apply to every name, however it begins or ends: a leading "."
// is itself no name character, and ".Kid" still must not be found in "Mr.Kid".
func traceMentionPattern(gamertags ...string) string {
	var names []string
	seen := map[string]bool{}
	for _, g := range gamertags {
		if g = strings.TrimSpace(g); g != "" && !seen[g] {
			seen[g] = true
			names = append(names, areLiteral(jsonStringBody(g)))
		}
	}
	if len(names) == 0 {
		return ""
	}
	return nameBefore + "(" + strings.Join(names, "|") + ")" + nameAfter
}

// jsonStringBody is s as PostgreSQL writes it between the quotes of a JSON string,
// which is how a gamertag stands in the text of a trace (payload::text): the quote,
// the backslash and the control characters escaped, and every other character, a
// letter outside ASCII included, as it is. It is escape_json of the server
// (src/backend/utils/adt/json.c), and TestLiveJSONStringBodyIsWhatTheDatabaseWrites
// holds the two to each other.
func jsonStringBody(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < ' ':
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// areLiteral makes s match only itself in an advanced regular expression, by
// escaping each of the characters the language reads.
func areLiteral(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\.[](){}*+?|^$`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// vectorDeleteBatch is how many ids one delete of vectors carries. A player with a
// long history has more ids than a statement should hold, so they go in batches.
const vectorDeleteBatch = 1000

// deleteVectors removes the vectors of every id, in every embedder space the
// catalogue lists (deleteVectorsIn).
func (p *Provider) deleteVectors(ctx context.Context, tx *sql.Tx, ids map[string]bool) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	list := p.vectorTables
	if list == nil {
		list = func(ctx context.Context, tx *sql.Tx) ([]string, error) { return memql.EmbeddingVectorTables(ctx, tx) }
	}
	tables, err := list(ctx, tx)
	if err != nil {
		return 0, err
	}
	return deleteVectorsIn(ctx, tx, tables, ids)
}

// execer is the part of a transaction the delete of vectors uses.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// deleteVectorsIn removes the vectors of every id from each of the tables, in
// every embedder space, and answers how many it removed.
//
// A TABLE NAME GOES INTO THE TEXT OF THE STATEMENT, because an identifier cannot
// be a parameter, so only the name of a vector table does (isVectorTable, the check
// recall's join makes too). memql.EmbeddingVectorTables already reads only such
// names from the catalogue; this does not rely on that, and the ids are parameters.
//
// IT FAILS CLOSED. A name that is no vector table's is a table whose vectors cannot
// be deleted, and an erasure that skipped it would report the player forgotten with
// their vectors still there. So every name is checked before anything is deleted, and
// one that fails is an error, which undoes the forget it is part of.
//
// Explicit placeholder lists rather than an array parameter
// (component/identity/authactivity's reason: bun's pgdriver does not encode a bare
// []string as text[], and a wrong encoding would delete nothing while reporting
// success).
func deleteVectorsIn(ctx context.Context, ex execer, tables []string, ids map[string]bool) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	for _, table := range tables {
		if !isVectorTable(table) {
			return 0, fmt.Errorf("the embedder tables include %.60q, which is no vector table's name, so the vectors in it cannot be deleted and nothing is deleted", table)
		}
	}
	list := make([]string, 0, len(ids))
	for rowID := range ids {
		list = append(list, rowID)
	}
	sort.Strings(list)
	total := 0
	for _, table := range tables {
		for start := 0; start < len(list); start += vectorDeleteBatch {
			batch := list[start:min(start+vectorDeleteBatch, len(list))]
			params := make([]any, len(batch))
			marks := make([]string, len(batch))
			for i, rowID := range batch {
				params[i] = rowID
				marks[i] = "$" + strconv.Itoa(i+1)
			}
			res, err := ex.ExecContext(ctx, "DELETE FROM "+table+" WHERE id IN ("+strings.Join(marks, ", ")+")", params...)
			if err != nil {
				return total, fmt.Errorf("%s: %w", table, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return total, fmt.Errorf("%s: %w", table, err)
			}
			total += int(n)
		}
	}
	return total, nil
}

// isNoRows reports database/sql's empty single-row answer.
func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
