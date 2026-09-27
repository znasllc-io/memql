package procedure

import (
	"strings"
)

// network.go -- may a recorded command send something out of the sandbox?
// (review finding: shadow must not repeat external side effects; epic
// memql#5408).
//
// seams.go promises a shadow is never a delivery outside the workbench, and
// the workbench runs real commands with full egress: a recorded `curl -X POST
// <webhook>` or `git push`, replayed beside every recording that matches it,
// would post the webhook or push the branch again each time. This is the one
// PURE answer the runner asks of an exec step before it lets it run:
//
//	shadow           a marked step is compared DRY -- the call it would make
//	                 held to the app's own call -- and never dispatched
//	canary, trusted  a completed marked step is a side effect in the guidance,
//	                 so the app is told not to repeat it
//
// THE LIST -- a command word, and for some the word that follows it:
//
//	curl, wget      a method other than GET or HEAD, or any body or upload:
//	                curl -d, --data*, -F, --form*, -T, --upload-file, --json,
//	                -X/--request; wget --post-*, --body-*, --method
//	git push; npm, pnpm, yarn publish; twine upload; gem push;
//	cargo publish; docker push
//	kubectl apply, create, delete, patch, replace, scale, rollout
//	gh, az, aws, gcloud                              (any use)
//	ssh, scp, sftp, rsync, nc, ncat, telnet, ftp     (any use)
//
// EVERY COMMAND WORD IS READ, the way component/procedure.UsedTools reads
// them: each simple command after &&, ||, ;, |, & and a newline, past NAME=value
// assignments and the wrappers that run another command (sudo, env, timeout,
// nohup, xargs, ...), inside a shell's -c script, a subshell, and a $(...) or
// `...` substitution. An argument vector (Codex) is read as the command it is.
//
// PLAIN READS ARE DELIBERATELY NOT ON THE LIST -- a GET, a clone, a fetch, a
// package install. A procedure that fetches what its recordings fetched has to
// fetch it again on replay, or the step that reads the result has nothing to
// read: marking reads would compare dry the very steps whose output the next
// step depends on, and a comparison made dry cannot catch a fetch that answers
// differently. What a read does on the far side is what it did when the app
// sent it.
//
// THE LIMIT, stated rather than hidden: this reads the command line, not what
// runs. A script the procedure runs (`./deploy.sh`, `make release`, `npm run
// ship`) can still reach the network, and nothing here sees into it. And a
// step compared dry produces nothing: a later step that reads a file it would
// have written finds none and its comparison fails -- which errs toward never
// promoting, never toward repeating a send.

// sendsOutside reports whether an exec step's command may send something out
// of the sandbox, and names the command that may.
func sendsOutside(args map[string]any) (bool, string) {
	for _, key := range []string{"command", "cmd", "argv"} {
		switch v := args[key].(type) {
		case string:
			if sends, why := lineSends(v, 0); sends {
				return true, why
			}
		case []string:
			if sends, why := commandSends(v, 0); sends {
				return true, why
			}
		case []any:
			words := make([]string, len(v))
			for n, w := range v {
				words[n], _ = w.(string)
			}
			if sends, why := commandSends(words, 0); sends {
				return true, why
			}
		}
	}
	return false, ""
}

// maxShellDepth bounds nested shells and substitutions: a command line that
// nests past it is not read further, which a recording never does.
const maxShellDepth = 4

// lineSends reads a command line's simple commands, and what each one's
// substitutions run.
func lineSends(line string, depth int) (bool, string) {
	if depth > maxShellDepth {
		return false, ""
	}
	for _, cmd := range simpleCommands(line) {
		if sends, why := commandSends(cmd.words, depth); sends {
			return true, why
		}
		for _, sub := range cmd.subs {
			if sends, why := lineSends(sub, depth+1); sends {
				return true, why
			}
		}
	}
	return false, ""
}

// simpleCommand is one command of a line: its words, and the text of every
// $(...) or `...` substitution inside them.
type simpleCommand struct {
	words []string
	subs  []string
}

// simpleCommands splits a command line into simple commands: words by
// whitespace, commands by ; & | ( ) and newline -- all outside quotes, and
// whether or not spaces surround them. Single quotes are literal; double
// quotes are literal but for escapes and substitutions, which are captured
// whole wherever they sit.
func simpleCommands(line string) []simpleCommand {
	var (
		out    []simpleCommand
		cur    simpleCommand
		word   strings.Builder
		inWord bool
		quote  rune
	)
	flushWord := func() {
		if inWord {
			cur.words = append(cur.words, word.String())
			word.Reset()
			inWord = false
		}
	}
	endCommand := func() {
		flushWord()
		if len(cur.words) > 0 || len(cur.subs) > 0 {
			out = append(out, cur)
		}
		cur = simpleCommand{}
	}
	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		case r == '\\':
			if i+1 < len(rs) {
				word.WriteRune(rs[i+1])
				i++
			}
			inWord = true
			continue
		case r == '$' && i+1 < len(rs) && rs[i+1] == '(':
			body, end := captureParen(rs, i+2)
			cur.subs = append(cur.subs, body)
			word.WriteString("$()")
			inWord = true
			i = end
			continue
		case r == '`':
			end := i + 1
			for end < len(rs) && rs[end] != '`' {
				end++
			}
			cur.subs = append(cur.subs, string(rs[i+1:min(end, len(rs))]))
			word.WriteString("``")
			inWord = true
			i = end
			continue
		case quote == '"':
			if r == '"' {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		case r == '\'' || r == '"':
			quote = r
			inWord = true
			continue
		}
		switch r {
		case ' ', '\t', '\r':
			flushWord()
		case ';', '\n', '&', '|', '(', ')':
			endCommand()
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	endCommand()
	return out
}

// captureParen reads a $( substitution's body from start to its matching
// close, and answers the body and the index of the close (the last rune when
// it never closes).
func captureParen(rs []rune, start int) (string, int) {
	depth := 1
	var quote rune
	for i := start; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '(':
			depth++
		case r == ')':
			depth--
			if depth == 0 {
				return string(rs[start:i]), i
			}
		}
	}
	return string(rs[min(start, len(rs)):]), len(rs) - 1
}

// shells run their -c argument as a command line.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "ash": true}

// wrappers run the command that follows them. The value names each one's
// options that take a separate value, so the value is not read as the
// command.
var wrappers = map[string]map[string]bool{
	"sudo":    {"-u": true, "-g": true, "-p": true, "-C": true, "-D": true, "-r": true, "-t": true, "-U": true, "-h": true},
	"doas":    {"-u": true, "-C": true},
	"env":     {"-u": true, "-S": true, "-C": true},
	"nice":    {"-n": true},
	"ionice":  {"-c": true, "-n": true, "-p": true},
	"timeout": {"-s": true, "-k": true},
	"stdbuf":  {"-i": true, "-o": true, "-e": true},
	"xargs":   {"-n": true, "-I": true, "-P": true, "-L": true, "-s": true, "-d": true, "-E": true, "-a": true},
	"exec":    {"-a": true},
	"command": {},
	"nohup":   {},
	"time":    {},
}

// commandSends reads one simple command's words.
func commandSends(words []string, depth int) (bool, string) {
	i := 0
	for i < len(words) {
		w := words[i]
		if isAssignmentWord(w) {
			i++
			continue
		}
		skip, isWrapper := wrapperArgs(baseName(w), words[i+1:])
		if !isWrapper {
			break
		}
		i += 1 + skip
	}
	if i >= len(words) {
		return false, ""
	}
	name, rest := baseName(words[i]), words[i+1:]
	if shells[name] {
		for n, a := range rest {
			if len(a) >= 2 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(a[1:], 'c') && n+1 < len(rest) {
				return lineSends(rest[n+1], depth+1)
			}
		}
		return false, ""
	}
	return commandWordSends(name, rest)
}

// wrapperArgs is how many words after a wrapper are its own: its options,
// their values, env's assignments, and timeout's duration.
func wrapperArgs(name string, rest []string) (int, bool) {
	valueOpts, ok := wrappers[name]
	if !ok {
		return 0, false
	}
	n := 0
scan:
	for n < len(rest) {
		w := rest[n]
		switch {
		case w == "--":
			n++
			break scan
		case len(w) > 1 && w[0] == '-':
			n++
			if valueOpts[w] && n < len(rest) {
				n++
			}
		case name == "env" && isAssignmentWord(w):
			n++
		default:
			break scan
		}
	}
	if name == "timeout" && n < len(rest) {
		n++ // the duration
	}
	return n, true
}

// Value-taking global options of the CLIs whose subcommand decides.
var (
	gitValueOptions     = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true, "--config-env": true}
	kubectlValueOptions = map[string]bool{"-n": true, "--namespace": true, "--context": true, "--kubeconfig": true, "--cluster": true, "--user": true, "-s": true, "--server": true, "--token": true, "--as": true, "--as-group": true}
	dockerValueOptions  = map[string]bool{"-H": true, "--host": true, "--context": true, "-c": true, "--config": true, "-l": true, "--log-level": true}
	kubectlWrites       = map[string]bool{"apply": true, "create": true, "delete": true, "patch": true, "replace": true, "scale": true, "rollout": true}
)

// commandWordSends is the list, applied to one command.
func commandWordSends(name string, args []string) (bool, string) {
	switch name {
	case "curl":
		if why, sends := curlSends(args); sends {
			return true, "curl " + why
		}
	case "wget":
		if why, sends := wgetSends(args); sends {
			return true, "wget " + why
		}
	case "git":
		if sub := positionals(args, gitValueOptions, 1); len(sub) == 1 && sub[0] == "push" {
			return true, "git push"
		}
	case "npm", "pnpm", "yarn":
		for _, p := range positionals(args, nil, 2) {
			if p == "publish" {
				return true, name + " publish"
			}
		}
	case "twine", "gem", "cargo":
		verb := map[string]string{"twine": "upload", "gem": "push", "cargo": "publish"}[name]
		if sub := positionals(args, nil, 1); len(sub) == 1 && sub[0] == verb {
			return true, name + " " + verb
		}
	case "docker":
		sub := positionals(args, dockerValueOptions, 2)
		if len(sub) > 0 && (sub[0] == "push" || (sub[0] == "image" && len(sub) > 1 && sub[1] == "push")) {
			return true, "docker push"
		}
	case "kubectl":
		if sub := positionals(args, kubectlValueOptions, 1); len(sub) == 1 && kubectlWrites[sub[0]] {
			return true, "kubectl " + sub[0]
		}
	case "gh", "az", "aws", "gcloud":
		return true, name + ", a cloud or forge command line whose every use reaches the network"
	case "ssh", "scp", "sftp", "rsync", "nc", "ncat", "telnet", "ftp":
		return true, name + ", a remote transfer or connection"
	}
	return false, ""
}

// curlSends reads curl's options for a body, an upload, or a method other
// than GET or HEAD. A short-option cluster is read letter by letter; an option
// that takes a value takes the rest of the cluster, or the next word.
func curlSends(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return "", false
		case strings.HasPrefix(a, "--"):
			name, val, hasVal := strings.Cut(a[2:], "=")
			switch {
			case strings.HasPrefix(name, "data"), strings.HasPrefix(name, "form"), name == "json", name == "upload-file":
				return "--" + name, true
			case name == "request":
				if !hasVal && i+1 < len(args) {
					i++
					val = args[i]
				}
				if !readMethod(val) {
					return "--request " + val, true
				}
			}
		case len(a) > 1 && a[0] == '-':
			letters := a[1:]
			for j := 0; j < len(letters); j++ {
				c := letters[j]
				switch {
				case c == 'd' || c == 'F' || c == 'T':
					return "-" + string(c), true
				case c == 'X':
					method := letters[j+1:]
					if method == "" && i+1 < len(args) {
						i++
						method = args[i]
					}
					if !readMethod(method) {
						return "-X " + method, true
					}
					j = len(letters)
				case curlValueShort[c]:
					if j == len(letters)-1 && i+1 < len(args) {
						i++
					}
					j = len(letters)
				}
			}
		}
	}
	return "", false
}

// curlValueShort are curl's one-letter options that take a value.
var curlValueShort = map[byte]bool{
	'A': true, 'b': true, 'c': true, 'C': true, 'D': true, 'e': true, 'E': true, 'H': true, 'K': true, 'm': true,
	'o': true, 'P': true, 'Q': true, 'r': true, 't': true, 'u': true, 'U': true, 'w': true, 'x': true, 'y': true,
	'Y': true, 'z': true,
}

// wgetSends reads wget's options for a body or a method other than GET or
// HEAD.
func wgetSends(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			continue
		}
		name, val, hasVal := strings.Cut(a[2:], "=")
		switch {
		case strings.HasPrefix(name, "post-"), strings.HasPrefix(name, "body-"):
			return "--" + name, true
		case name == "method":
			if !hasVal && i+1 < len(args) {
				i++
				val = args[i]
			}
			if !readMethod(val) {
				return "--method " + val, true
			}
		}
	}
	return "", false
}

// readMethod reports an HTTP method that only reads. An empty one is no
// method at all, and names nothing to send.
func readMethod(m string) bool {
	switch strings.ToUpper(strings.TrimSpace(m)) {
	case "", "GET", "HEAD":
		return true
	}
	return false
}

// positionals are up to max of a command's arguments that are not options,
// skipping the value of an option valueOpts names.
func positionals(args []string, valueOpts map[string]bool, max int) []string {
	var out []string
	for i := 0; i < len(args) && len(out) < max; i++ {
		a := args[i]
		if len(a) > 1 && a[0] == '-' {
			if valueOpts[a] && i+1 < len(args) {
				i++
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

// isAssignmentWord is NAME=value, which a shell reads as an assignment before
// a command rather than as the command.
func isAssignmentWord(w string) bool {
	name, _, found := strings.Cut(w, "=")
	if !found || name == "" {
		return false
	}
	for i, r := range name {
		letter := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !letter && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// baseName is a command word without its directory: /usr/bin/curl is curl.
func baseName(w string) string {
	if i := strings.LastIndex(w, "/"); i >= 0 {
		return w[i+1:]
	}
	return w
}
