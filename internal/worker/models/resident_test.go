package models

import (
	"strings"
	"testing"
	"time"
)

// resident_test.go pins how an owner's words about keeping a model loaded
// become what Ollama is told.
//
// The words are written by hand in policy.yaml, and the most natural way
// to write "never unload" is `keep_alive: -1`, which YAML reads as an
// integer. Every spelling below is one an owner will actually type.

func TestParseKeepAlive(t *testing.T) {
	tests := []struct {
		in       string
		want     KeepAlive
		wantWire any
		wantText string
	}{
		// Not said: nothing is sent and Ollama's own default stands.
		{"", KeepAlive{}, nil, ""},
		{"   ", KeepAlive{}, nil, ""},
		// Forever, in every spelling Ollama itself accepts plus the word.
		{"-1", KeepAlive{Set: true, Forever: true}, -1, "forever"},
		{"-5", KeepAlive{Set: true, Forever: true}, -1, "forever"},
		{"-1m", KeepAlive{Set: true, Forever: true}, -1, "forever"},
		{"forever", KeepAlive{Set: true, Forever: true}, -1, "forever"},
		{"Forever", KeepAlive{Set: true, Forever: true}, -1, "forever"},
		// Past int64 in the negative direction is still negative, so still forever.
		{"-99999999999999999999", KeepAlive{Set: true, Forever: true}, -1, "forever"},
		// A Go duration, as Ollama reads one.
		{"30m", KeepAlive{Set: true, For: 30 * time.Minute}, "30m0s", "30m0s"},
		{"2h", KeepAlive{Set: true, For: 2 * time.Hour}, "2h0m0s", "2h0m0s"},
		{"90s", KeepAlive{Set: true, For: 90 * time.Second}, "1m30s", "1m30s"},
		// A bare positive integer is SECONDS, as Ollama reads a number.
		{"3600", KeepAlive{Set: true, For: time.Hour}, "1h0m0s", "1h0m0s"},
		// An integer may carry one sign, and a plus is still an integer.
		{"+5", KeepAlive{Set: true, For: 5 * time.Second}, "5s", "5s"},
		// The cap is 30 days exactly, and it is accepted in both spellings.
		{"720h", KeepAlive{Set: true, For: MaxKeepAlive}, "720h0m0s", "720h0m0s"},
		{"2592000", KeepAlive{Set: true, For: MaxKeepAlive}, "720h0m0s", "720h0m0s"},
	}
	for _, tt := range tests {
		got, err := ParseKeepAlive(tt.in)
		if err != nil {
			t.Errorf("ParseKeepAlive(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseKeepAlive(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
		if w := got.Wire(); w != tt.wantWire {
			t.Errorf("ParseKeepAlive(%q).Wire() = %#v, want %#v", tt.in, w, tt.wantWire)
		}
		if s := got.String(); s != tt.wantText {
			t.Errorf("ParseKeepAlive(%q).String() = %q, want %q", tt.in, s, tt.wantText)
		}
	}
}

// TestParseKeepAliveRefusesWhatWouldSilentlyDoTheWrongThing. Zero is the
// one that matters: Ollama reads 0 as "unload after every call", the
// opposite of resident, and an owner who typed it meant something else.
func TestParseKeepAliveRefusesWhatWouldSilentlyDoTheWrongThing(t *testing.T) {
	tests := []struct {
		in   string
		want string // a phrase the sentence must carry
	}{
		{"0", "unloads the model after every call"},
		{"0s", "unloads the model after every call"},
		{"00", "unloads the model after every call"},
		{"soon", "not a duration"},
		{"5 minutes", "not a duration"},
		{"1.5", "not a duration"},
		// A finite value past the cap is nearly forever and almost
		// certainly a typo for -1; saying so beats picking one.
		{"9999h", "longer than 30 days"},
		{"99999999999", "longer than 30 days"},
		// One nanosecond or one second past the cap is refused.
		{"720h1ns", "longer than 30 days"},
		{"2592001", "longer than 30 days"},
		// Past int64 in the positive direction is too long, not "not a duration".
		{"99999999999999999999", "longer than 30 days"},
		// ONLY TEXT THAT IS ENTIRELY DIGITS (after one optional sign) IS AN
		// INTEGER. ParseInt reports a range error as soon as the digits overflow,
		// before it reaches a letter that follows them, so these used to read as
		// an integer past int64: the negative ones as forever, which is the
		// opposite of a refusal, and the positive ones as "longer than 30 days".
		// They are not integers and not durations Go can read, so they are typos.
		{"-99999999999999999999x", "not a duration"},
		{"-99999999999999999999h", "not a duration"},
		{"-99999999999999999999.5", "not a duration"},
		{"99999999999999999999x", "not a duration"},
		{"99999999999999999999h", "not a duration"},
		{"+-5", "not a duration"},
		{"-", "not a duration"},
	}
	for _, tt := range tests {
		got, err := ParseKeepAlive(tt.in)
		if err == nil {
			t.Errorf("ParseKeepAlive(%q) = %+v, want a refusal", tt.in, got)
			continue
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("ParseKeepAlive(%q) error %q does not say %q", tt.in, err, tt.want)
		}
		if got.Set {
			t.Errorf("ParseKeepAlive(%q) refused but still returned a setting: %+v", tt.in, got)
		}
	}
}
