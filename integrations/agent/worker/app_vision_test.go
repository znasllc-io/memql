//go:build agent

package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// app_vision_test.go -- the decisions of a staged vision input, without a
// database (issue memql#5523).
//
// The LIFECYCLE -- the `vision_input` source, the promotion wait, the archive
// at session end -- is pinned against a real engine in
// app_vision_lifecycle_db_test.go. What is here is the part that has to be
// right before a row is written at all: the name a file lands under, the
// prompt that refers to it, and the refusal.
//
// Run under `-tags agent`, which CI's "go test -tags agent" step covers. An
// untagged `make test` does not see this file, which is worth knowing when
// reading a green run.

// THE NAME IS THE CONTRACT. The cockpit writes each input under the name
// Content-Disposition carries, which is the Library row's `name`, which is
// what this function returns -- and the prompt refers to that same string. A
// mismatch is not a broken build: it is an app told to read a file that is not
// there, answering about an image it never saw.
func TestAVisionInputLandsUnderANameAnAppWillReadAsAnImage(t *testing.T) {
	cases := []struct {
		mime string
		want string
	}{
		{"image/png", "vision-input-01.png"},
		{"image/jpeg", "vision-input-01.jpg"},
		{"image/gif", "vision-input-01.gif"},
		{"image/webp", "vision-input-01.webp"},
		// A blank type is a caller that did not say, not unknown bytes: the
		// argument is typed as an image at every level above this one.
		{"", "vision-input-01.png"},
		// A type that is not an image at all still lands as one rather than as
		// `.bin` -- a harness decides how to read a file partly from its
		// extension, and there is no useful third answer here.
		{"application/octet-stream", "vision-input-01.png"},
		// A parameterised type is the header form a browser sends.
		{"image/png; charset=binary", "vision-input-01.png"},
	}
	for _, c := range cases {
		if got := visionInputName(0, c.mime); got != c.want {
			t.Errorf("visionInputName(0, %q) = %q, want %q", c.mime, got, c.want)
		}
	}

	// NUMBERED FROM THE POSITION IN THE CALL, so a multi-image prompt can say
	// "the first image" and mean a file the app can open.
	if got := visionInputName(2, "image/png"); got != "vision-input-03.png" {
		t.Errorf("visionInputName(2, image/png) = %q, want vision-input-03.png", got)
	}

	// No name may escape the workspace. It cannot happen from the mime table,
	// and it is asserted because the value is written into a filename on
	// somebody else's machine.
	for _, m := range []string{"image/png", "image/jpeg", "", "../../etc/passwd", "image/../x"} {
		name := visionInputName(0, m)
		if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
			t.Errorf("visionInputName(0, %q) = %q, which is not a single safe path segment", m, name)
		}
	}
}

// THE PROMPT NAMES WHAT LANDED, and it names it from the stage's own result.
func TestTheVisionPromptNamesEveryLandedFile(t *testing.T) {
	one := []stagedVisionInput{{ArtifactId: "a1", Name: "vision-input-01.png"}}
	got := visionPromptWithInputs("What is in this screenshot?", one)
	for _, want := range []string{"What is in this screenshot?", "vision-input-01.png"} {
		if !strings.Contains(got, want) {
			t.Errorf("the prompt does not carry %q:\n%s", want, got)
		}
	}

	three := []stagedVisionInput{
		{ArtifactId: "a1", Name: "vision-input-01.png"},
		{ArtifactId: "a2", Name: "vision-input-02.jpg"},
		{ArtifactId: "a3", Name: "vision-input-03.png"},
	}
	got = visionPromptWithInputs("Compare these.", three)
	for _, want := range []string{"vision-input-01.png", "vision-input-02.jpg", "vision-input-03.png"} {
		if !strings.Contains(got, want) {
			t.Errorf("the multi-image prompt does not name %q:\n%s", want, got)
		}
	}
	// IN ORDER. "The second image" has to mean the second file, and the
	// numbering in the prompt is the only thing that makes that true.
	i1 := strings.Index(got, "vision-input-01.png")
	i2 := strings.Index(got, "vision-input-02.jpg")
	i3 := strings.Index(got, "vision-input-03.png")
	if !(i1 < i2 && i2 < i3) {
		t.Errorf("the files are not named in call order:\n%s", got)
	}

	// NO STAGE, NO SENTENCE. An ordinary chat turn's prompt must come through
	// untouched -- this function is on the path of every app call.
	if got := visionPromptWithInputs("Just a question.", nil); got != "Just a question." {
		t.Errorf("a prompt with no staged images was modified: %q", got)
	}
}

// The staged ids ride `inputs`, AFTER whatever the caller named, and the
// caller's slice is not written into.
func TestStagedInputsAreAppendedWithoutTouchingTheCallers(t *testing.T) {
	callers := make([]string, 1, 8) // spare capacity: an append would mutate in place
	callers[0] = "caller-artifact"
	staged := []stagedVisionInput{
		{ArtifactId: "vision-a", Name: "vision-input-01.png"},
		// One that never got an index row. It must not reach `inputs` as an
		// empty string -- the cockpit would try to pull artifact "".
		{ArtifactId: "", FileId: "vision-file-2", Name: "vision-input-02.png"},
	}
	got := appendStagedInputs(callers, staged)

	want := []string{"caller-artifact", "vision-a"}
	if len(got) != len(want) {
		t.Fatalf("appendStagedInputs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("inputs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if len(callers) != 1 || callers[0] != "caller-artifact" {
		t.Errorf("the caller's slice was mutated: %v -- req.Inputs may be shared with the "+
			"decision record, and a staged id written into it would appear on something else", callers)
	}
	if got := appendStagedInputs(nil, nil); got != nil {
		t.Errorf("appendStagedInputs(nil, nil) = %v, want nil", got)
	}
}

// AN UNCONFIGURED REPLICA REFUSES, AND SAYS WHICH THING IS MISSING.
//
// The refusal is the whole of this feature's safety: a node with no blob
// storage cannot land an image, and the alternative to refusing is sending a
// prompt that names a file which is not there.
func TestAReplicaThatCannotStageRefusesByName(t *testing.T) {
	cases := []struct {
		what    string
		stager  *visionStager
		mustSay string
	}{
		{"no stager wired at all", nil, "no vision stager"},
		{"a stager with no engine", &visionStager{uploader: stubVisionUploader{}, container: "c"}, "no engine"},
		{"a stager with no blob storage", &visionStager{engine: &memqlengine.MemQLEngine{}}, "no blob storage"},
		{"a stager with an uploader and no container",
			&visionStager{engine: &memqlengine.MemQLEngine{}, uploader: stubVisionUploader{}}, "no blob storage"},
	}
	for _, c := range cases {
		ok, why := c.stager.ready()
		if ok {
			t.Errorf("%s: ready() said yes", c.what)
			continue
		}
		if !strings.Contains(why, c.mustSay) {
			t.Errorf("%s: ready() reason %q does not say %q -- an operator has to be able to tell "+
				"a missing container from a missing engine", c.what, why, c.mustSay)
		}
	}

	// And the positive, so the negatives above are evidence about the
	// configuration rather than about ready() always saying no.
	full := &visionStager{engine: &memqlengine.MemQLEngine{}, uploader: stubVisionUploader{}, container: "library"}
	if ok, why := full.ready(); !ok {
		t.Errorf("a fully configured stager was not ready: %s", why)
	}
}

// The refusal's own text has to distinguish the two failures an operator acts
// on differently -- and a PARTIAL stage has to read as a refusal, not as a
// warning.
func TestTheStagingRefusalIsItsOwnAnswer(t *testing.T) {
	if memqlengine.AppVisionStagingRefusalCode == memqlengine.AppRefusalCode {
		t.Fatal("the staging refusal shares its code with 'no app available' -- a storage outage " +
			"would read to an operator as 'you have no machines', which is a different remedy")
	}
	err := &memqlengine.AppVisionStagingFailed{
		AppId: "claude-code", Images: 3, Staged: 2, Reason: "blob upload failed",
	}
	msg := err.Error()
	for _, want := range []string{memqlengine.AppVisionStagingRefusalCode, "2 of 3", "claude-code", "blob upload failed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not carry %q:\n%s", want, msg)
		}
	}
	if err.Code() != memqlengine.AppVisionStagingRefusalCode {
		t.Errorf("Code() = %q", err.Code())
	}
}

// THE FINGERPRINT SEPARATES TWO IMAGES UNDER ONE PROMPT.
//
// "Describe this screenshot" twice over two different screenshots is two
// calls. Without the image bytes in the key the second reads as a repetition
// of the first and is tripped by the identical-request breaker -- which
// presents as a vision call that works once and then silently stops.
func TestTheAppFingerprintSeparatesDifferentImages(t *testing.T) {
	prompt := []common.ChatMessage{{Role: "user", Content: "Describe this screenshot."}}
	a := memqlengine.AppCallRequest{AppId: "claude-code", Messages: prompt,
		Images: []common.VisionContent{{MimeType: "image/png", Data: []byte("first-image")}}}
	b := memqlengine.AppCallRequest{AppId: "claude-code", Messages: prompt,
		Images: []common.VisionContent{{MimeType: "image/png", Data: []byte("second-image")}}}

	if memqlengine.AppCallFingerprint(a) == memqlengine.AppCallFingerprint(b) {
		t.Error("two different images under one prompt fingerprint the same -- the second call " +
			"reads as a repetition and the breaker stops it")
	}
	// Same bytes, same call: the breaker must still catch a real loop.
	again := memqlengine.AppCallRequest{AppId: "claude-code", Messages: prompt,
		Images: []common.VisionContent{{MimeType: "image/png", Data: []byte("first-image")}}}
	if memqlengine.AppCallFingerprint(a) != memqlengine.AppCallFingerprint(again) {
		t.Error("the same call fingerprints differently, so a genuine loop would look novel " +
			"every time and the breaker would never fire")
	}
	// Two images of the SAME LENGTH must differ, which is why the bytes are
	// hashed rather than the count or the size.
	c := memqlengine.AppCallRequest{AppId: "claude-code", Messages: prompt,
		Images: []common.VisionContent{{MimeType: "image/png", Data: []byte("AAAAAAAAAAA")}}}
	d := memqlengine.AppCallRequest{AppId: "claude-code", Messages: prompt,
		Images: []common.VisionContent{{MimeType: "image/png", Data: []byte("BBBBBBBBBBB")}}}
	if memqlengine.AppCallFingerprint(c) == memqlengine.AppCallFingerprint(d) {
		t.Error("two same-length images collide -- a dashboard screenshot is exactly the case")
	}
	// A vision call and a plain chat turn with the same text are not the same
	// call either.
	plain := memqlengine.AppCallRequest{AppId: "claude-code", Messages: prompt}
	if memqlengine.AppCallFingerprint(plain) == memqlengine.AppCallFingerprint(a) {
		t.Error("a vision call fingerprints as its own prompt without the image")
	}
}

// The session slug is what makes a staged file id legible and unique, and it
// has to survive whatever the session id looks like.
func TestTheSessionSlugIsAlwaysASafeKey(t *testing.T) {
	cases := map[string]string{
		"v1:worker:appSession:abc123": "abc123",
		"abc123":                      "abc123",
		"":                            "unknown",
		"v1:worker:appSession:":       "unknown",
		"::::":                        "unknown",
		"a/b":                         "ab",
		"../../x":                     "x",
	}
	for in, want := range cases {
		got := visionSessionSlug(in)
		if got != want {
			t.Errorf("visionSessionSlug(%q) = %q, want %q", in, got, want)
		}
		if strings.ContainsAny(got, `/\.`) {
			t.Errorf("visionSessionSlug(%q) = %q, which is not safe in a path", in, got)
		}
	}
}

type stubVisionUploader struct{}

func (stubVisionUploader) Upload(_ context.Context, _, objectName string, _ []byte, _ string) (string, error) {
	return "blob://" + objectName, nil
}

// --- the stage itself, over a fake engine ---------------------------------
//
// The control flow, which a real engine cannot be made to exercise on demand:
// a promotion that arrives on the second poll, one that never arrives, and
// what each leaves behind. The rows these calls WRITE are pinned against a
// real store in component/memql/library_vision_input_5523_db_test.go; neither
// half is worth having without the other.

type fakeVisionEngine struct {
	queries []string
	// artifactAfter is how many promotion polls answer nothing before the
	// index row appears. -1 means it never does.
	artifactAfter int
	polls         int
	failOn        string
}

func (f *fakeVisionEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	f.queries = append(f.queries, q)
	if f.failOn != "" && strings.Contains(q, f.failOn) {
		return nil, errFakeVision
	}
	if strings.Contains(q, "libraryArtifactBySourceConceptRef") {
		f.polls++
		if f.artifactAfter < 0 || f.polls <= f.artifactAfter {
			return &memqlengine.ExecuteResult{}, nil
		}
		return visionArtifactResult("v1:library:artifact:artifact-probe"), nil
	}
	return &memqlengine.ExecuteResult{}, nil
}

var errFakeVision = errFakeVisionType{}

type errFakeVisionType struct{}

func (errFakeVisionType) Error() string { return "fake engine refused" }

type recordingVisionUploader struct {
	paths     []string
	container string
	err       error
}

func (u *recordingVisionUploader) Upload(_ context.Context, bucket, objectName string, _ []byte, _ string) (string, error) {
	if u.err != nil {
		return "", u.err
	}
	u.container = bucket
	u.paths = append(u.paths, objectName)
	return "blob://" + bucket + "/" + objectName, nil
}

func newTestStager(eng visionEngine, up visionBlobUploader) *visionStager {
	s := newVisionStager(eng, up, "library", nil, nil)
	// A tight budget: the timeout case is a real assertion, not a six-second
	// pause in the suite.
	s.wait = 60 * time.Millisecond
	s.poll = 10 * time.Millisecond
	return s
}

func TestStagingWritesTheRowAVisionInputNeeds(t *testing.T) {
	eng := &fakeVisionEngine{artifactAfter: 1} // absent once, then present
	up := &recordingVisionUploader{}
	s := newTestStager(eng, up)

	staged, err := s.Stage(context.Background(), "user-1", "v1:worker:appSession:sess9",
		[]common.VisionContent{{MimeType: "image/png", Data: []byte("bytes")}})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if len(staged) != 1 {
		t.Fatalf("staged %d inputs, want 1", len(staged))
	}

	// THE BYTES go to a Library path under the OWNER, so the row's blobUrl and
	// the download route agree about where they are.
	if len(up.paths) != 1 || !strings.HasPrefix(up.paths[0], "library/user-1/") {
		t.Errorf("uploaded to %v, want a library/user-1/... path", up.paths)
	}
	if !strings.HasSuffix(up.paths[0], "/"+staged[0].Name) {
		t.Errorf("the blob path %q does not end in the name the prompt will use (%q) -- the "+
			"download sets Content-Disposition from the ROW's name, so these have to agree",
			up.paths[0], staged[0].Name)
	}

	// THE ROW carries the values the archive sweep and the Library list read.
	var create string
	for _, q := range eng.queries {
		if strings.Contains(q, "createLibraryFile") {
			create = q
		}
	}
	if create == "" {
		t.Fatal("no createLibraryFile call was made")
	}
	for _, want := range []string{
		`source:"vision_input"`, // its own value, not app_session
		`format:"image"`,        // what picks the Library viewer
		`mimeType:"image/png"`,
		`name:"vision-input-01.png"`,
	} {
		if !strings.Contains(create, want) {
			t.Errorf("createLibraryFile does not carry %s:\n  %s", want, create)
		}
	}
	// The SESSION is named in the summary, so an operator finding one of these
	// can tell which turn staged it.
	if !strings.Contains(create, "sess9") {
		t.Errorf("createLibraryFile does not name the session:\n  %s", create)
	}

	// THE PROMOTION WAS WAITED FOR, not derived. The fake answered nothing on
	// the first poll, so a stager that took the first answer would have
	// returned an empty artifact id.
	if eng.polls < 2 {
		t.Errorf("the stager polled %d time(s) -- it took the first answer rather than waiting "+
			"for the index row, which is the derived-id shortcut this deliberately does not take",
			eng.polls)
	}
	if staged[0].ArtifactId == "" {
		t.Error("the staged input has no artifact id, so `inputs` would carry an empty string")
	}
	// BARE, not canonical: the wire contract is bare ids everywhere.
	if strings.Contains(staged[0].ArtifactId, ":") {
		t.Errorf("the artifact id %q is canonical; `inputs` carries bare ids", staged[0].ArtifactId)
	}
}

// A PROMOTION THAT NEVER ARRIVES IS A REFUSAL -- and the file it wrote comes
// back, so the caller can release it.
func TestAPromotionThatNeverArrivesRefusesAndReturnsWhatItWrote(t *testing.T) {
	eng := &fakeVisionEngine{artifactAfter: -1}
	s := newTestStager(eng, &recordingVisionUploader{})

	staged, err := s.Stage(context.Background(), "user-1", "sess",
		[]common.VisionContent{{MimeType: "image/png", Data: []byte("a")}})
	if err == nil {
		t.Fatal("a stage whose index row never appeared returned no error -- `inputs` would " +
			"carry an empty id and the app would be asked to read a file it was never given")
	}
	if !strings.Contains(err.Error(), "did not appear") {
		t.Errorf("the refusal does not say what was waited for: %v", err)
	}
	// The partial result is what makes the caller's release possible.
	if len(staged) != 0 {
		t.Errorf("Stage returned %d completed inputs on a refusal, want 0", len(staged))
	}
}

// A MULTI-IMAGE STAGE IS ALL OR NOTHING, and the half that landed is returned
// for release. Two of three is worse than none: the answer looks complete.
func TestAPartialStageIsARefusalThatHandsBackWhatLanded(t *testing.T) {
	// The first image promotes; the second's upload fails.
	eng := &fakeVisionEngine{artifactAfter: 0}
	up := &failSecondUploader{}
	s := newTestStager(eng, up)

	staged, err := s.Stage(context.Background(), "user-1", "sess", []common.VisionContent{
		{MimeType: "image/png", Data: []byte("first")},
		{MimeType: "image/png", Data: []byte("second")},
	})
	if err == nil {
		t.Fatal("a stage where one image failed returned no error")
	}
	if len(staged) != 1 {
		t.Fatalf("Stage returned %d landed inputs, want the 1 that succeeded so the caller can "+
			"release it", len(staged))
	}
	if staged[0].FileId == "" {
		t.Error("the landed input carries no file id, so a release cannot name it")
	}
}

type failSecondUploader struct{ n int }

func (u *failSecondUploader) Upload(_ context.Context, bucket, objectName string, _ []byte, _ string) (string, error) {
	u.n++
	if u.n >= 2 {
		return "", errFakeVision
	}
	return "blob://" + bucket + "/" + objectName, nil
}

// RELEASE WRITES BOTH, and does not depend on the archive automation firing.
func TestReleaseArchivesBothRows(t *testing.T) {
	eng := &fakeVisionEngine{artifactAfter: 0}
	s := newTestStager(eng, &recordingVisionUploader{})
	s.Release(context.Background(), "user-1", []stagedVisionInput{
		{ArtifactId: "artifact-1", FileId: "file-1", Name: "vision-input-01.png"},
	})
	var sawArtifact, sawFile bool
	for _, q := range eng.queries {
		if strings.Contains(q, "archiveArtifact") && strings.Contains(q, "artifact-1") {
			sawArtifact = true
		}
		if strings.Contains(q, "archiveLibraryFile") && strings.Contains(q, "file-1") {
			sawFile = true
		}
	}
	if !sawArtifact {
		t.Error("Release did not archive the index row")
	}
	if !sawFile {
		t.Error("Release did not archive the backing FILE. archiveArtifact reaches it through the " +
			"archiveFileOnArtifactArchive automation -- an event and a subscriber -- and this " +
			"runs deferred at the end of a turn with nothing left to check the result")
	}

	// A STAGE THAT NEVER GOT AN INDEX ROW still has its file archived.
	eng.queries = nil
	s.Release(context.Background(), "user-1", []stagedVisionInput{{FileId: "file-2"}})
	var sawOrphan bool
	for _, q := range eng.queries {
		if strings.Contains(q, "archiveLibraryFile") && strings.Contains(q, "file-2") {
			sawOrphan = true
		}
	}
	if !sawOrphan {
		t.Error("a staged file with no artifact id was not archived -- that is exactly the file a " +
			"timed-out promotion leaves behind")
	}
}

// A BLANK OWNER REFUSES. The Library is owner-tier: a row written under a
// blank actor is readable by nobody, including the operator asking where the
// image went.
func TestAStageWithNoOwnerRefusesRatherThanWritingAnUnownedRow(t *testing.T) {
	eng := &fakeVisionEngine{artifactAfter: 0}
	s := newTestStager(eng, &recordingVisionUploader{})
	if _, err := s.Stage(context.Background(), "  ", "sess",
		[]common.VisionContent{{MimeType: "image/png", Data: []byte("a")}}); err == nil {
		t.Fatal("a stage with no acting user wrote a row")
	}
	for _, q := range eng.queries {
		if strings.Contains(q, "createLibraryFile") {
			t.Errorf("a row was written under a blank actor: %s", q)
		}
	}
}

// A TYPED-NIL ENGINE must refuse by name rather than panic inside Execute.
func TestATypedNilEngineIsNotReady(t *testing.T) {
	var engine *memqlengine.MemQLEngine
	s := newVisionStager(engine, stubVisionUploader{}, "library", nil, nil)
	if ok, why := s.ready(); ok {
		t.Error("a stager built over a typed-nil engine reported ready -- the interface holding " +
			"it is non-nil, so every guard passes and the panic lands inside Execute")
	} else if !strings.Contains(why, "no engine") {
		t.Errorf("ready() reason %q does not name the missing engine", why)
	}
}

// visionArtifactResult builds the one-row result a promotion poll gets back.
//
// Through the BUNDLE, which is the shape a query actually produces, so
// MaterializeRows reads it the way it reads a real answer -- a friendlier
// hand-built shape would make this test agree with a projection the engine
// does not apply.
func visionArtifactResult(id string) *memqlengine.ExecuteResult {
	return &memqlengine.ExecuteResult{
		Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{Id: id}}},
	}
}
