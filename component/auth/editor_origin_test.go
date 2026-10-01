package auth

import "testing"

func TestWebEditorOrigin(t *testing.T) {
	for _, origin := range []string{"https://vscode.dev", "https://insiders.vscode.dev", "https://github.dev", "https://v--0agu67usn2p2belftkh65259a1tfjqn1ktaorveglts8k968brdf.vscode-cdn.net"} {
		if !IsWebEditorOrigin(origin) {
			t.Errorf("refused %s", origin)
		}
	}
	for _, origin := range []string{"null", "http://vscode.dev", "https://vscode.dev:443", "https://vscode.dev.evil.test", "https://evil.test/vscode.dev", "https://vscode.dev@evil.test", "https://evil@vscode.dev", "https://vscode.dev/", "https://vscode.dev?x", "https://vscode.dev#x", "https://anything.vscode-cdn.net", "https://v--abc.vscode-cdn.net"} {
		if IsWebEditorOrigin(origin) {
			t.Errorf("admitted %s", origin)
		}
	}
}
