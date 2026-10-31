package gamespack

// RefusalCodes are the stable codes this pack's refusals lead with. A client
// matches the code, never the sentence around it; refusals_test.go holds this
// list equal to the codes the source actually raises.
var RefusalCodes = []string{
	"games_unauthenticated",       // the call carried no signed-in owner
	"games_invalid_argument",      // a required argument is missing or malformed
	"games_signature_mismatch",    // a supplied goalSignature is not GoalSignature(goalText, input keys)
	"games_skill_not_found",       // the skill is not readable by this caller
	"games_skill_not_serving",     // an outcome was reported for a skill that is not canary or trusted
	"games_skill_version_changed", // a promotion decision named a template digest the skill no longer has
	"games_no_promotion_open",     // a promotion decision for a skill with no open promotion
	"games_player_not_found",      // the player is not one of this owner's players
}
