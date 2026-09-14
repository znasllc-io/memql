package annotations

import (
	"strconv"
	"strings"
)

// Form is how an annotation's arguments are written. A Placement's Forms is
// the SET it accepts (a bit set); a Use's Form is the ONE the parser saw.
//
// The forms are the ones the parser can tell apart (parseAttribute in
// component/language/parser), and no more: a form the parser cannot
// distinguish from another could not be checked.
type Form uint16

const (
	// FormFlag is a bare annotation: `@serverOnly`.
	FormFlag Form = 1 << iota
	// FormEmpty is an empty argument list: `@when()`. It differs from a bare
	// flag only in how it was written, and a placement may take one and not
	// the other.
	FormEmpty
	// FormString is one quoted string: `@description("...")`.
	FormString
	// FormStrings is a list of two or more quoted strings:
	// `@createOnly("status", "attempts")`.
	FormStrings
	// FormNumber is one number: `@cache(300)`.
	FormNumber
	// FormKeywords is a list of keyword arguments, each `key=value` or a bare
	// flag key: `@rowAuthz(owner="ownerUserId", clusterOwner)`. The keys are
	// the placement's closed set.
	FormKeywords
	// FormObject is one object literal: `@args({ ... })`.
	FormObject
	// FormExpression is an expression, which only `@filter(...)` takes: the
	// lambda of `@filter(row => row.status == "open")`, parsed as a node.
	FormExpression
	// FormExclude is an exclusion list: `@name(!"a", !"b")`.
	FormExclude
	// FormBool is a bare true or false: `@default(false)`. The parser stores a
	// bare word as a flag key, so without this form a boolean value would
	// read as a keyword argument named "false".
	FormBool
)

// allForms lists every single form, in bit order.
var allForms = []Form{
	FormFlag, FormEmpty, FormString, FormStrings, FormNumber, FormKeywords,
	FormObject, FormExpression, FormExclude, FormBool,
}

// formWords are the single forms in words, as a refusal says them.
var formWords = map[Form]string{
	FormFlag:       "no arguments",
	FormEmpty:      "empty parentheses",
	FormString:     "one string",
	FormStrings:    "a list of strings",
	FormNumber:     "one number",
	FormKeywords:   "keyword arguments",
	FormObject:     "an object literal",
	FormExpression: "an expression",
	FormExclude:    "an exclusion list",
	FormBool:       "true or false",
}

// String renders the form set in words, joined with "or": "one number or
// keyword arguments". One string or a list of them reads "one or more
// strings", which is what an author means by it.
func (f Form) String() string {
	var parts []string
	rest := f
	if f&(FormString|FormStrings) == FormString|FormStrings {
		parts = append(parts, "one or more strings")
		rest &^= FormString | FormStrings
	}
	for _, single := range allForms {
		if rest&single != 0 {
			parts = append(parts, formWords[single])
			rest &^= single
		}
	}
	if rest != 0 {
		parts = append(parts, "Form("+strconv.Itoa(int(rest))+")")
	}
	switch len(parts) {
	case 0:
		return "nothing"
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " or " + parts[1]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " or " + parts[len(parts)-1]
}
