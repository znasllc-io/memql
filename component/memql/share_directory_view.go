package memql

import (
	"context"
	"strings"
)

// share_directory_view.go -- the share directory, for something other than a
// machine.
//
// fleetShareDirectory answers who a person may lend one of their OWN MACHINES
// to (fleet_share_directory.go): the groups they are in and the people in
// those groups, or everyone for a caller who may already read every person on
// the cluster. A person's API key is lent by the same rule to the same people
// (design record 2026-11-21-vendor-accounts-and-api-keys, K6), and a person
// with a key and no machine has no registration to ask that builtin about.
//
// So the directory itself is exported here, unchanged, as a view: the same
// rows, the same offer, the same names. What is lent, and whether the caller
// may lend it, is the caller's to establish before asking.

// ShareSubject is one person or group in a share directory.
type ShareSubject struct {
	// Id is bare.
	Id   string
	Name string
	// Detail is a person's email, and only for a caller who already reads one.
	Detail string
	// Members is a group's active people.
	Members int
}

// ShareDirectoryView is who one person may lend something of their own to,
// and the name of every person and group by id, so a subject already on a
// list can still be named after it stops being offered.
type ShareDirectoryView struct {
	// Everyone reports that the caller may already read every person on the
	// cluster, and so is offered all of them.
	Everyone bool
	People   []ShareSubject
	Groups   []ShareSubject

	dir shareDirectory
}

// OffersPerson reports whether the caller may add this person to a list.
func (v ShareDirectoryView) OffersPerson(id string) bool { return v.dir.offersPerson(id) }

// OffersGroup reports whether the caller may add this group to a list.
func (v ShareDirectoryView) OffersGroup(id string) bool { return v.dir.offersGroup(id) }

// Person names a person by id, whether or not they are still offered. known
// is false for an id no user row carries.
func (v ShareDirectoryView) Person(id string) (subject ShareSubject, known bool) {
	entry, known := v.dir.knownPeople[BareShortId(strings.TrimSpace(id))]
	return ShareSubject{Id: entry.Id, Name: entry.Name, Detail: entry.Detail}, known
}

// Group names a group by id, whether or not it is still offered.
func (v ShareDirectoryView) Group(id string) (subject ShareSubject, known bool) {
	entry, known := v.dir.knownGroups[BareShortId(strings.TrimSpace(id))]
	return ShareSubject{Id: entry.Id, Name: entry.Name, Members: entry.Members}, known
}

// ShareDirectoryFor builds the calling person's share directory. A context
// with no acting person is offered nobody.
func (e *MemQLEngine) ShareDirectoryFor(ctx context.Context) (ShareDirectoryView, error) {
	if e == nil {
		return ShareDirectoryView{}, nil
	}
	// Fleet's rule, unchanged (any active group the caller shares, custom
	// ones included), so not narrowed to the caller's organization the way a
	// Library file share is (library_share_directory.go).
	dir, err := e.shareDirectoryFor(ctx, false)
	if err != nil {
		return ShareDirectoryView{}, err
	}
	view := ShareDirectoryView{Everyone: dir.everyone, dir: dir}
	for _, p := range dir.people {
		view.People = append(view.People, ShareSubject{Id: p.Id, Name: p.Name, Detail: p.Detail})
	}
	for _, g := range dir.groups {
		view.Groups = append(view.Groups, ShareSubject{Id: g.Id, Name: g.Name, Members: g.Members})
	}
	return view, nil
}

// ShareSubjectsMax is how many people and groups one share may name.
const ShareSubjectsMax = shareSubjectsMax
