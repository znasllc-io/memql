package pipelinerun

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/identity/githubapp"
)

type releaseBuildTagHTTPRemote struct {
	*releaseBuildRemote
	remote GitHub
}

func (g *releaseBuildTagHTTPRemote) ObserveReleaseTag(ctx context.Context, token, repository, tag, selected string) (string, bool, error) {
	return g.remote.(releaseBuildGitHub).ObserveReleaseTag(ctx, token, repository, tag, selected)
}

func (g *releaseBuildTagHTTPRemote) CommitForRef(ctx context.Context, token, repository, ref string) (string, string, error) {
	return g.remote.CommitForRef(ctx, token, repository, ref)
}

// Exercise inspection through the production port and HTTP codec. The absent
// case reproduces GitHub's actual split: ref lookup is 404, commit lookup is
// 422. Replacing this with a fake CommitForRef returning 404 hid the defect.
func TestReleaseBuildTagObservationUsesExactGitReference(t *testing.T) {
	for _, name := range []string{
		"absent", "lightweight", "annotated", "annotated ABA", "annotated chain eight", "nested tag name", "conflicting commit",
		"tag chain excessive", "tag cycle", "tag identity", "tag unsupported", "tag read 404", "tag read 403", "tag read 422",
		"ref 401", "ref 403", "ref 422", "ref 429", "ref 500", "malformed reference",
		"wrong reference", "invalid object", "invalid object sha", "changed reference", "deleted reference",
		"resolution 422", "resolution 404", "resolution invalid sha", "lightweight mismatch",
		"hidden repository", "source 401", "source 403", "source 422", "source mismatch", "transport", "cancelled",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			r, g := configureReleaseBuild(t, h)
			if name == "nested tag name" {
				r.Tag = "cockpit/v0.20.2"
			}
			refPath := "/repos/" + repoName + "/git/ref/tags/" + r.Tag
			resolvePath := "/repos/" + repoName + "/commits/refs/tags/" + r.Tag
			sourcePath := "/repos/" + repoName + "/commits/" + shaA
			tagPrefix := "/repos/" + repoName + "/git/tags/"
			absent := name == "absent" || name == "hidden repository" || strings.HasPrefix(name, "source ")
			annotated := strings.HasPrefix(name, "annotated") || strings.HasPrefix(name, "tag ")
			refReads, resolutions, sourceReads, tagReads, nameReads := 0, 0, 0, 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodGet || req.Header.Get("Authorization") != "Bearer ghs_"+credID {
					t.Error("tag observation changed method or grant")
					w.WriteHeader(http.StatusForbidden)
					return
				}
				switch {
				case req.URL.Path == refPath:
					refReads++
					if absent || name == "deleted reference" && refReads == 2 {
						http.NotFound(w, req)
						return
					}
					if strings.HasPrefix(name, "ref ") {
						var status int
						_, _ = fmt.Sscanf(name, "ref %d", &status)
						w.WriteHeader(status)
						return
					}
					if name == "malformed reference" {
						_, _ = w.Write([]byte(`{"ref":`))
						return
					}
					ref, kind, sha := "refs/tags/"+r.Tag, "commit", shaA
					if annotated {
						kind, sha = "tag", shaB
					}
					switch name {
					case "conflicting commit":
						sha = shaC
					case "wrong reference":
						ref = "refs/heads/" + r.Tag
					case "invalid object":
						kind = "tree"
					case "invalid object sha":
						sha = "invalid"
					case "changed reference":
						if refReads == 2 {
							sha = shaC
						}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"ref": ref, "object": map[string]string{"type": kind, "sha": sha}})
				case req.URL.Path == resolvePath:
					nameReads++
					if absent {
						http.Error(w, `{"message":"No commit found for SHA: refs/tags/`+r.Tag+`"}`, 422)
						return
					}
					// The name temporarily points elsewhere, then returns to the
					// original tag object for the final reference read. A resolver
					// that asks by name would falsely bind the other commit.
					if name != "annotated ABA" {
						t.Error("present tag was resolved by mutable name")
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"sha": shaC})
				case strings.HasPrefix(req.URL.Path, tagPrefix):
					tagReads++
					requested := strings.TrimPrefix(req.URL.Path, tagPrefix)
					if strings.HasPrefix(name, "tag read ") {
						var status int
						_, _ = fmt.Sscanf(name, "tag read %d", &status)
						w.WriteHeader(status)
						return
					}
					identity, kind, sha := requested, "commit", shaA
					switch name {
					case "tag identity":
						identity = shaC
					case "tag cycle":
						kind, sha = "tag", requested
					case "tag unsupported":
						kind = "tree"
					case "annotated chain eight", "tag chain excessive":
						if tagReads < 8 || name == "tag chain excessive" {
							kind, sha = "tag", fmt.Sprintf("%040x", tagReads)
						}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"sha": identity, "object": map[string]string{"type": kind, "sha": sha}})
				case !absent && (req.URL.Path == sourcePath || req.URL.Path == "/repos/"+repoName+"/commits/"+shaC):
					resolutions++
					if name == "resolution 422" {
						w.WriteHeader(422)
						return
					} else if name == "resolution 404" {
						http.NotFound(w, req)
						return
					}
					sha := shaA
					if name == "conflicting commit" || name == "lightweight mismatch" {
						sha = shaC
					} else if name == "resolution invalid sha" {
						sha = "invalid"
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"sha": sha})
				case req.URL.Path == sourcePath:
					sourceReads++
					if name == "hidden repository" {
						http.NotFound(w, req)
						return
					}
					if strings.HasPrefix(name, "source ") && name != "source mismatch" {
						var status int
						_, _ = fmt.Sscanf(name, "source %d", &status)
						w.WriteHeader(status)
						return
					}
					sha := shaA
					if name == "source mismatch" {
						sha = shaC
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"sha": sha})
				default:
					t.Errorf("unexpected tag observation request %s", req.URL.Path)
					http.NotFound(w, req)
				}
			}))
			defer srv.Close()
			port := NewGitHub(&fakePackages{client: githubapp.New(githubapp.Config{}, githubapp.WithAPIBase(srv.URL), githubapp.WithHTTPClient(srv.Client()))})
			h.integ.Configure(func(d *Deps) { d.GitHub = &releaseBuildTagHTTPRemote{releaseBuildRemote: g, remote: port} })
			ctx := personCtx(ownerID)
			if name == "transport" {
				srv.Close()
			} else if name == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			source, err := h.integ.InspectReleaseBuild(ctx, r)
			good := name == "absent" || name == "lightweight" || strings.HasPrefix(name, "annotated") || name == "nested tag name" || name == "conflicting commit"
			if (err == nil) != good {
				t.Fatalf("inspection = %+v, %v", source, err)
			}
			if name == "absent" {
				if source.TagExists || source.TagSHA != "" || refReads != 1 || sourceReads != 1 || resolutions != 0 || nameReads != 0 {
					t.Fatalf("absence was not proven through the exact reference and readable source: %+v, reads=%d/%d/%d", source, refReads, sourceReads, resolutions)
				}
			} else if good {
				want := shaA
				if name == "conflicting commit" {
					want = shaC
				}
				if !source.TagExists || source.TagSHA != want || refReads != 2 || resolutions != 1 || sourceReads != 0 || nameReads != 0 {
					t.Fatalf("present/conflicting tag was not retained: %+v, reads=%d/%d/%d", source, refReads, sourceReads, resolutions)
				}
			}
			if name == "annotated chain eight" && tagReads != 8 || name == "tag chain excessive" && tagReads != 8 || name == "tag cycle" && tagReads != 1 {
				t.Fatalf("annotated object bound changed: %d requests", tagReads)
			}
			if len(h.github.created) != 0 || len(h.store.allRuns()) != 0 {
				t.Fatal("read-only tag inspection created an effect")
			}
		})
	}
}
