// A memorable address, generated.
//
// Sometimes an address should say nothing about what it serves -- a demo, a
// preview, a thing that is not ready to be found. A random string does that
// and is unusable: nobody can read one over a desk or recall which of three
// they meant. So this is the Docker-container shape, an ADJECTIVE and a NOUN,
// which is memorable for the same reason a phrase is: two ordinary words in a
// familiar order.
//
// RULES THE LISTS FOLLOW, because a generated name is one somebody may have to
// say out loud to a colleague or read off a screen:
//
//   - lowercase a-z only, so the result is a valid DNS label with no
//     normalisation step between here and the hostname;
//   - four to eight letters per word, so the pair fits a label comfortably
//     and neither half dominates;
//   - no proper nouns and no surnames. Docker names containers after real
//     scientists; a platform that hosts other people's sites should not put a
//     real person's name on somebody's address by accident;
//   - nothing that reads as a status ("failed", "broken", "live"), because
//     the address sits beside a status column and the two would be read
//     together;
//   - no near-homophones within a list, so a name survives being spoken.
//
// 100 x 100 is 10,000 pairs. That is not a uniqueness guarantee and is not
// meant to be one: the engine's hostname probe is what refuses a collision,
// and the answer to one is to generate again.

const ADJECTIVES = [
  "amber", "ancient", "arctic", "autumn", "azure", "balmy", "bold", "brave",
  "brisk", "bronze", "calm", "candid", "cedar", "clever", "cobalt", "coral",
  "cosmic", "crimson", "crisp", "curious", "dapper", "dawn", "deft", "dusky",
  "eager", "early", "easy", "ember", "fabled", "fearless", "fleet", "floral",
  "fluent", "frosty", "gentle", "gilded", "glad", "golden", "graceful", "grassy",
  "hardy", "hazel", "hidden", "humble", "indigo", "ivory", "jade", "jolly",
  "keen", "kindly", "lively", "lucid", "lunar", "marble", "meadow", "mellow",
  "merry", "misty", "modest", "morning", "nimble", "noble", "olive", "opal",
  "patient", "pearl", "placid", "polar", "prairie", "prism", "quiet", "rapid",
  "restless", "rustic", "sable", "sandy", "scarlet", "silent", "silken", "silver",
  "sleek", "snowy", "solar", "spirited", "spry", "steady", "stellar", "sunny",
  "supple", "swift", "tawny", "tender", "tidal", "timber", "topaz", "tranquil",
  "twilight", "velvet", "vivid", "wistful",
];

const NOUNS = [
  "acorn", "anchor", "arbor", "atlas", "aurora", "basin", "beacon", "bramble",
  "breeze", "bridge", "brook", "canyon", "cascade", "cavern", "cedar", "cinder",
  "clover", "comet", "compass", "cove", "crater", "crescent", "current", "delta",
  "ember", "estuary", "falcon", "fathom", "fern", "fjord", "forest", "fountain",
  "garden", "geyser", "glacier", "glade", "granite", "grotto", "harbor", "harvest",
  "heather", "horizon", "island", "jasmine", "juniper", "kestrel", "lagoon", "lantern",
  "lattice", "ledger", "lichen", "lily", "lookout", "lotus", "maple", "marsh",
  "meadow", "meridian", "mesa", "monsoon", "moraine", "mosaic", "nectar", "nimbus",
  "oasis", "orchard", "outpost", "parapet", "pebble", "pine", "plateau", "pollen",
  "prism", "quarry", "quill", "rapids", "reef", "ridge", "rivulet", "sable",
  "saffron", "sequoia", "shelter", "sierra", "signal", "solstice", "spindle", "spire",
  "spring", "summit", "sundial", "tempest", "thicket", "tide", "trellis", "tundra",
  "valley", "vantage", "willow", "zenith",
];

/** How many distinct names this can produce. Exported so a test asserts the
 *  space rather than trusting the lists' length by eye. */
export const NICKNAME_SPACE = ADJECTIVES.length * NOUNS.length;

/** Every word, so a test can hold the LISTS to the rule stated above rather
 *  than sampling the output and hoping a violation is drawn. */
export const NICKNAME_WORDS: readonly string[] = [...ADJECTIVES, ...NOUNS];

/**
 * A memorable, address-safe nickname: `<adjective>-<noun>`.
 *
 * HYPHEN, not the underscore Docker uses: an underscore is not valid in a DNS
 * label, and this becomes one.
 *
 * `random` is injectable so a test can assert the composition without asserting
 * a particular pair -- the words are chosen independently, which is what makes
 * the space the product of the two lists rather than the longer of them.
 */
export function generateNickname(random: () => number = Math.random): string {
  const pick = <T>(list: readonly T[]): T => list[Math.floor(random() * list.length) % list.length]!;
  return `${pick(ADJECTIVES)}-${pick(NOUNS)}`;
}
