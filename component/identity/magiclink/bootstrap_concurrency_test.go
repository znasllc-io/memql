package magiclink

import "testing"

func TestBootstrapFinishNeverConsumesWithoutCoordination(t *testing.T) {
	v, engine, _ := newFloorVerifier("owner")
	engine.oauthCtx = `{"bootstrap":true,"adminSession":true}`
	if _, err := finishOnce(v); err == nil {
		t.Fatal("bootstrap finished without a database gate")
	}
	if engine.consumeCalls != 0 {
		t.Fatal("link was spent before ownership coordination was available")
	}
}
