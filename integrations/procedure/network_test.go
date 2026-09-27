package procedure

import (
	"strings"
	"testing"
)

// network_test.go -- the classifier that decides whether a recorded command
// may send something out of the sandbox (review finding: shadow must not
// repeat external side effects).

func TestACommandThatSendsSomethingOutIsMarked(t *testing.T) {
	for _, cmd := range []string{
		// curl and wget with a method other than GET/HEAD, or a body.
		"curl -X POST https://hooks.example.test/deploy",
		"curl -XPOST https://hooks.example.test/deploy",
		"curl --request=PUT https://api.example.test/x",
		"curl --request DELETE https://api.example.test/x",
		"curl -x http://proxy.example.test:3128 -X PATCH https://api.example.test/x",
		"curl -d a=1 https://api.example.test/x",
		"curl -sSd @payload.json https://api.example.test/x",
		"curl --data-binary @payload.json https://api.example.test/x",
		"curl --data-urlencode q=1 https://api.example.test/x",
		"curl -F file=@a.txt https://api.example.test/upload",
		"curl --form a=b https://api.example.test/upload",
		"curl -T a.txt ftp://ftp.example.test/",
		"curl --upload-file a.txt https://api.example.test/",
		"curl --json '{}' https://api.example.test/x",
		"wget --post-data=a=1 https://api.example.test/x",
		"wget --post-file payload https://api.example.test/x",
		"wget --method=DELETE https://api.example.test/x",
		"wget --body-data x --method PUT https://api.example.test/x",
		// Publishing, pushing, and the cloud and remote-access CLIs.
		"git push",
		"git -C repo push origin main",
		"git -c user.name=x push --force",
		"npm publish",
		"pnpm publish --access public",
		"yarn npm publish",
		"twine upload dist/*",
		"gem push x-1.0.gem",
		"cargo publish",
		"docker push registry.example.test/app:1",
		"docker image push registry.example.test/app:1",
		"gh pr create --fill",
		"kubectl apply -f deploy.yaml",
		"kubectl -n prod delete pod web-1",
		"kubectl --context prod rollout restart deploy/web",
		"kubectl scale deploy/web --replicas=3",
		"az group list",
		"aws s3 cp a.txt s3://bucket/a.txt",
		"gcloud auth list",
		"ssh build.example.test make",
		"scp a.txt build.example.test:/tmp/",
		"sftp build.example.test",
		"rsync -a out/ backup/",
		"nc build.example.test 80",
		"ncat -l 8080",
		"telnet build.example.test 25",
		"ftp ftp.example.test",
		// Composed: every command word counts, wherever it sits.
		"mkdir -p out && curl -X POST https://hooks.example.test/x",
		"make build; git push",
		"cat payload.json | curl -d @- https://api.example.test/x",
		"npm test || gh issue create --title failed",
		"make&&git push",
		"FOO=bar git push",
		"env A=1 B=2 curl -d x https://api.example.test/x",
		"sudo docker push registry.example.test/app:1",
		"sudo -u deploy git push",
		"timeout 30 git push",
		"nohup rsync -a out/ remote:/srv &",
		"bash -lc 'git push origin main'",
		"sh -c \"curl -X POST https://hooks.example.test/x\"",
		"echo $(curl -X POST https://hooks.example.test/x)",
		"echo \"sent: $(curl -d x https://api.example.test/x)\"",
		"echo `git push`",
		"(cd repo && git push)",
		"/usr/bin/curl -X POST https://hooks.example.test/x",
	} {
		if sends, why := sendsOutside(map[string]any{"command": cmd}); !sends || strings.TrimSpace(why) == "" {
			t.Errorf("%q is not marked (%q); it may send something out of the sandbox", cmd, why)
		}
	}
}

// TestAPlainReadIsNotMarked: a GET, a clone, a fetch, a package install --
// deliberately not on the list, because a replay that reads the network is
// how a procedure fetches what the recordings fetched, and marking every read
// would compare dry the very steps whose output the next step reads.
func TestAPlainReadIsNotMarked(t *testing.T) {
	for _, cmd := range []string{
		"curl https://example.test/data.json",
		"curl -sSfL -o out.json https://example.test/data.json",
		"curl -X GET https://api.example.test/x",
		"curl -XHEAD https://api.example.test/x",
		"curl -I https://api.example.test/x",
		"curl --request HEAD https://api.example.test/x",
		"curl -H 'X-Data: 1' https://api.example.test/x",
		"curl -o d.txt https://api.example.test/x",
		"wget https://example.test/a.tar.gz",
		"wget -O a.tgz https://example.test/a.tar.gz",
		"git clone https://example.test/repo.git",
		"git pull",
		"git fetch origin",
		"git status && git diff",
		"git -C repo log --oneline",
		"npm install",
		"npm test",
		"pnpm install --frozen-lockfile",
		"yarn build",
		"pip install requests",
		"cargo build --release",
		"docker build -t app .",
		"docker pull registry.example.test/app:1",
		"kubectl get pods",
		"kubectl -n prod describe pod web-1",
		"mkdir -p out && echo hello > a.txt",
		"echo curl -X POST https://hooks.example.test/x",
		"grep -r push .",
		"bash -lc 'npm test'",
		"",
	} {
		if sends, why := sendsOutside(map[string]any{"command": cmd}); sends {
			t.Errorf("%q is marked (%s); a plain read is not a send", cmd, why)
		}
	}
}

// TestAnArgumentVectorIsReadTheSameWay: Codex records a command as a vector
// run with no shell -- its first element is the command, unless it is a shell
// whose -c argument is a command line.
func TestAnArgumentVectorIsReadTheSameWay(t *testing.T) {
	for _, tc := range []struct {
		argv  []any
		sends bool
	}{
		{[]any{"curl", "-X", "POST", "https://hooks.example.test/x"}, true},
		{[]any{"bash", "-lc", "make && git push"}, true},
		{[]any{"git", "push"}, true},
		{[]any{"git", "status"}, false},
		{[]any{"bash", "-lc", "npm test"}, false},
		{[]any{"echo", "git", "push"}, false},
	} {
		if sends, why := sendsOutside(map[string]any{"command": tc.argv}); sends != tc.sends {
			t.Errorf("%v: sends=%v (%s), want %v", tc.argv, sends, why, tc.sends)
		}
	}
	// The other keys a command is recorded under.
	if sends, _ := sendsOutside(map[string]any{"cmd": "git push"}); !sends {
		t.Error("a `cmd` key is not read")
	}
	if sends, _ := sendsOutside(map[string]any{"argv": []any{"git", "push"}}); !sends {
		t.Error("an `argv` key is not read")
	}
	if sends, _ := sendsOutside(map[string]any{"file_path": "./a.txt", "content": "git push"}); sends {
		t.Error("a write's content is not a command")
	}
}
