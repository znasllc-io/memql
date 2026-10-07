package releasecatalog

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSourceConfigurationRefusesAmbiguityAndUntrustedTransport(t *testing.T) {
	valid := source{ID: "publisher", Publisher: "fixture", Endpoint: "https://api.example.com", CredentialSecret: "PUBLISHER_TOKEN", PublicKeys: map[string]string{"key": base64.StdEncoding.EncodeToString(make([]byte, 32))}}
	for _, fault := range []string{"version", "unknown field", "duplicate field", "case duplicate", "trailing JSON", "size", "source count", "duplicate identity", "http", "userinfo", "path", "query", "fragment", "invalid port", "key size", "key identity", "no keys", "bad roots"} {
		t.Run(fault, func(t *testing.T) {
			s := valid
			s.PublicKeys = map[string]string{"key": valid.PublicKeys["key"]}
			switch fault {
			case "http":
				s.Endpoint = "http://api.example.com"
			case "userinfo":
				s.Endpoint = "https://token@api.example.com"
			case "path":
				s.Endpoint += "/catalog"
			case "query":
				s.Endpoint += "?token=secret"
			case "fragment":
				s.Endpoint += "#catalog"
			case "invalid port":
				s.Endpoint += ":port"
			case "key size":
				s.PublicKeys["key"] = base64.StdEncoding.EncodeToString(make([]byte, 31))
			case "key identity":
				s.PublicKeys = map[string]string{"../key": valid.PublicKeys["key"]}
			case "no keys":
				s.PublicKeys = nil
			case "bad roots":
				s.RootCAPEM = "not a certificate"
			}
			cfg := configuration{FormatVersion: 1, Sources: []source{s}}
			switch fault {
			case "version":
				cfg.FormatVersion = 2
			case "duplicate identity":
				cfg.Sources = append(cfg.Sources, s)
			case "source count":
				cfg.Sources = make([]source, 17)
			}
			body, err := json.Marshal(cfg)
			require.NoError(t, err)
			raw := string(body)
			switch fault {
			case "unknown field":
				raw = `{"extra":true,` + raw[1:]
			case "duplicate field":
				raw = `{"formatVersion":1,` + raw[1:]
			case "case duplicate":
				raw = `{"FormatVersion":1,` + raw[1:]
			case "trailing JSON":
				raw += `{}`
			case "size":
				raw += strings.Repeat(" ", 256<<10)
			}
			r := New(func(context.Context, string) (string, error) { return raw, nil }, func(context.Context, string) (string, error) {
				t.Fatal("invalid configuration reached credentials")
				return "", nil
			})
			_, err = r.Sources(readerContext())
			require.Error(t, err)
			_, err = r.List(readerContext(), "publisher", "", 1)
			require.Error(t, err)
		})
	}
	for endpoint, want := range map[string]string{"https://api.example.com/": "https://api.example.com:443", "https://[::1]:443": "https://[::1]:443"} {
		s := valid
		s.Endpoint = endpoint
		_, _, err := s.trust()
		require.NoError(t, err)
		require.Equal(t, want, s.endpoint())
	}
}

func TestUnavailableConfigurationIsNotAnEmptyPublisherList(t *testing.T) {
	r := New(func(context.Context, string) (string, error) { return "", errors.New("private configuration failure") }, nil)
	_, err := r.Sources(readerContext())
	require.ErrorContains(t, err, "could not be read")
	require.NotContains(t, err.Error(), "private")
	r = New(func(context.Context, string) (string, error) { return `{"formatVersion":1,"sources":[]}`, nil }, nil)
	sources, err := r.Sources(readerContext())
	require.NoError(t, err)
	require.Empty(t, sources)
}
