package calls

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

// This opt-in test uses only a new, disposable media room and synthetic frames.
// The untrusted publisher is given a stream key, never the operator secret/JWT.
func TestLiveKitResourceIngressRevocation(t *testing.T) {
	python, publisher := os.Getenv("CALLKEY_RESOURCE_PYTHON"), os.Getenv("CALLKEY_RESOURCE_PUBLISHER")
	if python == "" || publisher == "" || os.Getenv(EnvMediaAPIURL) == "" {
		t.Skip("set local media and CALLKEY_RESOURCE_PYTHON/PUBLISHER to run the actual WHIP check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	api, key, secret := os.Getenv(EnvMediaAPIURL), os.Getenv(EnvMediaAPIKey), os.Getenv(EnvMediaAPISecret)
	media, err := NewLiveKitBackend(api, key, secret)
	require.NoError(t, err)
	ingress := &liveKitResources{client: lksdk.NewIngressClient(api, key, secret)}
	room := fmt.Sprintf("resource-revocation-test-%d", time.Now().UnixNano())
	require.NoError(t, media.CreateRoom(ctx, room, 3))
	defer media.DeleteRoom(context.Background(), room)
	lease := fmt.Sprintf("lease-%d", time.Now().UnixNano())
	grant, err := ingress.CreateResourceIngress(ctx, room, lease, "resource-test-source", resourceTestSpec())
	require.NoError(t, err)
	defer ingress.DeleteResourceIngress(context.Background(), grant.ID)
	var video, audio atomic.Int64
	callbacks := lksdk.NewRoomCallback()
	callbacks.OnTrackSubscribed = func(track *webrtc.TrackRemote, _ *lksdk.RemoteTrackPublication, _ *lksdk.RemoteParticipant) {
		go func() {
			for {
				_, _, e := track.ReadRTP()
				if e != nil {
					return
				}
				if track.Kind() == webrtc.RTPCodecTypeVideo {
					video.Add(1)
				} else {
					audio.Add(1)
				}
			}
		}()
	}
	pass, err := MintPass(key, secret, callrules.KindPerson, room, "test-observer", "Test observer", nil, 2*time.Minute)
	require.NoError(t, err)
	observer, err := lksdk.ConnectToRoomWithToken(os.Getenv(EnvMediaPublicURL), pass, callbacks, lksdk.WithConnectTimeout(10*time.Second))
	require.NoError(t, err)
	defer observer.Disconnect()
	publication := callrules.ResourcePublication{ResourceID: "synthetic", LeaseID: lease, RoomID: room, Identity: grant.Identity, Protocol: "whip", URL: grant.URL, StreamKey: grant.StreamKey, ExpiresAt: time.Now().Add(time.Minute)}
	data, err := json.Marshal(publication)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "publication.json")
	require.NoError(t, os.WriteFile(path, data, 0600))
	command := exec.CommandContext(ctx, python, publisher, "--publication", path, "--seconds", "60", "--tone")
	// Publisher process never inherits server control credentials from the test.
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "MEMQL_CALLS_MEDIA_") {
			command.Env = append(command.Env, v)
		}
	}
	log, err := os.Create(filepath.Join(t.TempDir(), "publisher.log"))
	require.NoError(t, err)
	defer log.Close()
	command.Stdout, command.Stderr = log, log
	require.NoError(t, command.Start())
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	require.Eventually(t, func() bool { return video.Load() > 5 && audio.Load() > 5 }, 35*time.Second, 200*time.Millisecond, "synthetic video and audio must actually traverse the gateway")
	require.NoError(t, ingress.DeleteResourceIngress(ctx, grant.ID))
	require.Eventually(t, func() bool {
		people, e := media.Participants(ctx, room)
		if e != nil {
			return false
		}
		for _, p := range people {
			if p.Identity == grant.Identity {
				return false
			}
		}
		return true
	}, 15*time.Second, 200*time.Millisecond, "DeleteIngress must stop the existing pipeline")
	// The same old stream key cannot create another pipeline after deletion.
	retry := exec.CommandContext(ctx, python, publisher, "--publication", path, "--seconds", "2", "--expect-rejected")
	retry.Env = command.Env
	output, err := retry.CombinedOutput()
	require.NoError(t, err, "revoked publisher result: %s", output)
	require.Contains(t, string(output), "REJECTED")
	found, err := ingress.FindResourceIngress(ctx, room, lease)
	require.NoError(t, err)
	require.Nil(t, found)
	t.Logf("received %d video / %d audio RTP packets; pipeline removed and old ingress credential rejected", video.Load(), audio.Load())
}
