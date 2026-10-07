package release

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

func TestCandidateCommandsGateEveryNonOwnerBeforeConfiguration(t *testing.T) {
	i := NewIntegration(nil, &tripwireEngine{t: t}, resolver{systemVariable: func(context.Context, string) (string, error) { t.Fatal("nonowner read configuration"); return "", nil }})
	if err := i.ConfigureCandidates(CandidateDependencies{Database: func() *sql.DB { t.Fatal("nonowner read journal"); return nil }, Library: &candidateLibraryFixture{t: t}}); err != nil {
		t.Fatal(err)
	}
	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleDeveloper, auth.RoleWriter, auth.RoleReader, auth.Role("")} {
		for _, capability := range i.Capabilities() {
			switch capability.Name {
			case "candidateConfiguration", "listCandidates", "prepareCandidate", "approveCandidate", "publishCandidate", "getCandidate", "retireCandidate":
			default:
				continue
			}
			if _, err := capability.Handler(actorContext(role), map[string]any{"candidateId": "candidate", "approved": true, "verified": true}, 0); RefusalCode(err) != CodeNotOwner {
				t.Fatalf("%s admitted %s: %v", capability.Name, role, err)
			}
		}
	}
}

func TestCandidateConfigurationRequiresExplicitUniqueBoundedOperatorValues(t *testing.T) {
	base := candidateOperatorConfiguration{FormatVersion: 1,
		Sources:    []candidateVersionSource{{Component: "engine", Repository: "acme/engine", Path: "VERSION"}},
		Registries: []candidateRegistryTarget{{ID: "registry", Component: "engine", Artifact: "image", Origin: "https://registry.example.com", Repository: "acme/engine"}},
	}
	for _, failure := range []string{"valid", "missing", "read error", "version", "duplicate source", "duplicate target", "unknown component", "unknown field", "plaintext credential", "trailing", "too large"} {
		t.Run(failure, func(t *testing.T) {
			cfg := base
			switch failure {
			case "version":
				cfg.FormatVersion = 2
			case "duplicate source":
				cfg.Sources = append(cfg.Sources, cfg.Sources[0])
			case "duplicate target":
				cfg.Registries = append(cfg.Registries, cfg.Registries[0])
			case "unknown component":
				cfg.Sources = []candidateVersionSource{{Component: "another", Repository: "acme/engine", Path: "VERSION"}}
			}
			body, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			raw := string(body)
			switch failure {
			case "missing":
				raw = ""
			case "unknown field":
				raw = strings.TrimSuffix(raw, "}") + `,"ambientCredentials":true}`
			case "plaintext credential":
				raw = strings.Replace(raw, `"origin":`, `"password":"secret-must-not-appear","origin":`, 1)
			case "trailing":
				raw += `{}`
			case "too large":
				raw = strings.Repeat(" ", 1<<20) + raw
			}
			i := NewIntegration(nil, &tripwireEngine{t: t}, resolver{
				systemVariable: func(ctx context.Context, name string) (string, error) {
					if name != CandidateConfigurationVariable || !memql.FreshReadFromContext(ctx) {
						t.Fatal("configuration was not an exact fresh read")
					}
					if failure == "read error" {
						return "", errors.New("secret-must-not-appear")
					}
					return raw, nil
				},
				systemSecret: func(context.Context, string) (string, error) {
					t.Fatal("configuration read fetched credentials")
					return "", nil
				},
				env: func(string) string { t.Fatal("candidate configuration used ambient env fallback"); return "" },
			})
			if err := i.ConfigureCandidates(CandidateDependencies{Database: func() *sql.DB { t.Fatal("configuration read touched database"); return nil }, Library: &candidateLibraryFixture{t: t}}); err != nil {
				t.Fatal(err)
			}
			p, err := i.configuredCandidate(ownerCtx())
			if failure == "valid" {
				if err != nil || p == nil || p.evidence == nil || p.targetReader == nil {
					t.Fatal("configured native ports unavailable", err)
				}
			} else if err == nil || strings.Contains(err.Error(), "secret-must-not-appear") {
				t.Fatal("invalid configuration accepted or credential disclosed", err)
			}
		})
	}
}

func TestCandidateDependenciesCannotBeReboundOrSilentlyMissing(t *testing.T) {
	i := NewIntegration(nil, nil, resolver{})
	if _, err := i.candidateStorage(ownerCtx()); err == nil {
		t.Fatal("missing artifact store was accepted")
	}
	if err := i.ConfigureCandidates(CandidateDependencies{}); err == nil {
		t.Fatal("empty dependencies accepted")
	}
	deps := CandidateDependencies{Database: func() *sql.DB { return nil }, Library: &candidateLibraryFixture{t: t}}
	if err := i.ConfigureCandidates(deps); err != nil {
		t.Fatal(err)
	}
	if err := i.ConfigureCandidates(deps); err == nil {
		t.Fatal("native authority rebound after startup")
	}
}

func TestCandidateJSONRejectsUnknownAndAmbiguousEnvelope(t *testing.T) {
	for _, raw := range []string{"", `{"extra":true}`, `{"formatVersion":1} {}`, `[]`, `"candidate"`, `null`, `{"formatVersion":1,"FormatVersion":2}`, `{"formatVersion":1,"sources":[{"component":"engine","component":"other"}]}`, `{"sources":` + strings.Repeat("[", 65) + `0` + strings.Repeat("]", 65) + `}`} {
		var cfg candidateOperatorConfiguration
		if err := decodeCandidateObject([]byte(raw), &cfg); err == nil {
			t.Fatal("invalid JSON envelope accepted", raw)
		}
	}
}
