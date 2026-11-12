package storestats

import "testing"

// WHO IS WHOLESALE, as a value. An enabled store's granted buyer is wholesale
// whatever the case and spaces of the address; another address, a blank one,
// and any address of a store that is not enabled or of the zero state are not.
// Not a database test.
func TestIsWholesale(t *testing.T) {
	granted := map[string]bool{"buyer@example.com": true}
	for _, c := range []struct {
		name  string
		state wholesaleState
		email string
		want  bool
	}{
		{"the granted buyer", wholesaleState{Enabled: true, Granted: granted}, "buyer@example.com", true},
		{"the granted buyer, another case and spaces", wholesaleState{Enabled: true, Granted: granted}, "  Buyer@Example.COM ", true},
		{"another address", wholesaleState{Enabled: true, Granted: granted}, "other@example.com", false},
		{"no address", wholesaleState{Enabled: true, Granted: map[string]bool{"": true}}, "", false},
		{"spaces only", wholesaleState{Enabled: true, Granted: map[string]bool{"": true}}, "   ", false},
		{"a store not enabled", wholesaleState{Enabled: false, Granted: granted}, "buyer@example.com", false},
		{"the zero state", wholesaleState{}, "buyer@example.com", false},
	} {
		if got := c.state.isWholesale(c.email); got != c.want {
			t.Errorf("%s: isWholesale(%q) = %v, want %v", c.name, c.email, got, c.want)
		}
	}
}
