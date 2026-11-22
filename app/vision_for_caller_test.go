package app

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// vision_for_caller_test.go -- a picture is described by the model resolved
// FOR WHOEVER IS HAVING IT DESCRIBED (owner decision 2026-11-22: the cluster's
// vendor account pays for a person's calls only when that person may use it,
// and a picture follows the rule).
//
// The Library's and the attachment route's extractor used to be handed one
// vision provider resolved with no caller at all. A call nobody is behind is
// the cluster's own work, so every uploader's pictures were described on the
// cluster's account, whoever uploaded them.

type recordingVision struct {
	calls []string
}

func (v *recordingVision) CallVision(ctx context.Context, _ string, _ []common.VisionContent) (string, error) {
	who := ""
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		who = ac.UserId
	}
	v.calls = append(v.calls, who)
	return "a picture of a harbour", nil
}

func asPerson(id string) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: id, Role: auth.RoleWriter})
}

func TestAPictureIsDescribedByTheModelResolvedForItsCaller(t *testing.T) {
	served := &recordingVision{}
	var resolvedFor []string
	vision := callerVision{resolve: func(ctx context.Context) common.VisionAIProvider {
		who := ""
		if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
			who = ac.UserId
		}
		resolvedFor = append(resolvedFor, who)
		// The router's answer for a person no vision model can serve: the
		// cluster's account is closed to them and they have no local model.
		if who == "ben" {
			return nil
		}
		return served
	}}
	image := []common.VisionContent{{MimeType: "image/png", Data: []byte{1}}}

	got, err := vision.CallVision(asPerson("ana"), "describe", image)
	if err != nil || got != "a picture of a harbour" {
		t.Fatalf("ana's picture: %q, %v", got, err)
	}
	if _, err := vision.CallVision(asPerson("ben"), "describe", image); err == nil ||
		!strings.Contains(err.Error(), "no vision model") {
		t.Fatalf("ben's picture: err = %v, want it to say no vision model can describe it for him", err)
	}
	// Asked again for every call, with that call's own caller: nothing
	// resolved for one person is kept for the next.
	if _, err := vision.CallVision(asPerson("ana"), "describe", image); err != nil {
		t.Fatal(err)
	}
	if want := "ana,ben,ana"; strings.Join(resolvedFor, ",") != want {
		t.Fatalf("resolved for %v, want %s", resolvedFor, want)
	}
	if want := "ana,ana"; strings.Join(served.calls, ",") != want {
		t.Fatalf("the model was called for %v, want %s: ben's picture must not have reached it", served.calls, want)
	}
}

// visionResolver stands in for the router behind the engine's seam. It records
// who each resolution was FOR, as the engine stamped it from the call's
// context, and answers with a model that records whose picture it described.
type visionResolver struct {
	resolvedFor []string
	vision      *recordingVision
}

func (r *visionResolver) ResolveFor(_ context.Context, req airoute.ResolveRequest) (memql.ResolvedProvider, error) {
	r.resolvedFor = append(r.resolvedFor, req.UserId)
	return memql.ResolvedProvider{Client: r.vision}, nil
}

func appResolvingVisionThrough(r *visionResolver) *App {
	engine := &memql.MemQLEngine{}
	engine.SetAIResolver(r)
	return &App{engine: engine, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

var onePicture = []common.VisionContent{{MimeType: "image/png", Data: []byte{1}}}

// THE TWO PLACES THE APP HANDS OUT A VISION PROVIDER, exercised as wired. Each
// used to resolve one when it was wired, with no caller, and keep it: these
// fail against that wiring, because the router is then asked once, for
// nobody, before any picture exists. The Library's own is in
// transport_artifacts_vision_test.go, which builds with the bff as its wiring
// does.

// A plug-in's getter (PluginContext.ResolveVisionProvider): the files
// integration and the Library's analysis pass both read images through it.
func TestAPlugInsVisionProviderIsResolvedForWhoeverHasAPictureDescribed(t *testing.T) {
	router := &visionResolver{vision: &recordingVision{}}
	a := appResolvingVisionThrough(router)

	vision := a.pluginContext().ResolveVisionProvider()
	if len(router.resolvedFor) != 0 {
		t.Fatalf("handing the provider to a plug-in resolved a model for %q: nobody had a picture yet", router.resolvedFor)
	}
	for _, who := range []string{"ana", "ben"} {
		if _, err := vision.CallVision(asPerson(who), "describe", onePicture); err != nil {
			t.Fatalf("%s's picture: %v", who, err)
		}
	}
	if got := strings.Join(router.resolvedFor, ","); got != "ana,ben" {
		t.Fatalf("the router was asked for %q, want ana then ben: each picture is its uploader's call", got)
	}
	if got := strings.Join(router.vision.calls, ","); got != "ana,ben" {
		t.Fatalf("the model described pictures for %q, want ana then ben", got)
	}
}

// With no engine nothing resolves, and the call says so instead of panicking:
// text formats are unaffected.
func TestAPictureWithNoEngineToResolveThroughIsAnErrorOnThatPicture(t *testing.T) {
	a := &App{}
	if _, err := a.visionForCaller().CallVision(context.Background(), "describe", nil); err == nil {
		t.Fatal("a call with nothing to resolve returned no error")
	}
}
