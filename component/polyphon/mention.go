package polyphon

import (
	"regexp"
	"strings"
)

var mentionRegex = regexp.MustCompile(`@(\w+)`)

// referentPrepositions are words that, when they appear IMMEDIATELY before an
// @-mention, mark it as a referent (the user is talking ABOUT that participant,
// not TO them). Example: "can you tell me more about @Stella" -- Stella is the
// topic, not the addressee. The previous responder keeps the turn.
var referentPrepositions = map[string]bool{
	"about":     true,
	"regarding": true,
	"re":        true,
	"with":      true,
	"for":       true,
	"against":   true,
	"versus":    true,
	"vs":        true,
	"like":      true,
	"including": true,
	"called":    true,
	"named":     true,
	"from":      true,
}

// addresseePrefixWords are short prefix words that, when they make up the
// entire prefix (or all-but-one-word of it), still make the @-mention an
// addressee. Covers greetings and leading conversational connectives.
var addresseePrefixWords = map[string]bool{
	"hey":     true,
	"hi":      true,
	"hello":   true,
	"dear":    true,
	"yo":      true,
	"so":      true,
	"ok":      true,
	"okay":    true,
	"wait":    true,
	"um":      true,
	"well":    true,
	"alright": true,
}

// ParticipantRef is a minimal mention-lookup view of a participant. Both
// agents and humans fit into it so ParseMentions can detect @-mentions that
// target either. The handler assembles a combined slice from agent candidates
// plus the human roster before calling.
type ParticipantRef struct {
	ID              string
	Name            string
	ParticipantType string // "agent" or "human"
}

// ParseMentions scans utterance text for @{name} tokens and matches them
// against the participant roster. Returns detected mentions and cleaned text
// with @ prefixes removed for downstream scoring. Mentions carry
// ParticipantType so the router can distinguish "addressed an agent" (that
// agent responds) from "addressed a human" (AI stays silent).
func ParseMentions(text string, participants []ParticipantRef) ([]Mention, string) {
	if text == "" || len(participants) == 0 {
		return nil, text
	}

	// Build a name lookup: lowercase first-name -> participant ref.
	nameLookup := make(map[string]*ParticipantRef, len(participants))
	for i := range participants {
		// Use first token of name for matching (e.g., "Sofia" from "Sofia AI").
		parts := strings.Fields(participants[i].Name)
		if len(parts) > 0 {
			nameLookup[strings.ToLower(parts[0])] = &participants[i]
		}
	}

	matches := mentionRegex.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil, text
	}

	var mentions []Mention
	cleanText := text

	for _, match := range matches {
		// match[0]:match[1] is the full @name, match[2]:match[3] is the captured name.
		name := text[match[2]:match[3]]
		nameLower := strings.ToLower(name)

		ref, found := nameLookup[nameLower]
		if !found {
			continue
		}

		// Decide addressee vs reference. The prefix (text before the @name)
		// carries the grammatical cue: "about @Stella" = referent (topic),
		// "hey @Stella" = addressee. Default to reference unless we see a
		// clear address signal; false-addressee errors are worse than
		// false-reference (the router can pick the reference up as a topic
		// hint, but a false addressee hijacks the turn from the previous
		// responder).
		prefix := strings.TrimSpace(text[:match[0]])
		suffix := strings.TrimSpace(text[match[1]:])
		position := "mid"
		if prefix == "" || isGreetingPrefix(prefix) {
			position = "start"
		}

		role := classifyMentionRole(prefix, suffix, position)

		participantType := ref.ParticipantType
		if participantType == "" {
			participantType = "agent"
		}

		mentions = append(mentions, Mention{
			ParticipantId:   ref.ID,
			Name:            ref.Name,
			ParticipantType: participantType,
			Position:        position,
			Role:            role,
		})
	}

	// Clean text: strip @ prefixes for scoring.
	cleanText = mentionRegex.ReplaceAllString(text, "$1")

	return mentions, cleanText
}

// isGreetingPrefix checks if the text before a mention is just a greeting word.
func isGreetingPrefix(s string) bool {
	lower := strings.ToLower(strings.TrimSpace(s))
	switch lower {
	case "hey", "hi", "hello", "dear", "yo":
		return true
	}
	return false
}

// classifyMentionRole picks addressee vs reference for a parsed @-mention.
// Decision uses BOTH grammatical context before the mention (referent
// prepositions, short leading connectives) AND the shape of what comes
// after it (interrogative auxiliaries, polite imperatives, question
// words). The latter catches the common "I'm talking to A, then break to
// ask B something" pattern where the @B sits mid-utterance:
//
//	"alright before we do that @Sofia can you change the appearance"
//
// without a suffix check, the only signal is "...that" (5-token prefix,
// neither greeting nor referent prep), which falls to the default
// reference and silently routes the request to the previous responder.
// Looking at "can you ..." after @Sofia clearly indicates Sofia is the
// addressee.
func classifyMentionRole(prefix, suffix, position string) MentionRole {
	// Start-of-utterance mentions are almost always addressees.
	if position == "start" {
		return MentionRoleAddressee
	}

	// Look at the token immediately preceding the @-mention. That word
	// carries the strongest grammatical signal.
	prefixLower := strings.ToLower(prefix)
	tokens := strings.Fields(prefixLower)
	if len(tokens) == 0 {
		return MentionRoleAddressee
	}
	prev := strings.TrimRight(tokens[len(tokens)-1], ",.!?:;")

	// Referent preposition wins: "about @X" / "regarding @X" is
	// unambiguously topic-about-X, not address-to-X. This is the case the
	// current user flagged: "can you tell me more about @Stella" after
	// Jade responded -- Stella is the topic, Jade still has the turn.
	if referentPrepositions[prev] {
		return MentionRoleReference
	}

	// Short leading prefix made entirely of connective / greeting words
	// ("hey @Stella", "so @Jade", "ok @Vale") counts as addressee even
	// though position is "mid" after the comma-trim above.
	if len(tokens) <= 2 && addresseePrefixWords[prev] {
		return MentionRoleAddressee
	}

	// Suffix shape -- the words AFTER the @-mention. Imperative /
	// interrogative starters mean the user is talking TO the mentioned
	// agent, regardless of how long the lead-in was.
	if isAddresseeSuffix(suffix) {
		return MentionRoleAddressee
	}

	// Ambiguous mid-utterance mention: default to reference. The router
	// prompt has conversation context (previous responder, transcript)
	// and can decide whether to treat it as address based on that. Being
	// conservative here prevents @-mentioned referents from hijacking a
	// turn away from the agent the user is actually talking to.
	return MentionRoleReference
}

// isAddresseeSuffix reports whether the text immediately following an
// @-mention has the shape of an instruction or question DIRECTED AT
// that mention. Patterns covered:
//
//   - Auxiliary-verb questions: "can you", "could you", "would you",
//     "will you", "do you", "did you", "are you", "is it", "should you"
//   - Polite imperatives: "please", "please,"
//   - Question words: "what", "how", "when", "where", "why", "who",
//     "which"
//   - Vocative comma followed by an instruction starter
//
// The existing classifier already handles "ask/tell @X about Y"-style
// referent suffixes via the prefix's referent prepositions check;
// suffix logic only fires when prefix is non-referential, so we don't
// risk overriding "tell me about @Sofia how it works" -> reference.
func isAddresseeSuffix(suffix string) bool {
	if suffix == "" {
		return false
	}
	// Trim a leading vocative comma so "@Sofia, can you ..." reads
	// the same as "@Sofia can you ...".
	trimmed := strings.TrimLeft(suffix, " ,.:;-")
	lower := strings.ToLower(trimmed)
	if lower == "" {
		return false
	}
	addresseeStarters := []string{
		"can you", "could you", "would you", "will you", "won't you",
		"can we", "could we", "would we", "should you", "should we",
		"do you", "did you", "are you", "were you", "is it", "was it",
		"please ", "please,",
		"let's ", "let us ", "lets ",
		"show me", "tell me", "give me", "send me", "help me",
		"what", "how", "when", "where", "why", "who", "which",
	}
	for _, start := range addresseeStarters {
		if strings.HasPrefix(lower, start) {
			// Disambiguate "what" vs "what's" (question word vs
			// possessive). Both should count as questions.
			return true
		}
	}
	return false
}
