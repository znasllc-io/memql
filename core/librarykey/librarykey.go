// Package librarykey answers one question about a stored Library blob key:
// is it inside its owner's own Library namespace?
//
// ===========================================================================
// WHY THE QUESTION HAS TO BE ASKED AT ALL
// ===========================================================================
// A v1:library:file row carries `blobUrl`, the storage path of its bytes, and
// every reader of a file streams that path with the NODE's storage credential.
// Row admission decides who may read the ROW; nothing about the row decides
// which OBJECT the credential then opens. So a row that names somebody else's
// object hands its reader somebody else's bytes, with every authorization gate
// green -- the reader really does own the row.
//
// Every Library writer stores bytes under one layout,
//
//	library/{ownerUserId}/{fileId}/{name}
//	library/{ownerUserId}/{fileId}/{key}/{name}   (a later version, a prepared image)
//
// so the answer is a prefix: the key's first segment is `library` and its
// second is the row owner's id. createLibraryFile is @serverOnly, so only a
// server writer composes the key; this check is what a READER runs anyway,
// because a row written before that was true, or by a writer that forwarded a
// value it did not compose, must not be streamed on the strength of the row
// being the caller's.
//
// ===========================================================================
// THE KEY IS READ THE WAY THE STORAGE CLIENT READS IT
// ===========================================================================
// A check that parsed the key differently from the client that fetches it
// would vouch for one object and open another. Split mirrors
// integrations/azureblob's parse for the two endpoint shapes MemQL runs:
//
//   - https://<account>.blob.core.windows.net/<container>/<object> -- the
//     account is the host, so the first path segment is the container. The
//     client reads a URL on a FOREIGN host the same way (ParseBlobObject),
//     against its own account: the host never decides which object opens.
//   - http://<host>:<port>/<account>/<container>/<object> -- Azurite, whose
//     account is the first path segment.
//
// The path is decoded exactly once, as the client decodes it, so an encoded
// `..` or `/` is judged as the segment it becomes. A bare key with no scheme
// (the chunked upload's row, or a node with no storage configured) is judged
// as written: the URL readers cannot open one, and the readers that open a key
// inside the Library's container judge exactly that key (CheckKey).
// integrations/azureblob's librarykey cross-check holds the two parsers
// together.
//
// An endpoint of any other shape (an https emulator with the account in the
// path) is read with the https rule, so the account segment is taken for the
// container: its own keys fail the check and its files are refused, the closed
// direction on purpose. MemQL runs neither such shape; a reader that knows its
// container should pass it, which closes the rest.
package librarykey

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/znasllc-io/memql/core/id"
)

// Prefix is the first segment of every Library object key.
const Prefix = "library"

// userConcept is the concept a canonical owner id names. The path segment and
// the row's ownerUserId may spell one person two ways -- the upload route
// writes the bare access id, the row stamps the canonical node id -- and only
// THIS concept's ids are treated as the same person.
const userConcept = "v1:identity:user"

// Split resolves a stored blob key to the (container, object) pair the storage
// client opens. A bare key has no container and is its own object. ok is false
// for anything the client cannot open: another scheme, or a URL naming no
// object below its container.
func Split(blobURL string) (container, object string, ok bool) {
	blobURL = strings.TrimSpace(blobURL)
	if blobURL == "" {
		return "", "", false
	}
	if !strings.Contains(blobURL, "://") {
		return "", blobURL, true
	}
	u, err := url.Parse(blobURL)
	if err != nil {
		return "", "", false
	}
	path := strings.TrimPrefix(u.Path, "/")
	switch u.Scheme {
	case "https":
	case "http":
		// Azurite: the account is the first segment, ahead of the container.
		idx := strings.Index(path, "/")
		if idx <= 0 {
			return "", "", false
		}
		path = path[idx+1:]
	default:
		return "", "", false
	}
	idx := strings.Index(path, "/")
	if idx <= 0 || idx == len(path)-1 {
		return "", "", false
	}
	return path[:idx], path[idx+1:], true
}

// Check reports whether blobURL names an object inside owner's own Library
// namespace, library/<owner>/..., and -- when container is not blank -- in that
// container. nil means it does; the error says why it does not, in words for a
// log line rather than for the caller, who is told only that the file is not
// there.
func Check(blobURL, owner, container string) error {
	gotContainer, object, ok := Split(blobURL)
	if !ok {
		return fmt.Errorf("the key is not a storage path the Library writes")
	}
	if container = strings.TrimSpace(container); container != "" && gotContainer != "" && gotContainer != container {
		return fmt.Errorf("the key names container %q, not the Library's %q", gotContainer, container)
	}
	return CheckKey(object, owner)
}

// CheckKey is Check for a reader that opens a key AS WRITTEN inside the
// Library's container (the campaign import, a package or site deploy from a
// Library zip): key is judged exactly as the object name that will be opened,
// with no URL parsing, so the check and the read cannot disagree about which
// object it is.
func CheckKey(key, owner string) error {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return fmt.Errorf("the file has no owner to hold its key to")
	}
	// A backslash is split on as well as a slash: storage may read one as the
	// other, and a key is judged by the most that could be made of it. The
	// segments after the owner are a file's own (a name may carry a backslash
	// or a control character when its writer did not clean it); only a
	// relative segment can climb out of the prefix, so only that is refused.
	segments := strings.Split(strings.ReplaceAll(key, "\\", "/"), "/")
	for _, s := range segments {
		if s == "." || s == ".." {
			return fmt.Errorf("the key has a relative path segment")
		}
	}
	if len(segments) < 3 || segments[0] != Prefix || strings.Join(segments[2:], "") == "" {
		return fmt.Errorf("the key is not a file under %s/{owner}/", Prefix)
	}
	if !sameUser(segments[1], owner) {
		return fmt.Errorf("the key is under another owner's %s/ prefix", Prefix)
	}
	return nil
}

// sameUser reports whether a key's owner segment and a row's owner id name the
// same person: equal as written, or the same user short id once a canonical
// user id is reduced to it. An id of any other concept never matches, so a
// segment like v1:agents:agent:<id> does not pass for user <id>.
func sameUser(segment, owner string) bool {
	if segment == owner {
		return true
	}
	s, sok := userShortId(segment)
	o, ook := userShortId(owner)
	return sok && ook && s == o
}

// userShortId reduces a user id to its short id: a bare id is its own, a
// canonical v1:identity:user id yields its last segment, and any other concept
// yields nothing.
func userShortId(value string) (string, bool) {
	concept, short, err := id.ParseNodeId(value)
	if err != nil || short == "" {
		return "", false
	}
	if concept != "" && concept != userConcept {
		return "", false
	}
	return short, true
}
