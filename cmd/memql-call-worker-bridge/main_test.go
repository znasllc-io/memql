package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const validLease = `{"id":"one","action":"lease","siteId":"site","roomId":"room"}`

func TestRequestBoundaryRefusesQueryPassthroughAndAmbiguity(t *testing.T) {
	for _, input := range []string{
		`[]`, `null`, validLease + `{}`, strings.Replace(validLease, `"lease"`, `"query"`, 1),
		strings.Replace(validLease, `"siteId":"site"`, `"siteId":"site","siteId":"other"`, 1),
		strings.Replace(validLease, `"roomId":"room"`, `"roomId":"room","query":"private"`, 1),
		strings.Replace(validLease, `"roomId":"room"`, `"roomId":"room","event":null`, 1),
		strings.Replace(validLease, `"roomId":"room"`, `"roomId":"room","reset":null`, 1),
		strings.Replace(validLease, `"id":"one"`, `"id":"credential with whitespace"`, 1),
		`{"id":"one","action":"publish","siteId":"site","roomId":"room","leaseId":"lease","revision":1.5,"event":{"text":"hello"}}`,
		`{"id":"one","action":"publish","siteId":"site","roomId":"room","leaseId":"lease","revision":1,"event":{}}`,
		`{"id":"one","action":"rooms","siteId":"site","roomId":""}`,
		`{"id":"one","action":"rooms","siteId":"site","event":{}}`,
		`{"id":"one","action":"lease","siteId":"site","roomId":"room","maxTracks":null}`,
		`{"id":"one","action":"lease","siteId":"site","roomId":"room","maxTracks":301}`,
		`{"id":"one","action":"lease","siteId":"site","roomId":"room","maxTracks":-1}`,
		`{"id":"one","action":"lease","siteId":"site","roomId":"room","maxTracks":0.5}`,
		`{"id":"one","action":"publish","siteId":"site","roomId":"room","leaseId":"lease","revision":1,"event":{"text":"hello"},"maxTracks":0}`,
	} {
		_, err := parseRequest([]byte(input))
		require.Error(t, err, input)
	}
	req, err := parseRequest([]byte(validLease))
	require.NoError(t, err)
	require.Equal(t, "one", req.ID)
	req, err = parseRequest([]byte(`{"id":"two","action":"publish","siteId":"site","roomId":"room","leaseId":"lease","revision":2,"event":{"text":"'); builtin unrelated({})","sequence":1}}`))
	require.NoError(t, err)
	require.Equal(t, json.Number("1"), req.Event["sequence"])
}

func TestSerialProtocolDoesNotEchoBackendErrorsOrReplay(t *testing.T) {
	var out bytes.Buffer
	calls := 0
	err := serve(context.Background(), strings.NewReader("{}\n"+validLease+"\n"+strings.Replace(validLease, "one", "two", 1)+"\n"), &out, func(ctx context.Context, req request) (any, error) {
		calls++
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		require.LessOrEqual(t, time.Until(deadline), operationTimeout)
		if req.ID == "one" {
			return map[string]any{"private": "must not escape"}, errors.New("Bearer secret diagnostic")
		}
		return map[string]any{"leaseId": "allowed"}, nil
	})
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 3)
	require.JSONEq(t, `{"id":"","ok":false,"error":"invalid_request"}`, lines[0])
	require.JSONEq(t, `{"id":"one","ok":false,"error":"request_failed"}`, lines[1])
	require.JSONEq(t, `{"id":"two","ok":true,"result":{"leaseId":"allowed"}}`, lines[2])
	require.NotContains(t, out.String(), "secret")
	require.NotContains(t, out.String(), "private")
}

func TestOversizeInputStopsWithoutExecutingAndOversizeReplyIsBounded(t *testing.T) {
	var out bytes.Buffer
	called := false
	err := serve(context.Background(), strings.NewReader(strings.Repeat("x", maxRequestBytes+8)+"\n"+validLease+"\n"), &out, func(context.Context, request) (any, error) {
		called = true
		return nil, nil
	})
	require.Error(t, err)
	require.False(t, called)
	require.JSONEq(t, `{"id":"","ok":false,"error":"invalid_request"}`, out.String())
	out.Reset()
	require.NoError(t, writeResponse(&out, response{ID: "one", OK: true, Result: strings.Repeat("x", maxResponseBytes)}))
	require.JSONEq(t, `{"id":"one","ok":false,"error":"response_too_large"}`, out.String())
}

func TestCancelledParentStopsFurtherRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	calls := 0
	err := serve(ctx, strings.NewReader(validLease+"\n"+validLease+"\n"), &out, func(context.Context, request) (any, error) {
		calls++
		cancel()
		return nil, context.Canceled
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, calls)
}

func TestEndpointAndCredentialCannotDowngradeOrLeak(t *testing.T) {
	for _, endpoint := range []string{"", "api.example:443", "http://api.example:80", "https://user:pass@api.example", "https://api.example/path", "https://api.example?token=secret", "https://api.example#fragment", "https://api.example:0", "grpc://127.0.0.1:70000"} {
		_, err := configuration(func(name string) string {
			switch name {
			case "MEMQL_WORKER_ENDPOINT":
				return endpoint
			case "MEMQL_WORKER_OPERATOR_TOKEN":
				return "private-token"
			case "MEMQL_WORKER_ALLOW_LOCAL":
				return "1"
			}
			return ""
		})
		require.Error(t, err, endpoint)
		require.NotContains(t, err.Error(), "private-token")
	}
	for _, local := range []string{"", "1"} {
		cfg, err := configuration(func(name string) string {
			return map[string]string{"MEMQL_WORKER_ENDPOINT": "grpc://localhost:50050", "MEMQL_WORKER_OPERATOR_TOKEN": "private-token", "MEMQL_WORKER_ALLOW_LOCAL": local}[name]
		})
		if local == "" {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
			require.Equal(t, "grpc://127.0.0.1:50050", cfg.Endpoint)
		}
	}
}
