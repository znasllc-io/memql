package hardware

import (
	"context"
	"testing"
)

// Version output is not a format anybody promised. These are the real
// greetings, captured, and the parser has to survive all of them --
// including the one that carries no version at all, which reports
// "present, version unknown" rather than lying about a number.
func TestParseRuntimeVersion(t *testing.T) {
	for _, tc := range []struct{ name, out, want string }{
		{"mflux", "mflux-generate, version 0.9.1\n", "0.9.1"},
		{"whisper banner", "whisper-cli version 1.7.2 (build 4a9f)\n", "1.7.2"},
		{"bare number", "0.13.0\n", "0.13.0"},
		{"two segments", "v1.7\n", "1.7"},
		{"four segments", "tool 2.1.0.3\n", "2.1.0.3"},
		{"no version anywhere", "usage: whisper-cli [options]\n", ""},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRuntimeVersion(tc.out); got != tc.want {
				t.Fatalf("parseRuntimeVersion(%q) = %q, want %q", tc.out, got, tc.want)
			}
		})
	}
}

// The docker probe reads the SERVER version, so its output is one line
// and firstLine must not fold a trailing warning into it.
func TestFirstLine(t *testing.T) {
	got := firstLine("27.3.1\nWARNING: daemon is in swarm mode\n")
	if got != "27.3.1" {
		t.Fatalf("firstLine = %q, want 27.3.1", got)
	}
}

// THE THREE SHAPES PEOPLE ACTUALLY WRITE. Accepting only a full URL is
// not a stricter parser, it is a probe that reports a runtime ABSENT
// while it is serving: url.Parse reads "127.0.0.1:11434/api/version" as
// a scheme named "127.0.0.1", the request fails, and the machine says
// it has no Ollama while `memql worker models` lists five.
func TestBaseURLFrom(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// A full URL, kept as written.
		{"http://box.local:9000", "http://box.local:9000"},
		{"https://box.local:9000", "https://box.local:9000"},
		{"http://box.local:9000/", "http://box.local:9000"},
		// A host:port, which is not a URL and is what a person types.
		{"127.0.0.1:11434", "http://127.0.0.1:11434"},
		{"box.local:9000", "http://box.local:9000"},
		// A bare host, which gets the runtime's own default port. Port
		// 80 is not where anybody's Ollama is.
		{"box.local", "http://box.local:7777"},
		{"127.0.0.1", "http://127.0.0.1:7777"},
		{"  box.local  ", "http://box.local:7777"},
	} {
		if got := baseURLFrom(tc.in, "7777", "FALLBACK"); got != tc.want {
			t.Errorf("baseURLFrom(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := baseURLFrom("", "7777", "FALLBACK"); got != "FALLBACK" {
		t.Errorf("empty = %q, want the fallback", got)
	}
}

// The two resolvers MUST agree with the models package's, because they
// answer the same question -- and a machine whose hardware inventory
// and model inventory disagree about where Ollama is reports a runtime
// absent while advertising its models.
func TestBaseURLsAcceptWhatTheModelsPackageAccepts(t *testing.T) {
	t.Setenv("MEMQL_KOKORO_HOST", "")
	if got := KokoroBaseURL(); got != DefaultKokoroBaseURL {
		t.Fatalf("default kokoro = %q, want %q", got, DefaultKokoroBaseURL)
	}
	t.Setenv("MEMQL_KOKORO_HOST", "127.0.0.1:8880")
	if got := KokoroBaseURL(); got != "http://127.0.0.1:8880" {
		t.Fatalf("kokoro host:port = %q", got)
	}

	t.Setenv("OLLAMA_HOST", "127.0.0.1")
	if got := ollamaBaseURL(); got != "http://127.0.0.1:11434" {
		t.Fatalf("ollama bare host = %q, want the default port applied", got)
	}
	t.Setenv("OLLAMA_HOST", "box.local:11434")
	if got := ollamaBaseURL(); got != "http://box.local:11434" {
		t.Fatalf("ollama host:port = %q", got)
	}
}

// detectRuntimes must produce a byte-identical payload for an unchanged
// machine: the list is sorted, so map iteration order cannot rewrite a
// registration row on every refresh.
//
// It drives the REAL detectRuntimes rather than sortRuntimes alone,
// because the property at risk is the fan-out's map iteration order --
// a test that sorted its own slice would stay green with the sort
// deleted from the function.
func TestDetectRuntimesIsSorted(t *testing.T) {
	// Every probe points at a closed port or a binary nothing has, so
	// this reads no runtime off the machine running the tests. What it
	// asserts is the ORDER of whatever it did find, which must be
	// stable and must be sorted.
	t.Setenv("OLLAMA_HOST", "127.0.0.1:1")
	t.Setenv("MEMQL_KOKORO_HOST", "127.0.0.1:1")

	first := detectRuntimes(context.Background())
	second := detectRuntimes(context.Background())

	if len(first) != len(second) {
		t.Fatalf("two scans of one machine disagreed: %v vs %v", first, second)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("scan %d differed at %d: %v vs %v", i, i, first, second)
		}
		if i > 0 && first[i-1].Name >= first[i].Name {
			t.Fatalf("runtimes are not sorted by name: %v", first)
		}
	}
}
