package librarykey

import (
	"strings"
	"testing"
)

const (
	cloudBase   = "https://acct.blob.core.windows.net/memql-library/"
	azuriteBase = "http://azurite:10000/devstoreaccount1/memql-library/"
)

// Every shape a Library writer actually stores, against the owner it was
// stored for. A check that refuses one of these 404s somebody's own file.
func TestCheckAdmitsEveryShapeTheWritersStore(t *testing.T) {
	cases := []struct {
		name, blobURL, owner string
	}{
		{"cloud URL, bare owner", cloudBase + "library/u-1/f-1/notes.md", "u-1"},
		// The row's ownerUserId is stamped canonical while the upload route
		// writes the path from the bare access id: one person, two spellings.
		{"cloud URL, canonical row owner", cloudBase + "library/u-1/f-1/notes.md", "v1:identity:user:u-1"},
		{"canonical id in the path", cloudBase + "library/v1:identity:user:u-1/f-1/notes.md", "u-1"},
		{"a later version", cloudBase + "library/u-1/f-1/k-9/notes.md", "u-1"},
		{"a prepared image", cloudBase + "library/u-1/f-1/" + strings.Repeat("ab", 32) + "/cat.png", "u-1"},
		{"an escaped name", cloudBase + "library/u-1/f-1/space%20name%252F.md", "u-1"},
		{"Azurite, account in the path", azuriteBase + "library/u-1/f-1/notes.md", "u-1"},
		{"a bare key (chunked upload, or no storage configured)", "library/u-1/f-1/notes.md", "u-1"},
		// Not every writer cleans a name (skills capture keeps a script's own),
		// and nothing after the owner can leave the owner's prefix unless it is
		// a relative segment.
		{"a Windows path kept as the name", "library/u-1/f-1/C:\\scripts\\deploy.sh", "u-1"},
		{"a control character in the name", "library/u-1/f-1/line\nbreak.md", "u-1"},
	}
	for _, tc := range cases {
		if err := Check(tc.blobURL, tc.owner, "memql-library"); err != nil {
			t.Errorf("%s: %q for owner %q refused: %v", tc.name, tc.blobURL, tc.owner, err)
		}
	}
}

// The forgeries: every one names bytes that are not the owner's to read.
func TestCheckRefusesAKeyOutsideTheOwnersLibrary(t *testing.T) {
	cases := []struct {
		name, blobURL, owner string
	}{
		{"another user's library", cloudBase + "library/u-2/f-1/payroll.csv", "u-1"},
		{"another user's library, canonical owner", cloudBase + "library/u-2/f-1/payroll.csv", "v1:identity:user:u-1"},
		{"another user's library, bare key", "library/u-2/f-1/payroll.csv", "u-1"},
		{"another user's library, Azurite", azuriteBase + "library/u-2/f-1/payroll.csv", "u-1"},
		{"an id of another concept with the same short id", cloudBase + "library/v1:agents:agent:u-1/f-1/x", "u-1"},
		{"not under library/", cloudBase + "sites/u-1/index.html", "u-1"},
		{"library/ further down", cloudBase + "packages/p-1/library/u-1/f/x", "u-1"},
		{"another container", "https://acct.blob.core.windows.net/logs-archive/library/u-1/f-1/x", "u-1"},
		{"nothing after the owner", cloudBase + "library/u-1", "u-1"},
		{"nothing after the owner but a slash", cloudBase + "library/u-1/", "u-1"},
		{"dot-dot traversal", cloudBase + "library/u-1/../u-2/f-1/payroll.csv", "u-1"},
		{"encoded dot-dot traversal", cloudBase + "library/u-1/%2E%2E/u-2/f-1/payroll.csv", "u-1"},
		{"encoded slash traversal", cloudBase + "library/u-1/..%2Fu-2/f-1/payroll.csv", "u-1"},
		{"bare-key traversal", "library/u-1/../u-2/f-1/payroll.csv", "u-1"},
		{"dot segment", cloudBase + "library/u-1/./f-1/x", "u-1"},
		{"an empty owner segment", cloudBase + "library//u-1/f-1/x", "u-1"},
		{"backslash traversal", "library/u-1/f-1\\..\\..\\u-2\\x", "u-1"},
		{"encoded backslash traversal", cloudBase + "library/u-1/f-1%5C..%5C..%5Cu-2%5Cx", "u-1"},
		{"a backslash owner", "library\\u-2\\f-1\\x", "u-1"},
		{"no scheme the storage client reads", "local://library/u-1/f-1/x", "u-1"},
		{"a foreign scheme", "gs://bucket/library/u-1/f-1/x", "u-1"},
		{"no owner", cloudBase + "library/u-1/f-1/x", ""},
		{"no key", "", "u-1"},
	}
	for _, tc := range cases {
		if err := Check(tc.blobURL, tc.owner, "memql-library"); err == nil {
			t.Errorf("%s: %q was admitted for owner %q", tc.name, tc.blobURL, tc.owner)
		}
	}
}

// A blank container means the caller does not know its own, so the container
// is not compared -- the owner prefix still is.
func TestCheckWithoutAContainerStillChecksTheOwner(t *testing.T) {
	if err := Check("https://acct.blob.core.windows.net/any/library/u-1/f/x", "u-1", ""); err != nil {
		t.Errorf("an owner's key in an unknown container was refused: %v", err)
	}
	if err := Check("https://acct.blob.core.windows.net/any/library/u-2/f/x", "u-1", ""); err == nil {
		t.Error("another owner's key was admitted because no container was named")
	}
}

// Split is what the check reads, so it has to be what the storage client
// reads: integrations/azureblob holds the cross-check against the client's
// own parser. These pin the two shapes in isolation.
func TestSplitReadsTheContainerTheStorageClientReads(t *testing.T) {
	cases := []struct {
		blobURL, container, object string
		ok                         bool
	}{
		{cloudBase + "library/u-1/f/x", "memql-library", "library/u-1/f/x", true},
		{azuriteBase + "library/u-1/f/x", "memql-library", "library/u-1/f/x", true},
		{"https://acct.blob.core.windows.net/c/a%2Fb", "c", "a/b", true},
		{"library/u-1/f/x", "", "library/u-1/f/x", true},
		{"https://acct.blob.core.windows.net/only-a-container", "", "", false},
		{"http://azurite:10000/devstoreaccount1/c", "", "", false},
		{"ftp://host/c/x", "", "", false},
		{"  ", "", "", false},
	}
	for _, tc := range cases {
		container, object, ok := Split(tc.blobURL)
		if container != tc.container || object != tc.object || ok != tc.ok {
			t.Errorf("Split(%q) = (%q, %q, %t), want (%q, %q, %t)",
				tc.blobURL, container, object, ok, tc.container, tc.object, tc.ok)
		}
	}
}

// CheckKey judges the object name a key-form reader opens, as written: a URL
// handed to it is an object literally named like a URL, not the object the URL
// would name, so it is no Library key at all.
func TestCheckKeyJudgesTheKeyAsWritten(t *testing.T) {
	if err := CheckKey("library/u-1/f-1/list.csv", "v1:identity:user:u-1"); err != nil {
		t.Errorf("the owner's own bare key was refused: %v", err)
	}
	for _, key := range []string{
		"library/u-2/f-1/list.csv",
		cloudBase + "library/u-1/f-1/list.csv",
		" library/u-1/f-1/list.csv",
		"library/u-1/../u-2/list.csv",
		"sites/s-1/1/index.html",
	} {
		if err := CheckKey(key, "u-1"); err == nil {
			t.Errorf("CheckKey(%q) admitted it for u-1", key)
		}
	}
	if err := CheckKey("library/u-1/f-1/list.csv", " "); err == nil {
		t.Error("a key was admitted for a blank owner")
	}
}
