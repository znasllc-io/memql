---
title: Consuming the games pack from a game runtime
audience: public
status: stable
area: build
sinceVersion: 0.31.11
owner: znas
---

# Consuming the games pack from a game runtime

The **games pack** (`packs/gamespack`, domain `games`) gives a game runtime
that runs **outside** the cluster two things: a **memory** of the people who
play, and **skills** learned from the plans the runtime executed. The runtime
executes every action itself. The engine stores, ranks, generalizes and
governs; it never runs a game action.

If you have not read [Building a pack](building-a-pack.md), read it first. A
**pack** is a client-agnostic product feature, the third of the [three
extension words](../concepts/component-integration-pack.md).

> **The pack ships disabled.** It stores memories about the people who play,
> and holding that data is the owner's decision. Enable it in **Cluster →
> Modules → games**, then restart every node: enablement is read once, at
> boot. Until then its concepts load and nothing else does, and a call answers
> `function "<name>" not found`.

## Who calls, and whose rows they are

Every concept is on the owned tier with the cluster-owner read arm, and every
write stamps `ownerUserId` from the caller. The runtime and the owner's own
pages therefore sign in **as the owner** and read and write the same rows. A
live subscription on one of the pack's concepts reaches the owner's streams
and a cluster owner's, and nobody else's.

Three writers are server-only: plan traces, skills and the skill ladder are
written by the pack's builtins, never by a client.

## What you are adopting

| Concept | What it is | Retention |
|---|---|---|
| `v1:games:player` | A person the owner registered: `gamertag`, `displayName`, `consented` | newest version kept; older versions expire after 30 days |
| `v1:games:episode` | One remembered moment: `characterId`, `playerId?`, `worldId`, `at`, `kind` (`chat`, `event`, `activity`, `session`), `summary`, `importance` 1-10 | expires whole after 30 days |
| `v1:games:preference` | What a player likes: `playerId`, `subject`, `valence` -1..1, `confidence` 0..1, `evidence` | newest version kept |
| `v1:games:promise` | What a character promised a player: `characterId`, `playerId`, `text`, `status` (`open`, `kept`, `dropped`), `madeAt` | newest version kept |
| `v1:games:place` | A place in a world, shared by every character: `worldId`, `name`, `kind`, `position`, `discoveredBy?`, `at` | newest version kept |
| `v1:games:skill` | A learned skill and its place on the ladder | newest version kept |
| `v1:games:planTrace` | One executed plan, the evidence skills are learned from | expires whole after 30 days |

`characterId` and `worldId` are your runtime's own stable slugs. Retention is
the engine's hourly sweep; your runtime prunes nothing.

**Ids are yours, bare, at most 64 characters, and unique per concept across
the whole cluster, across concepts as well.** A builtin's returned rows are keyed
by node id, so an episode, a place and a preference that share an id collapse
into one. Another owner's row on the same id is refused (the refusal also
reveals the id exists). A second insert of the **same owner** on an existing id
writes a new version of that row, not a failure, so a re-send is safe and the
history grows one version per send. Derive ids that cannot collide with another owner's: put the owner's
token subject in the hash. With `h(x)` the first 20 hex digits of
`sha256(x)`:

| Id | Derivation |
|---|---|
| `playerId` | `"p-" + h(sub + "|" + lower(gamertag))` |
| `preferenceId` | `"pf-" + h(sub + "|" + playerId + "|" + normalize(subject))` |
| `placeId` | `"pl-" + h(sub + "|" + worldId + "|" + normalize(name))` |
| episode, promise, trace | a prefix (`e-`, `pr-`, `t-`) and 32 random hex digits |

`normalize` is the goal-signature normalization below. Skill ids are the
engine's.

## Memory

A time you compare must be in the stored form `2006-01-02T15:04:05.000Z`
(UTC, milliseconds always present): `gamesSessionEpisodes` compares `at` as
text, so a `since` spelled `...12:00:00Z` drops the episodes at `12:00:00.000Z`
and an offset spelling returns nothing. A qualified id is accepted and echoed
as sent.

**Record.** `builtin gamesRecordEpisodes(episodes: [...])` writes up to 50
episodes, then embeds each summary on the **background** rate bucket, so a
burst of memories never spends the interactive budget a decision needs. It
answers `{written, embedded, pending, ids}`. An embedding that fails leaves
its episode recorded and pending, and `builtin gamesEmbedPending(limit: 25)`
embeds the backlog when the model is back, answering `{embedded, failed,
more}`. Call it until `more` is false; that is also how memory heals after
the cluster's embedder binding changes.

**Recall.** One call returns what a character remembers that matters now:

```text
builtin gamesRecall(characterId: "<character>", playerId: "<player>", worldId: "<world>", text: "<what was said>")
```

Episodes are the character's own or about the player, scored

```text
score = 0.5 x relevance + 0.3 x recency + 0.2 x importance / 10
relevance = max(cosine(query, summary), 0)
recency   = exp(-ln 2 x ageHours / 72), age measured from the episode's `at` to `asOf`
```

and the top `k` (default 8, at most 50) are followed by the player's most
confident preferences (3), the character's open promises to the player (5)
and the world's places (10). Each row is `{rank, kind, id, text, score,
relevance, recency, importance, relevanceAvailable, row}`. **Order by
`rank`**: the order rows arrive in is the transport's. The `id` and `row.id` are bare, while ids inside `row` (`ownerUserId`,
`playerId`) are canonical. `asOf`, `halfLifeHours`, the three weights and the
three counts are optional arguments: `asOf` is RFC 3339 (`T`, and `Z` or an
offset; years 0001-9999), weights are 0..1000, and a half-life is
0.000001..87600 hours; anything else is refused with `games_invalid_argument`.
`relevanceAvailable` is per row, and is also false when the similarity was not
a number. Recall waits at most 2 seconds for the embedder.

With no embedder bound, or while the model cannot embed the query, relevance
is 0 for every episode, recency and importance still rank them, and every row
says `relevanceAvailable: false`. Recall never fails because a model is away.

**Forget.** `builtin gamesForgetPlayer(playerId: "<player>")` deletes, in
every version and in one transaction, the player; every episode, preference
and promise whose `playerId` is the player; and, of the caller's own rows, the
ones that name the player's gamertag as a whole name, in any case: an episode's
`summary`, a preference's `subject`, a promise's `text` and a place's `name`
(free text only; ids, characters, worlds, kinds, `discoveredBy` and `evidence`
are never read), every plan trace that mentions it, and every skill whose goal,
spoken line, constants or last binding name it. Earlier gamertags of a renamed
player count. The vectors of everything deleted go too. A skill that names
nobody is kept. Only the caller's own rows are touched, a cluster owner's
included. It answers `{playerId, deleted: {players, episodes, preferences,
promises, places, planTraces, skills, vectors}}`; a row that two rules both
found is counted once, under its own concept's key.

Limits, stated so they are not a surprise: the match follows the database
collation, so a gamertag inside a longer word is not a match but `Zo` in
`Zoe` with a diacritic may be; a gamertag equal to a plain word (`look`,
`true`, `5`) deletes the caller's traces containing it; `displayName` is not
matched, and free-text mentions in a row with no `playerId` of the player are
found only by the gamertag; cached embeddings keyed by a text digest stay until
their 30-day expiry; and a write that commits at the same moment as a forget
can survive it, so repeat the forget if in doubt.

The plain writes are mutations: `upsertGamesPlayer`, `createGamesEpisode`,
`upsertGamesPreference`, `upsertGamesPromise`, `setGamesPromiseStatus` and
`upsertGamesPlace`. The reads are queries: `gamesPlayers`, `gamesPlayerById`,
`gamesPreferencesForPlayer`, `gamesOpenPromises`, `gamesPromisesForPlayer`,
`gamesPlacesInWorld`, `gamesSessionEpisodes` and `gamesEpisodesForPlayer`.

## Skills

A skill is a plan your runtime executed often enough, the same way, that it
can run again without asking a model.

**Record.** After every plan, `builtin gamesRecordTrace(...)` with the
`goalText`, the goal's `inputs`, the `steps` (`{action, args, ok,
durationMs, error}`), the `outcome` (`success`, `failure`, `abandoned`) and
the player's `reaction` (`positive`, `neutral`, `negative`). It answers
`{traceId, goalSignature, learnedSkillId}`.

**The goal signature** is how a goal is recognized again:

```text
normalize(s)  = lower-case; keep letters and digits; a run of whitespace becomes one space; drop everything else; trim
GoalSignature = hex(sha256(normalize(goalText) + "|" + join(sort(input keys), ",")))
```

`GoalSignature("Light up the area!", ["spot"])` is
`d22aafce7ea66a16b435a1e6b8d178a8cdcbddaf5f254dc5677096ac76b5c0bd`. Only the
input KEYS enter it. Pass your own as `goalSignature` and the engine checks
it: a mismatch is refused with `games_signature_mismatch` on the first trace,
rather than never matching anything.

**Learn.** When two successful traces of one signature, neither disliked and
neither a skill run, share an action sequence, the engine generalizes them:
an argument equal in both stays a constant, one that differs becomes a hole
named `<stepIndex>.<argKey>`. The skill is born in **shadow**, once per
goal and action sequence.

```text
template = {"steps": [{"action": "collect", "args": {"block": {"const": "coal"}}},
                      {"action": "craft", "args": {"item": {"const": "torch"}, "count": {"hole": "1.count"}}}],
            "holes": ["1.count"]}
binding  = {"1.count": 5}
```

**The ladder** is the one learned procedures use, under the cluster's ladder
policy, whose defaults are:

| Call | What it does |
|---|---|
| `gamesShadowCompare(goalSignature, plan)` | Your model planned the goal; compare its plan with the shadow skills. Five consecutive matches across at least two values of every hole propose a promotion. Answers `{compared, matched, proposed}`. |
| `gamesDecideSkillPromotion(skillId, templateDigest, approved)` | The owner's one decision, pinned to the version they were shown. Approval makes the skill canary. |
| `gamesReportSkillOutcome(skillId, ok, binding)` | One canary or trusted run. Five clean canary runs make it trusted; two consecutive failures send it back to shadow. |
| `gamesSweepSkills()` | Re-applies demotion, and retires a skill unused for 30 days. Call it at start and daily. |

A skill's `state` is `shadow`, `awaiting_approval`, `canary`, `trusted` or
`retired`. A retired skill is final; the goal coming back is learned again as
a new skill.

**The owner approves, the runtime does not.** The runtime and the owner's
pages are the same identity, so nothing in the engine can tell a person's
click from your runtime calling `gamesDecideSkillPromotion`. Never call it
from the runtime.

**Lookup.** `builtin gamesFindSkill` takes exactly one of:

| Key | Answers |
|---|---|
| `goalSignature` | the trusted skill of the goal, else the canary one; your runtime asked a model first |
| `trigger` | the most recently used trusted skill learned under it, matched whole and as recorded; no model asked at all |
| `text` | the trusted skill whose goal text is nearest, at a cosine of at least `minSimilarity` (0.82) |

Only the caller's own skills are found, a cluster owner's included, and the
lists a lookup picks from are paged (a goal's 20 newest skills, the 100 most
recently used trusted ones). The answer is the skill re-read past the result
cache, so a demotion on another replica is never served stale. It answers one
row, the skill plus `serve` (`skill` or `model`), `standby`
(a canary skill runs with the model standing by), `serveReason` and, for
`text`, `similarity`, or no row. Fill the holes with your own binding and
execute the steps yourself. A `text` lookup fails while the text cannot be
embedded; treat that as no match.

## Paged reads are the caller's own

The lists (`gamesPlayers`, `gamesSessionEpisodes`, `gamesSkills`,
`gamesTrustedSkills` and the rest) each filter on the caller's own
`ownerUserId`, so a cluster owner's page is its own rows, not the union the
owned tier's read arm would hand it. A by-id read (`gamesPlayerById`,
`gamesSkillById`) is not filtered that way; the pack's builtins hold what they
act on to the caller's own rows regardless.

## Live views

Subscribe with `subscribeGraph` to `v1:games:player`, `episode`,
`preference`, `promise`, `place` and `skill`, actions `created` and
`updated`; the events cross replicas. Plan traces are not forwarded, and
deletes publish nothing, so a live view re-reads after a forget.

## Refusals

Refusals lead with a stable code. Match the code, not the sentence.

| Code | When |
|---|---|
| `games_unauthenticated` | the call carried no signed-in owner |
| `games_invalid_argument` | a required argument is missing or malformed |
| `games_signature_mismatch` | your `goalSignature` is not the engine's |
| `games_skill_not_found` | the skill is not readable by this caller |
| `games_skill_not_serving` | an outcome for a skill that is not canary or trusted |
| `games_skill_version_changed` | a decision about a template digest the skill no longer has |
| `games_no_promotion_open` | a decision for a skill with nothing to decide |
| `games_player_not_found` | the player is not one of this owner's |

Model calls meet the engine's own refusals too; see [LLM cost
control](../ai/llm-cost-control.md).

## Putting reflection on the background lane

Your own prompts reach the model through `builtin ai`. A call nobody is
waiting on, such as a reflection written after a session, belongs on the
background lane: `builtin ai(templateId: "<prompt>", data: {...}, lane:
"background")`. See [Functions](../language/functions.md).
