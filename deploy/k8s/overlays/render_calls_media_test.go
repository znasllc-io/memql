package overlays

import (
	"errors"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// render_calls_media_test.go -- the calls media wiring in both cloud overlays.
//
// An installation builds on overlays/cloud or overlays/cloud-entry, and most installations
// have no media server. So the one thing this gate exists for is that composing
// ../components/calls-media changes nothing for them: the bff's read of the
// memql-calls-media Secret is OPTIONAL, which is what lets a cluster with no such Secret
// start exactly as it did before (calls then simply has no media server, and the bff
// answers no_media_server). Without `optional` every bff pod would sit in
// CreateContainerConfigError waiting for a Secret nobody created -- an outage, from a
// manifest that reads correctly. And an overlay that left the component out would be an
// installation whose calls could never reach a media server, whatever it seeded.
//
// The edge's media host suffix is a per-installation VALUE derived from the domain
// (media.$(MEMQL_DOMAIN)), the same way components/ask-voice derives the voice
// signalling host, so no file names a domain and no branch picks one. An installation
// whose media servers live somewhere else overrides that one value.
//
// The local overlay's twin is TestCallsMediaReachesTheBFFAndTheEdge in
// local/render_frontdoor_test.go.

// callsMediaOverlays are the instance overlays an installation builds on. Listed, for
// the reason instanceOverlays is: the failure this catches is an overlay without the
// wiring, and a discovered list would shrink with it.
var callsMediaOverlays = []string{cloudOverlay, entryOverlay}

// callsMediaPod is the slice of a rendered Deployment these assertions read. The shared
// podResource type does not decode `optional`, which is the assertion.
type callsMediaPod struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Name    string                         `yaml:"name"`
					Env     []struct{ Name, Value string } `yaml:"env"`
					EnvFrom []struct {
						SecretRef struct {
							Name     string `yaml:"name"`
							Optional *bool  `yaml:"optional"`
						} `yaml:"secretRef"`
						ConfigMapRef struct {
							Name string `yaml:"name"`
						} `yaml:"configMapRef"`
					} `yaml:"envFrom"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

func TestCloudOverlaysReadTheCallsMediaSecretOptionally(t *testing.T) {
	for _, overlay := range callsMediaOverlays {
		t.Run(overlay, func(t *testing.T) {
			pods := map[string]callsMediaPod{}
			dec := yaml.NewDecoder(strings.NewReader(render(t, overlay)))
			for i := 0; ; i++ {
				var p callsMediaPod
				err := dec.Decode(&p)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("decoding document %d of the rendered overlay: %v", i+1, err)
				}
				if p.Kind == "Deployment" {
					pods[p.Metadata.Name] = p
				}
			}

			var reads int
			for name, p := range pods {
				for _, c := range p.Spec.Template.Spec.Containers {
					for _, source := range c.EnvFrom {
						if source.SecretRef.Name != "memql-calls-media" {
							continue
						}
						reads++
						if name != "bff" {
							t.Errorf("%s/%s reads memql-calls-media; only the bff holds the media server's credentials", name, c.Name)
						}
						if source.SecretRef.Optional == nil || !*source.SecretRef.Optional {
							t.Errorf("%s/%s reads memql-calls-media without optional: true, so an installation with no "+
								"media server never starts it", name, c.Name)
						}
					}
				}
			}
			if reads == 0 {
				t.Errorf("no Deployment in %s reads memql-calls-media: an installation that seeds it gets no "+
					"media server", overlay)
			}

			edge, ok := pods["edge"]
			if !ok {
				t.Fatalf("%s renders no edge Deployment", overlay)
			}
			var suffix string
			var mountsDomain bool
			for _, c := range edge.Spec.Template.Spec.Containers {
				for _, env := range c.Env {
					if env.Name == "MEMQL_CALLS_MEDIA_HOST_SUFFIX" {
						suffix = env.Value
					}
				}
				for _, source := range c.EnvFrom {
					if source.ConfigMapRef.Name == "memql-domain" {
						mountsDomain = true
					}
				}
			}
			if suffix != "media.$(MEMQL_DOMAIN)" {
				t.Errorf("edge MEMQL_CALLS_MEDIA_HOST_SUFFIX = %q, want media.$(MEMQL_DOMAIN), derived from the "+
					"installation's domain rather than written down", suffix)
			}
			if !mountsDomain {
				t.Error("the edge does not mount memql-domain, so $(MEMQL_DOMAIN) in its media host suffix never expands")
			}
		})
	}
}
