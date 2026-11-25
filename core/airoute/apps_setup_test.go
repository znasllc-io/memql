package airoute

import "testing"

// AppIsSetUp is AppRuns with the "ready right now" tests dropped, and differs
// from it in nothing else.
func TestAppIsSetUpDiffersFromAppRunsOnlyByWhatIsTemporary(t *testing.T) {
	cases := []struct {
		name                string
		id                  string
		allowed, signedIn   bool
		readiness           string
		wantRuns, wantSetUp bool
	}{
		{"ready", AppCodex, true, true, AppReadinessReady, true, true},
		{"never measured", AppCodex, true, true, "", true, true},
		{"unknown", AppCodex, true, true, AppReadinessUnknown, false, true},
		{"out of usage", AppCodex, true, true, AppReadinessOutOfUsage, false, true},
		{"signed out", AppCodex, true, true, AppReadinessSignedOut, false, false},
		{"not allowed", AppCodex, false, true, "", false, false},
		{"not signed in", AppCodex, true, false, "", false, false},
		{"unknown id", "gemini", true, true, "", false, false},
		{"wildcard", "*", true, true, "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AppRuns(tc.id, tc.allowed, tc.signedIn, tc.readiness); got != tc.wantRuns {
				t.Fatalf("AppRuns = %v, want %v", got, tc.wantRuns)
			}
			if got := AppIsSetUp(tc.id, tc.allowed, tc.signedIn, tc.readiness); got != tc.wantSetUp {
				t.Fatalf("AppIsSetUp = %v, want %v", got, tc.wantSetUp)
			}
		})
	}
}
