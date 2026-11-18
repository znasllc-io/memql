package overlays

import (
	"errors"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// render_calls_media_test.go -- the calls media wiring in the cloud overlay.
//
// The cloud overlay is the base real installations build on, and most of them have no
// media server. So the one thing this gate exists for is that composing
// ../components/calls-media changes nothing for them: the bff's read of the
// memql-calls-media Secret is OPTIONAL, which is what lets a cluster with no such Secret
// start exactly as it did before (calls then simply has no media server, and the bff
// answers no_media_server). Without `optional` every bff pod would sit in
// CreateContainerConfigError waiting for a Secret nobody created -- an outage, from a
// manifest that reads correctly.
//
// The edge's media host suffix is a per-installation VALUE derived from the domain
// (media.$(MEMQL_DOMAIN)), the same way components/ask-voice derives the voice
// signalling host, so no file names a domain and no branch picks one. An installation
// whose media servers live somewhere else overrides that one value.
//
// The local overlay's twin, which also checks that no other node holds the credentials,
// is TestCallsMediaReachesTheBFFAndTheEdge in local/render_frontdoor_test.go.

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

func TestCloudOverlayReadsTheCallsMediaSecretOptionally(t *testing.T) {
	pods := map[string]callsMediaPod{}
	dec := yaml.NewDecoder(strings.NewReader(render(t, cloudOverlay)))
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
		t.Error("no Deployment in the cloud overlay reads memql-calls-media: an installation that seeds it gets " +
			"no media server")
	}

	edge, ok := pods["edge"]
	if !ok {
		t.Fatal("the cloud overlay renders no edge Deployment")
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
}
