package compiler

import (
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

func TestRetiredRuntimeReferencesRemainStringLiterals(t *testing.T) {
	c := NewDefault()
	for _, value := range []string{"$steps.a.result", "$item.id", "$input.path", "$var.X", "$timestamp", "$error", "item.id", "input.path", " input.path "} {
		if got := c.valueToString(value); got != parser.QuoteString(value) {
			t.Errorf("valueToString(%q)=%q; want a quoted string", value, got)
		}
	}
}
