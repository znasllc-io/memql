package access

import (
	"encoding/json"
	"io"
)

// ===========================================================================
// THE MACHINE-READABLE HALF OF "AN ABSENCE IS NOT AN EMPTY VALUE"
// ===========================================================================
// JSON makes the confusion EASIER than prose does, because a missing key and a
// false one are both falsey to every caller that reads them. A script asking
// "may this machine deploy?" must not read a cluster that never mentioned
// roles as a person who holds none.
//
// So every block that can be absent carries BOTH flags the text report
// distinguishes, and they answer different questions:
//
//	on_wire   does THIS BUILD's wire carry the field at all? False means no
//	          cluster could have sent one -- the contract predates it.
//	reported  did a value actually arrive in THIS response?
//
// A block's contents appear only when `reported` is true, so there is no shape
// in which a caller sees `"slug": ""` and has to guess which it means.
//
// TWO THINGS ARE DELIBERATELY NOT `omitempty`. `rank` is a POINTER because 0 is
// a real rank meaning "holds nothing", and `omitempty` on a plain int would
// delete precisely the value a caller most needs. `items` and `account_ids`
// are emitted as `[]` rather than dropped, because a dropped key is a THIRD
// shape that means the opposite of the second: a jq script doing
// `.groups.items[]` would die on the ordinary account-less user, and a caller
// could not tell "in no groups" from "asked a cluster that does not answer".

type jsonSummary struct {
	Cluster      string     `json:"cluster"`
	UserID       string     `json:"user_id,omitempty"`
	PrimaryEmail string     `json:"primary_email,omitempty"`
	DisplayName  string     `json:"display_name,omitempty"`
	SessionID    string     `json:"session_id,omitempty"`
	Role         jsonRole   `json:"role"`
	Groups       jsonGroups `json:"groups"`
	Scope        jsonScope  `json:"account_scope"`
}

type jsonRole struct {
	OnWire   bool   `json:"on_wire"`
	Reported bool   `json:"reported"`
	Slug     string `json:"slug,omitempty"`
	Name     string `json:"name,omitempty"`
	Rank     *int32 `json:"rank,omitempty"`
}

type jsonGroups struct {
	OnWire   bool        `json:"on_wire"`
	Reported bool        `json:"reported"`
	Items    []jsonGroup `json:"items"`
}

type jsonGroup struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name,omitempty"`
	Kind        string `json:"kind,omitempty"`
	AccountID   string `json:"account_id,omitempty"`
	AccountName string `json:"account_name,omitempty"`
}

type jsonScope struct {
	OnWire       bool     `json:"on_wire"`
	Reported     bool     `json:"reported"`
	AccountIDs   []string `json:"account_ids"`
	EveryAccount *bool    `json:"every_account,omitempty"`
}

// RenderJSON writes the access report as JSON.
func RenderJSON(w io.Writer, s Summary, cluster string) error {
	out := jsonSummary{
		Cluster:      cluster,
		UserID:       s.UserID,
		PrimaryEmail: s.PrimaryEmail,
		DisplayName:  s.DisplayName,
		SessionID:    s.SessionID,
		Role:         jsonRole{OnWire: s.Role.OnTheWire, Reported: s.Role.Reported},
		Groups:       jsonGroups{OnWire: s.Groups.OnTheWire, Reported: s.Groups.Reported},
		Scope:        jsonScope{OnWire: s.Scope.OnTheWire, Reported: s.Scope.Reported},
	}
	if s.Role.Reported {
		out.Role.Slug = s.Role.Slug
		out.Role.Name = s.Role.Name
		if s.Role.HasRank {
			rank := s.Role.Rank
			out.Role.Rank = &rank
		}
	}
	// ALWAYS a list, never nil and never absent -- in every state, reported or
	// not. `null` would be a third shape, and `.groups.items[]` dies on it just
	// as it dies on a missing key.
	out.Groups.Items = []jsonGroup{}
	out.Scope.AccountIDs = []string{}
	if s.Groups.Reported {
		for _, g := range s.Groups.Items {
			out.Groups.Items = append(out.Groups.Items, jsonGroup(g))
		}
	}
	if s.Scope.Reported {
		if s.Scope.AccountIDs != nil {
			out.Scope.AccountIDs = s.Scope.AccountIDs
		}
		every := s.Scope.EveryAccount
		out.Scope.EveryAccount = &every
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
