package inference

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// parallel.go: how many requests a model takes at once, written in one
// place and read back from it.
//
// Ollama decides from OLLAMA_NUM_PARALLEL in the unit this package writes.
// The worker advertises the same number as the `max=` on every model label
// and enforces it (modelcall.Limiter), and the engine rations by it. When
// the two came from different places they drifted: the unit never set it,
// the worker read it from its OWN environment, and a person who exported it
// to the wrong process got a ceiling nothing enforced. So the unit is the
// source, and UnitParallel reads it back. Design record:
// docs/superpowers/specs/2026-10-18-fleet-tuning-design.md, D3.

// DefaultParallel is the number of slots a fresh native install gives
// Ollama for this machine class. Every slot costs memory, and the measured
// cost is the whole basis for the table.
//
// MEASURED on an RTX 5090 Laptop (24 GB) with the qwen3.8:27b the 24 and 32
// classes pull, from llama-server's own fit report in the unit's log: one
// slot at a 16K window projects 17.0 GB, and each further slot adds 748 MiB
// of recurrent state plus 38 KiB per token of cache -- 1.36 GB at 16K,
// 1.96 GB at 32K. The 0.6B embedder beside it is estimated, not measured,
// at 1.4 GB and 0.5 GB per further slot. Two slots at 16K then leave about
// 2.7 GB of the 22.4 GB the card reports free; two at 32K leave about
// 1.4 GB, under the 1.9 GB the report itself says it wants to leave, so
// the fit step would start taking layers off the GPU (not observed; the
// rollout reads the report). So the default is two for the classes that
// pull that model, and nothing is claimed for a class nobody measured:
// one is Ollama's own default and costs nothing to keep.
func DefaultParallel(class string) int {
	switch class {
	case "24", "32":
		return 2
	default:
		return 1
	}
}

// parallelEnvironment is the unit line for the stage's slots, or nothing.
// One slot is Ollama's default, so a stage that wants one writes the unit
// it always wrote.
func (s Stage) parallelEnvironment() string {
	if s.Parallel <= 1 {
		return ""
	}
	return "# Requests a model takes at once. The worker reads this line and advertises the same ceiling.\n" +
		"Environment=OLLAMA_NUM_PARALLEL=" + strconv.Itoa(s.Parallel) + "\n"
}

// UnitParallel is the OLLAMA_NUM_PARALLEL a unit declares, read the way
// systemd merges a unit with its drop-ins: texts are the main unit first
// and then each drop-in in lexical order; the last assignment of the
// variable wins; and an empty Environment= resets the list. Zero means the
// unit says nothing usable -- no line, a number below one, or a word, which
// Ollama ignores the same way.
//
// Only [Service] counts and comments are skipped. EnvironmentFile= is not
// followed: this cockpit never writes one for Ollama, and an owner who did
// has told the cockpit through models.parallel instead.
func UnitParallel(texts ...string) int {
	value := ""
	for _, text := range texts {
		inService := false
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			switch {
			case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
				continue
			case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
				inService = line == "[Service]"
				continue
			}
			if !inService {
				continue
			}
			key, rest, ok := strings.Cut(line, "=")
			if !ok || strings.TrimSpace(key) != "Environment" {
				continue
			}
			rest = strings.TrimSpace(rest)
			if rest == "" {
				value = ""
				continue
			}
			for _, word := range unitWords(rest) {
				if name, v, ok := strings.Cut(word, "="); ok && name == "OLLAMA_NUM_PARALLEL" {
					value = strings.TrimSpace(v)
				}
			}
		}
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 {
		return 0
	}
	return n
}

// unitWords splits the right-hand side of an Environment= line the way
// systemd does for the cases a unit this cockpit wrote, or an owner edited,
// contains: whitespace separates assignments, a pair of single or double
// quotes keeps spaces inside one, and a backslash keeps the next character
// as it is.
func unitWords(s string) []string {
	var (
		words   []string
		cur     strings.Builder
		quote   rune
		inWord  bool
		escaped bool
	)
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped, inWord = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

// ManagedParallel is the slots the unit this cockpit manages declares for
// its Ollama, or zero when this machine has no such unit (macOS, Docker, an
// Ollama somebody started by hand). The worker hands it to discovery as
// models.Discoverer.ManagedParallel.
func ManagedParallel() int {
	home, err := os.UserHomeDir()
	if err != nil {
		return 0
	}
	return managedParallelAt(DefaultNativeFacts(home, "").UnitPath)
}

func managedParallelAt(unitPath string) int {
	main, err := os.ReadFile(unitPath)
	if err != nil {
		return 0
	}
	texts := []string{string(main)}
	dropIns, _ := filepath.Glob(unitPath + ".d/*.conf")
	sort.Strings(dropIns)
	for _, path := range dropIns {
		if b, err := os.ReadFile(path); err == nil {
			texts = append(texts, string(b))
		}
	}
	return UnitParallel(texts...)
}
