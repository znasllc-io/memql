package abuse

import "testing"

func TestIsDisposable(t *testing.T) {
	cases := []struct {
		email string
		want  bool
	}{
		{"someone@mailinator.com", true},
		{"throwaway+tag@10minutemail.com", true},
		{"user@guerrillamail.com", true},
		{"USER@MAILINATOR.COM", true}, // case-insensitive
		{"user@gmail.com", false},
		{"user@example.com", false},
		{"user@visionarys.io", false},
		{"malformed", false},
		{"", false},
		{"missing-domain@", false},
	}
	for _, tc := range cases {
		got := IsDisposable(tc.email)
		if got != tc.want {
			t.Errorf("IsDisposable(%q) = %v, want %v", tc.email, got, tc.want)
		}
	}
}
