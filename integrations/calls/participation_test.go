package calls

import (
	callrules "github.com/znasllc-io/memql/component/calls"
	"testing"
)

func TestParticipationDeclarationRequiresAdultSupervisorAndAuthority(t *testing.T) {
	for _, tc := range []struct {
		name    string
		c       callrules.Confirmation
		allowed bool
	}{
		{"adult legacy", callrules.Confirmation{Adult: true, NoMinors: true, Terms: "draft"}, true},
		{"adult scope", callrules.Confirmation{Adult: true, NoMinors: true, Terms: "draft", Participation: "adults_only"}, true},
		{"supervised", callrules.Confirmation{Adult: true, Terms: "draft", Participation: "children_present", AuthorityAccepted: true}, false},
		{"missing adult", callrules.Confirmation{Terms: "draft", Participation: "children_present", AuthorityAccepted: true}, false},
		{"missing authority", callrules.Confirmation{Adult: true, Terms: "draft", Participation: "children_present"}, false},
		{"unknown scope", callrules.Confirmation{Adult: true, NoMinors: true, Terms: "draft", Participation: "anything"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, c, err := checkJoiner("Supervisor", tc.c)
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v error=%v", tc.allowed, err)
			}
			if err == nil && c != tc.c {
				t.Fatal("declaration was overwritten")
			}
		})
	}
}
