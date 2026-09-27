package work

import (
	"errors"
	"strings"
	"testing"
)

// "A section ending mid-effect is refused" (#5418 acceptance).
func TestASectionEndingMidEffectIsRefused(t *testing.T) {
	err := CheckSectionBoundaries([]Section{
		{Name: "gather", Purpose: "gather the invoices", Outputs: []string{"invoices"}},
		{Name: "upload", Purpose: "upload the report", Effects: Footprint{Files: true}},
	})
	var be *SectionBoundaryError
	if !errors.As(err, &be) || be.Section != "upload" || !strings.Contains(err.Error(), "mid-effect") {
		t.Fatalf("err = %v, want the upload section refused as mid-effect", err)
	}
}

func TestASectionWithNoEndIsRefused(t *testing.T) {
	err := CheckSectionBoundaries([]Section{{Name: "think", Purpose: "think about it"}})
	var be *SectionBoundaryError
	if !errors.As(err, &be) || !strings.Contains(err.Error(), "no end") {
		t.Fatalf("err = %v, want a section with neither outputs nor a postcondition refused", err)
	}
}

func TestSectionsThatEndOnACheckPass(t *testing.T) {
	err := CheckSectionBoundaries([]Section{
		{Name: "gather", Purpose: "gather the invoices", Outputs: []string{"invoices"}},
		{Name: "write", Purpose: "write the report file", Effects: Footprint{Files: true}, Postcondition: "the report file exists"},
		{Name: "record", Purpose: "record the totals", Effects: Footprint{Concepts: []string{"v1:ledger:entry"}}, Postcondition: "rowWritten:v1:ledger:entry", Outputs: []string{"entryId"}},
	})
	if err != nil {
		t.Fatalf("every section ends on a checkable state: %v", err)
	}
}

// "A section with a catalogued reusable automation spends no model" (#5418
// acceptance).
func TestASectionWithACataloguedReusableAutomationSpendsNoModel(t *testing.T) {
	s := Section{Name: "summarise", Purpose: "summarise the month's invoices", Inputs: []string{"month"}, Outputs: []string{"summary"}}
	exact := map[string][]SectionCandidate{
		SectionSignature(s): {{ConstructId: "c1", Name: "summariseInvoices", Reuse: "reusable", Args: []string{"month"}}},
	}
	plan := DecideSections([]Section{s}, exact, nil, 0)
	if plan.NeedsModel {
		t.Fatal("a plan whose every section the catalog serves needs no model")
	}
	if d := plan.Sections[0]; d.Route != SectionCatalogExact || d.Candidate == nil || d.Candidate.Name != "summariseInvoices" {
		t.Errorf("decision = %+v", d)
	}
}

func TestANearHitIsReusableCoversTheInputsAndSpendsNoModel(t *testing.T) {
	s := Section{Name: "summarise", Purpose: "summarise the monthly invoices", Inputs: []string{"month"}, Outputs: []string{"summary"}}
	reusable := []SectionCandidate{
		{Name: "summariseInvoicesMonthly", Reuse: "reusable", Text: "summarise monthly invoices", Args: []string{"month", "currency"}},
	}
	plan := DecideSections([]Section{s}, nil, reusable, 0)
	if d := plan.Sections[0]; d.Route != SectionCatalogNear || plan.NeedsModel {
		t.Fatalf("decision = %+v, needsModel = %v", d, plan.NeedsModel)
	}
}

func TestAGoalSpecificConstructIsNeverANearHit(t *testing.T) {
	s := Section{Name: "summarise", Purpose: "summarise the monthly invoices", Inputs: []string{"month"}, Outputs: []string{"summary"}}
	candidates := []SectionCandidate{
		{Name: "a", Reuse: "goalSpecific", Text: "summarise the monthly invoices", Args: []string{"month"}},
		{Name: "b", Reuse: "accountSpecific", Text: "summarise the monthly invoices", Args: []string{"month"}},
		{Name: "c", Reuse: "", Text: "summarise the monthly invoices", Args: []string{"month"}},
	}
	plan := DecideSections([]Section{s}, nil, candidates, 0)
	if plan.Sections[0].Route != SectionIntelligence || !plan.NeedsModel {
		t.Errorf("only a REUSABLE construct is a near hit; got %+v", plan.Sections[0])
	}
}

func TestANearHitMustCoverTheSectionInputs(t *testing.T) {
	s := Section{Name: "summarise", Purpose: "summarise the monthly invoices", Inputs: []string{"month", "client"}, Outputs: []string{"summary"}}
	candidates := []SectionCandidate{{Name: "a", Reuse: "reusable", Text: "summarise the monthly invoices", Args: []string{"month"}}}
	if plan := DecideSections([]Section{s}, nil, candidates, 0); plan.Sections[0].Route != SectionIntelligence {
		t.Errorf("a construct with no argument for the client would answer a different question; got %+v", plan.Sections[0])
	}
}

func TestSectionSimilarityIsSymmetricAndBounded(t *testing.T) {
	a, b := "summarise the monthly invoices", "monthly invoice summary for the team"
	if SectionSimilarity(a, b) != SectionSimilarity(b, a) {
		t.Error("similarity must be symmetric")
	}
	if got := SectionSimilarity(a, a); got != 1 {
		t.Errorf("identical purposes = %v, want 1", got)
	}
	if got := SectionSimilarity("send the email", "rotate the keys"); got != 0 {
		t.Errorf("unrelated purposes = %v, want 0", got)
	}
	if got := SectionSimilarity("", a); got != 0 {
		t.Errorf("an empty purpose = %v, want 0", got)
	}
}
