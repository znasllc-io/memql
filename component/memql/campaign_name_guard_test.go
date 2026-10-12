package memql

import (
	"strings"
	"testing"
)

// campaign_name_guard_test.go -- the caps on the names a campaign's mail shows
// (memql#6033), and the sentences a person is refused with.
//
// The end-to-end half, through the real engine and the mutations the app calls,
// is TestAnOverLongNameIsRefusedInAPlainSentence in component/campaigns.

func TestCampaignNamesOverTheirCapsAreRefusedInASentence(t *testing.T) {
	const (
		campaignSentence = "Use a campaign name of 200 characters or fewer."
		senderSentence   = "Use a sender name of 100 characters or fewer."
	)
	at := func(n int) string { return strings.Repeat("日", n) } // three bytes a character

	for name, tc := range map[string]struct {
		concept string
		payload map[string]any
		want    string // "" is accepted
	}{
		"a campaign name at the cap":           {conceptCampaignsCampaign, map[string]any{"name": at(200)}, ""},
		"a campaign name one over":             {conceptCampaignsCampaign, map[string]any{"name": at(201)}, campaignSentence},
		"a campaign's sender name at the cap":  {conceptCampaignsCampaign, map[string]any{"name": "ok", "fromName": at(100)}, ""},
		"a campaign's sender name one over":    {conceptCampaignsCampaign, map[string]any{"name": "ok", "fromName": at(101)}, senderSentence},
		"an identity's sender name at the cap": {conceptCampaignsSenderIdentity, map[string]any{"fromName": at(100)}, ""},
		"an identity's sender name one over":   {conceptCampaignsSenderIdentity, map[string]any{"fromName": at(101)}, senderSentence},
		// The name is judged first, so a row with both over reads one sentence, not two.
		"both over": {conceptCampaignsCampaign, map[string]any{"name": at(300), "fromName": at(300)}, campaignSentence},
		// Bytes are not characters: 200 three-byte characters are 600 bytes.
		"a name that is long in bytes only": {conceptCampaignsCampaign, map[string]any{"name": at(200)}, ""},
		// Nothing else is this guard's business.
		"another concept":                       {"v1:campaigns:template", map[string]any{"name": at(500), "fromName": at(500)}, ""},
		"a campaign with no names in the write": {conceptCampaignsCampaign, map[string]any{"status": "paused"}, ""},
		"a name that is not a string":           {conceptCampaignsCampaign, map[string]any{"name": 12}, ""},
		"no payload":                            {conceptCampaignsCampaign, nil, ""},
		"an identity's name field is fromName":  {conceptCampaignsSenderIdentity, map[string]any{"name": at(500)}, ""},
	} {
		err := validateCampaignNameLengths(tc.concept, tc.payload)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", name, err)
		case tc.want != "" && (err == nil || err.Error() != tc.want):
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

// TestTheNameCapsAreTheNumbersTheSentencesSay: the sentences are built from the
// constants, so a cap cannot change and leave a sentence telling people the old
// number.
func TestTheNameCapsAreTheNumbersTheSentencesSay(t *testing.T) {
	if CampaignNameMaxRunes != 200 || SenderNameMaxRunes != 100 {
		t.Fatalf("the caps are %d and %d: dsl/campaigns declares 200 and 100 and component/campaigns sizes its tokens by them", CampaignNameMaxRunes, SenderNameMaxRunes)
	}
	if got := errCampaignNameTooLong.Error(); !strings.Contains(got, "200") {
		t.Errorf("the campaign name sentence %q does not say the cap", got)
	}
	if got := errSenderNameTooLong.Error(); !strings.Contains(got, "100") {
		t.Errorf("the sender name sentence %q does not say the cap", got)
	}
}
