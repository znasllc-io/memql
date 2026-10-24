package setupproject

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseEnvelope(t *testing.T) {
	tests := []struct {
		name       string
		stdout     string
		wantErr    bool
		wantOK     bool
		wantCap    string
		wantErrMsg string // when the envelope is a failure envelope
	}{
		{
			name:   "ok envelope",
			stdout: `{"ok":true,"capability":"project.bootstrap","changed":true,"result":{"product":"acme"},"error":null}`,
			wantOK: true, wantCap: "project.bootstrap",
		},
		{
			name:   "ok envelope with trailing newline",
			stdout: "{\"ok\":true,\"capability\":\"project.bootstrap\",\"changed\":false,\"result\":{},\"error\":null}\n",
			wantOK: true, wantCap: "project.bootstrap",
		},
		{
			name:       "error envelope",
			stdout:     `{"ok":false,"capability":"project.bootstrap","changed":false,"result":{},"error":{"code":5,"message":"clone failed"}}`,
			wantOK:     false,
			wantCap:    "project.bootstrap",
			wantErrMsg: "clone failed",
		},
		{
			name: "human logs on stderr do not confuse the last-line reader",
			// In practice logs go to stderr, but be robust: the envelope is the
			// last JSON-object line.
			stdout: "INFO: cloning...\n{\"ok\":true,\"capability\":\"c\",\"changed\":true,\"result\":{},\"error\":null}\n",
			wantOK: true, wantCap: "c",
		},
		{
			name:    "no envelope",
			stdout:  "INFO: did some things\nWARNING: nothing structured here\n",
			wantErr: true,
		},
		{
			name:    "empty stdout",
			stdout:  "",
			wantErr: true,
		},
		{
			name:    "malformed json object",
			stdout:  `{"ok":true,,,}`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := ParseEnvelope([]byte(tt.stdout))
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseEnvelope err=%v, wantErr=%v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if env.OK != tt.wantOK {
				t.Errorf("OK=%v, want %v", env.OK, tt.wantOK)
			}
			if env.Capability != tt.wantCap {
				t.Errorf("Capability=%q, want %q", env.Capability, tt.wantCap)
			}
			if tt.wantErrMsg != "" {
				if env.Err() == nil || !strings.Contains(env.Err().Error(), tt.wantErrMsg) {
					t.Errorf("Err()=%v, want to contain %q", env.Err(), tt.wantErrMsg)
				}
			}
			if tt.wantOK && env.Err() != nil {
				t.Errorf("Err() on ok envelope = %v, want nil", env.Err())
			}
		})
	}
}

func TestEnvelopeDecodeResult(t *testing.T) {
	env, err := ParseEnvelope([]byte(`{"ok":true,"capability":"project.bootstrap","changed":true,"result":{"product":"acme","productOrg":"acme-io","domain":"acme.local","engineVersion":"v1.2.3","workspaceRoot":"/tmp/ws","stampedRepos":["acme-carrier","acme-client"],"dryRun":false},"error":null}`))
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	got, err := env.DecodeResult()
	if err != nil {
		t.Fatalf("DecodeResult: %v", err)
	}
	want := BootstrapResult{
		Product: "acme", ProductOrg: "acme-io", Domain: "acme.local",
		EngineVersion: "v1.2.3", WorkspaceRoot: "/tmp/ws",
		StampedRepos: []string{"acme-carrier", "acme-client"}, DryRun: false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DecodeResult\n got=%+v\nwant=%+v", got, want)
	}
}

func TestEnvelopeDecodeResultAbsent(t *testing.T) {
	env := Envelope{OK: true}
	got, err := env.DecodeResult()
	if err != nil {
		t.Fatalf("DecodeResult on absent result: %v", err)
	}
	if !reflect.DeepEqual(got, BootstrapResult{}) {
		t.Errorf("DecodeResult on absent result = %+v, want zero", got)
	}
}
