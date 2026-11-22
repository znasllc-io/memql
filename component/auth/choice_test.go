package auth

import (
	"context"
	"testing"
	"time"
)

// A CHOICE IS THE PERSON'S ONLY WHEN THE PERSON MADE IT (vendor accounts
// design, K8 line 4 and owner decision O4).
//
// A route a person picks, or a source they pin, is them saying their own API
// key may pay. A call's SUBJECT is not enough to know that: an app session's
// subject is the person who owns the machine, and the coding agent holding it
// can name a vendor source on a turn or on a goal it opens. Counted as the
// person's choice, that spent their key with "Let Auto use this key" off,
// which is the one switch that says work nobody chose in person may.
func TestAChoiceIsThePersonsOnlyThroughTheirOwnCredential(t *testing.T) {
	forwarded := func(class string) context.Context {
		return forwardedSession(t, class, sessionPerson, "", time.Now().Add(time.Hour))
	}
	cases := map[string]struct {
		ctx  context.Context
		want bool
	}{
		"a signed-in session":                {session("", nil), true},
		"a session whose token names a user": {session(ForwardedClassUser, nil), true},
		"the person's own access token":      {session("", map[string]any{"pat_id": "pat-1"}), true},
		"a token of class pat":               {session(ForwardedClassPat, nil), true},
		"a forwarded session":                {forwarded(ForwardedClassUser), true},
		"a forwarded access token":           {forwarded(ForwardedClassPat), true},
		"an app session":                     {session(ForwardedClassAppSession, nil), false},
		"a forwarded app session":            {forwarded(ForwardedClassAppSession), false},
		"a service account":                  {session(ForwardedClassServiceAccount, nil), false},
		"a badge":                            {session(ForwardedClassBadge, nil), false},
		"a class that is not a string":       {session("", map[string]any{"class": 7}), false},
		"delegated access":                   {ContextWithDelegation(session("", nil), &DelegationContext{DelegationId: "v1:identity:delegation:d"}), false},
		"an assertion that says nothing":     {ContextWithForwardedAuthority(session("", nil), ForwardedAuthority{}), false},
		"the cluster acting":                 {ContextWithSystemActor(context.Background(), "sweep"), false},
		"a person's authority, borrowed":     {ContextWithUserActor(context.Background(), sessionPerson), false},
		"a connector":                        {ContextWithConnectorActor(context.Background(), "shopify"), false},
		"nobody":                             {context.Background(), false},
		"nobody signed in":                   {ContextWithAnonymousActor(context.Background()), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ChoiceIsThePersons(tc.ctx); got != tc.want {
				t.Errorf("ChoiceIsThePersons = %v, want %v", got, tc.want)
			}
		})
	}
}
