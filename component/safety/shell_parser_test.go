package safety

import (
	"reflect"
	"testing"
)

func TestSegmentCommand(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   \t\n", nil},
		{"ls", []string{"ls"}},
		{"ls -la /tmp", []string{"ls -la /tmp"}},
		{"ls; cat foo", []string{"ls", "cat foo"}},
		{"a && b", []string{"a", "b"}},
		{"a || b", []string{"a", "b"}},
		{"a | b | c", []string{"a", "b", "c"}},
		{"a && b || c; d & e\nf", []string{"a", "b", "c", "d", "e", "f"}},
		// Quotes shield separators.
		{`echo "a; b" ; ls`, []string{`echo "a; b"`, "ls"}},
		{`echo 'a | b' | cat`, []string{`echo 'a | b'`, "cat"}},
		// Trailing separator yields no extra empty segment.
		{"ls;", []string{"ls"}},
		// Mixed && and & isn't ambiguous to our matcher (longer first).
		{"a && b & c", []string{"a", "b", "c"}},
	}
	for _, tc := range cases {
		got := SegmentCommand(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("SegmentCommand(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

func TestExtractCommandBinary(t *testing.T) {
	cases := map[string]string{
		"":                         "",
		"   ":                      "",
		"ls":                       "ls",
		"ls -la":                   "ls",
		"/usr/bin/python3 main.py": "python3",
		"./script.sh arg":          "script.sh",
		"FOO=1 ls":                 "ls",
		"FOO=1 BAR=2 BAZ=3 ls -la": "ls",
		"FOO=1":                    "",       // assignment-only segment
		"FOO=1 BAR=2":              "",       // multiple assignments, no binary
		"=value":                   "=value", // not an assignment (empty key)
	}
	for in, want := range cases {
		if got := ExtractCommandBinary(in); got != want {
			t.Errorf("ExtractCommandBinary(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCommandBinaries(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"ls -la /tmp", []string{"ls"}},
		{"cat foo | grep bar", []string{"cat", "grep"}},
		{"FOO=1 /usr/bin/python3 main.py && ./go-thing run", []string{"python3", "go-thing"}},
		{"", nil},
		{"FOO=1", nil},
	}
	for _, tc := range cases {
		got := CommandBinaries(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("CommandBinaries(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

func TestContainsSubshell(t *testing.T) {
	cases := map[string]bool{
		"ls":                      false,
		"cat foo | grep bar":      false,
		`echo "$(date)"`:          true,  // $(...) inside double quotes still interpolates
		`echo '$(date)'`:          false, // single quotes don't interpolate
		"echo `date`":             true,  // backticks
		"cat <<EOF\nhi\nEOF":      true,  // here-doc
		"grep <<< 'inline'":       true,  // here-string (also caught by <<)
		`echo "no subshell here"`: false,
		"a $(b) c":                true,
		"a `b` c":                 true,
		"foo$bar":                 false, // bare $ without ( isn't a subshell
	}
	for in, want := range cases {
		if got := ContainsSubshell(in); got != want {
			t.Errorf("ContainsSubshell(%q) = %v, want %v", in, got, want)
		}
	}
}
