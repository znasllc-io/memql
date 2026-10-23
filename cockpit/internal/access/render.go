package access

import (
	"fmt"
	"io"
	"strings"
)

// ===========================================================================
// WHAT THIS SURFACE IS FOR
// ===========================================================================
// It answers one question -- *what am I on this cluster, and why was I just
// refused?* -- and the reason it is a command rather than a portal page is
// that the portal renders what the CLUSTER believes about a browser session,
// while a person hitting a refusal from this machine wants what the cluster
// believes about THIS credential. Those differ exactly when it matters: a
// stale token, a PAT with a different ceiling, a second account.
//
// ===========================================================================
// AN ABSENCE IS A SENTENCE, NEVER A BLANK
// ===========================================================================
// Every field here has a state where the cluster said nothing, and a blank or
// a dash reads as the OPPOSITE claim -- "you hold no role" instead of "this
// cluster did not say". The first sends somebody to ask for a grant they
// already have. So each absence prints why it is absent, and the two never
// render alike. It is the same rule probe.Figure keeps for a measurement that
// could not be taken.
//
// THERE ARE TWO ABSENCES, and they are not the same fact:
//
//	absentFromWire  this build's wire has no such field, so no cluster could
//	                have sent one. The engine change is named at the bottom.
//	absentFromReply this build understands the field and the cluster sent
//	                nothing in it.
//
// Only the first is explained by a pending engine issue; printing that
// explanation under the second would tell somebody to wait for a change their
// cluster already has.

// labelWidth aligns the value column. Wide enough for the longest label
// ("Account scope") plus breathing room.
const labelWidth = 16

const (
	absentFromWire  = "not reported by this cluster"
	absentFromReply = "none reported"
)

// Render writes the human-readable access report.
func Render(w io.Writer, s Summary, cluster string) {
	fmt.Fprintf(w, "Your access on %q\n\n", cluster)

	fmt.Fprintf(w, "  %-*s%s\n", labelWidth, "Signed in as", signedInAs(s))
	if s.UserID != "" {
		fmt.Fprintf(w, "  %-*s%s\n", labelWidth, "User id", s.UserID)
	}
	fmt.Fprintf(w, "  %-*s%s\n", labelWidth, "Session", sessionLine(s))
	fmt.Fprintln(w, "")

	renderRole(w, s.Role)
	renderGroups(w, s.Groups)
	renderScope(w, s.Scope)

	if notes := pendingNotes(s); len(notes) > 0 {
		fmt.Fprintln(w, "")
		for _, n := range notes {
			fmt.Fprintln(w, n)
		}
	}
}

// signedInAs never contradicts the User id line below it. A resolved user with
// no name and no email is the state the wire documents as ordinary -- a first
// request racing the registration insert, a PAT with no provisioned user --
// and saying "no user row" directly above a populated id is the report
// disagreeing with itself in adjacent rows.
func signedInAs(s Summary) string {
	switch {
	case s.DisplayName != "" && s.PrimaryEmail != "":
		return fmt.Sprintf("%s <%s>", s.DisplayName, s.PrimaryEmail)
	case s.PrimaryEmail != "":
		return s.PrimaryEmail
	case s.DisplayName != "":
		return s.DisplayName
	case s.UserID != "":
		return "(this cluster reports no name or email for you)"
	default:
		return "(the cluster resolved no user row for this credential)"
	}
}

func sessionLine(s Summary) string {
	if s.SessionID == "" {
		// A PAT, an operator key or a service account has no session row to
		// name. MyAccessResult's own comment calls this out as not an error.
		return "none — this credential carries no session"
	}
	return s.SessionID + "  (this device)"
}

func renderRole(w io.Writer, r Role) {
	if !r.Reported {
		fmt.Fprintf(w, "  %-*s%s\n", labelWidth, "Role", roleAbsence(r))
		return
	}
	fmt.Fprintf(w, "  %-*s%s\n", labelWidth, "Role", roleHeadline(r))
	if detail := roleDetail(r); detail != "" {
		fmt.Fprintf(w, "  %-*s%s\n", labelWidth, "", detail)
	}
}

func roleAbsence(r Role) string {
	if !r.OnTheWire {
		return absentFromWire
	}
	// This build understands `role` and the cluster sent none. Every user row
	// carries one, so this is a node behind on the change or a credential with
	// no user behind it -- not a person whose role is blank.
	return "none — this cluster sent no role for this credential"
}

// roleHeadline is the name when there is one, else the slug.
func roleHeadline(r Role) string {
	if r.Name != "" {
		return r.Name
	}
	if r.Slug != "" {
		return r.Slug
	}
	return "(the cluster reported an empty role)"
}

// roleDetail is the slug-and-rank line under the headline. The slug is
// repeated when a name stood in for it above, because the slug is what every
// grant, invitation and delegation ceiling is written in terms of.
func roleDetail(r Role) string {
	parts := make([]string, 0, 2)
	if r.Name != "" && r.Slug != "" {
		parts = append(parts, r.Slug)
	}
	if r.HasRank {
		parts = append(parts, rankPhrase(r.Rank))
	}
	return strings.Join(parts, " · ")
}

// rankPhrase spells the rank, and explains it AT rank 0 rather than on a line
// of its own.
//
// Rank 0 is the design record's unknown slug: the resolver treats the holder
// as holding nothing, everywhere, until they are re-roled -- the documented
// consequence of the cluster-owner write escape. It is the single fact that
// explains every refusal the reader is about to hit, so a bare "rank 0" buries
// it. Saying it twice is no better: two lines both beginning "rank 0" read as
// two different facts about two different things.
func rankPhrase(rank int32) string {
	if rank == 0 {
		return "rank 0 — this role holds no permissions on this cluster"
	}
	return fmt.Sprintf("rank %d", rank)
}

func renderGroups(w io.Writer, g Groups) {
	// len==0 is folded in with !Reported on purpose: Render is public and takes
	// a plain struct, and a "reported" block with nothing in it would otherwise
	// print no line at all -- a blank where a sentence belongs.
	if !g.Reported || len(g.Items) == 0 {
		fmt.Fprintf(w, "  %-*s%s\n", labelWidth, "Groups", blockAbsence(g.OnTheWire))
		return
	}
	for i, item := range g.Items {
		label := "Groups"
		if i > 0 {
			label = ""
		}
		fmt.Fprintf(w, "  %-*s%s\n", labelWidth, label, groupLine(item))
	}
}

// groupLine shows the id as well as the name, for the reason roleDetail
// repeats the slug: the id is what a person quotes to whoever administers
// grants, and it is the only thing that tells two same-named groups apart.
// It is not repeated when it is already standing in for a missing name.
//
// It NEVER returns a blank or a dangling clause. A membership whose group row
// was archived, or a `MyAccessGroup` that landed with different field names,
// would otherwise render as whitespace -- indistinguishable from a bug, and a
// direct contradiction of this file's own rule.
func groupLine(g Group) string {
	var b strings.Builder
	switch {
	case g.Name != "":
		b.WriteString(g.Name)
		if g.Kind != "" {
			b.WriteString(" (" + g.Kind + ")")
		}
		if g.ID != "" {
			b.WriteString(" · " + g.ID)
		}
	case g.ID != "":
		b.WriteString(g.ID)
		if g.Kind != "" {
			b.WriteString(" (" + g.Kind + ")")
		}
	default:
		b.WriteString("(a group this cluster named neither by id nor by name)")
	}
	if account := g.AccountName; account != "" {
		b.WriteString(" — " + account)
	} else if g.AccountID != "" {
		b.WriteString(" — " + g.AccountID)
	}
	return b.String()
}

func renderScope(w io.Writer, s Scope) {
	if !s.Reported {
		fmt.Fprintf(w, "  %-*s%s\n", labelWidth, "Account scope", blockAbsence(s.OnTheWire))
		return
	}
	fmt.Fprintf(w, "  %-*s%s\n", labelWidth, "Account scope", scopeLine(s))
}

// scopeLine keeps the named accounts ALONGSIDE the staff flag. --json emits
// both, and a command whose job is to be believed must not have its two
// renderings disagree about one record: a staff account that also holds
// explicit per-account grants would otherwise be invisible in the human half.
func scopeLine(s Scope) string {
	named := strings.Join(s.AccountIDs, ", ")
	if s.EveryAccount {
		// Record A's D6: account_ids is empty AND the bool is set. Rendering
		// that as "none" would be the exact inverse of the truth.
		if named == "" {
			return "every account on this cluster"
		}
		return "every account on this cluster (also named: " + named + ")"
	}
	return named
}

func blockAbsence(onTheWire bool) string {
	if !onTheWire {
		return absentFromWire
	}
	return absentFromReply
}

// pendingNotes explains the absences that a PENDING ENGINE CHANGE causes, once,
// at the bottom -- and names only the blocks actually missing from the wire.
// A note saying group membership has not arrived, printed three rows under a
// list of groups, is the report contradicting itself; memql#5165 and
// memql#5181 are separate engine PRs editing one message, so a split landing
// is scheduled rather than hypothetical.
func pendingNotes(s Summary) []string {
	var notes []string
	if !s.Role.OnTheWire {
		notes = append(notes,
			"This cluster does not report roles as data yet. The engine change that adds",
			"the role slug, its name and its rank to MyAccess is memql#5181; until that",
			"lands there is nothing here to show. The cockpit does NOT fall back to the",
			"retired five-value enum: a custom role is none of those five, so a guess",
			"would be wrong in exactly the case this command exists for.")
	}
	if subject := pendingScopeSubject(s); subject != "" {
		if len(notes) > 0 {
			notes = append(notes, "")
		}
		notes = append(notes, subject+" with memql#5165.")
	}
	return notes
}

func pendingScopeSubject(s Summary) string {
	switch {
	case !s.Groups.OnTheWire && !s.Scope.OnTheWire:
		return "Group membership and account scope arrive"
	case !s.Groups.OnTheWire:
		return "Group membership arrives"
	case !s.Scope.OnTheWire:
		return "Account scope arrives"
	default:
		return ""
	}
}
