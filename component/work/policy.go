package work

// policy.go -- v1:work:feedbackPolicy:primary as values (epic memql#5414; the
// design record's section 5, "Values, not constants").
//
// The seeded row carries these values and is re-asserted on every boot;
// DefaultFeedbackPolicy is only the fallback for a row that is absent or
// unreadable, and it carries the seed's values so the two cases behave alike.

// FeedbackPolicy is the values epic memql#5414 decides by.
type FeedbackPolicy struct {
	// ValidateAnswers switches the answer validator (D22) on.
	ValidateAnswers bool
	// ReusableAfterSignatures is how many distinct goal signatures make a
	// construct reusable (D24).
	ReusableAfterSignatures int
}

// DefaultFeedbackPolicy is the seed's values.
func DefaultFeedbackPolicy() FeedbackPolicy {
	return FeedbackPolicy{ValidateAnswers: true, ReusableAfterSignatures: 2}
}

// Normalize replaces a non-positive threshold with its default: a threshold of
// zero would call every construct reusable on no evidence.
func (p FeedbackPolicy) Normalize() FeedbackPolicy {
	if p.ReusableAfterSignatures <= 0 {
		p.ReusableAfterSignatures = DefaultFeedbackPolicy().ReusableAfterSignatures
	}
	return p
}

// FeedbackPolicyFrom reads the policy off a row's payload. A row that is
// absent (nil) answers the defaults; a present row's missing switch is read as
// on, because the seed always writes it and an absent value is a row this
// build did not write.
func FeedbackPolicyFrom(row map[string]any) FeedbackPolicy {
	if row == nil {
		return DefaultFeedbackPolicy()
	}
	p := DefaultFeedbackPolicy()
	if b, ok := row["validateAnswers"].(bool); ok {
		p.ValidateAnswers = b
	}
	if n := intOf(row["reusableAfterSignatures"]); n > 0 {
		p.ReusableAfterSignatures = n
	}
	return p.Normalize()
}
