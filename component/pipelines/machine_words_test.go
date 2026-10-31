package pipelines

import "testing"

// A machine is named by what its owner chose, else by the start of its id, and
// never by anything else: MachineWords is handed the owner's display name and
// nothing the machine reported, so the hostname has no way in.
func TestMachineWords(t *testing.T) {
	// A production registration id: 24 random hex characters.
	const id = "3f9c2ab01d2e4f3a8b5c6d7e"
	for _, c := range []struct {
		name, owner, id, want string
	}{
		{"the display name its owner chose", "MSI Raider", id, "MSI Raider"},
		{"the name is trimmed", "  MSI Raider  ", id, "MSI Raider"},
		{"no name: the start of the id", "", id, "machine 3f9c2ab0"},
		{"a name of only whitespace is no name", "   ", id, "machine 3f9c2ab0"},
		{"a canonical id reads as the bare one", "", "v1:worker:registration:" + id, "machine 3f9c2ab0"},
		{"an id shorter than the cut is whole", "", "laptop", "machine laptop"},
		// Byte 8 is the middle of the third e-acute, so the cut backs up to its start.
		{"a multi-byte id is never split in a rune", "", "abcéééééé", "machine abcéé"},
		{"no name and no id", "", "", "the machine"},
		{"no name and an id of only whitespace", "", "  ", "the machine"},
		{"the name wins over the id", "CyberPC", "v1:worker:registration:" + id, "CyberPC"},
	} {
		if got := MachineWords(c.owner, c.id); got != c.want {
			t.Errorf("%s: MachineWords(%q, %q) = %q, want %q", c.name, c.owner, c.id, got, c.want)
		}
	}
}
