package inference

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
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
// source of Ollama's slots, unless models.parallel or the worker's own
// OLLAMA_NUM_PARALLEL says otherwise, and UnitParallel reads it back. Design
// record: docs/superpowers/specs/2026-10-18-fleet-tuning-design.md, D3.

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
	return "# Requests a model takes at once. The worker advertises this unless " +
		"models.parallel or the worker's own OLLAMA_NUM_PARALLEL says otherwise.\n" +
		"Environment=OLLAMA_NUM_PARALLEL=" + strconv.Itoa(s.Parallel) + "\n"
}

// UnitParallel is the OLLAMA_NUM_PARALLEL a unit declares, read the way
// systemd merges a unit with its drop-ins: texts are the main unit first
// and then each drop-in in lexical order; the last assignment of the
// variable wins; and an empty Environment= resets the list. Zero means the
// unit says nothing usable -- no line, a number below one, or text Ollama
// would not read as a count either (a word, "+2", "2.0"). The value is read
// the way Ollama reads it, models.ParseOllamaCount, so the cockpit never
// advertises slots Ollama will not give.
//
// It reads the lines this cockpit writes and an owner's ordinary edits, not
// all of systemd's syntax: a backslash at the end of a line does not continue
// it, UnsetEnvironment= is not read, only the user unit and its own .d
// directory are read, and an unterminated quote runs to the end of its line.
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
					value = v
				}
			}
		}
	}
	return models.ParseOllamaCount(value)
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
	main, err := readRegularFile(unitPath)
	if err != nil {
		return 0
	}
	texts := []string{string(main)}
	// The drop-in directory is read as a directory and never matched as a
	// pattern: the path runs through the owner's home, and a glob reads its
	// brackets, backslashes and stars as syntax, so it can miss the drop-ins
	// it means. ReadDir returns the names sorted, which is the order systemd
	// applies them in. A directory that is missing or cannot be read holds no
	// drop-ins, and the unit alone decides.
	dir := unitPath + ".d"
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		name := e.Name()
		// systemd ignores hidden files and anything that is not a .conf.
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".conf") {
			continue
		}
		if b, err := readRegularFile(filepath.Join(dir, name)); err == nil {
			texts = append(texts, string(b))
		}
	}
	return UnitParallel(texts...)
}

// readRegularFile reads path if it is a regular file, or a link to one, and
// says so as an error otherwise. A unit and its drop-ins are files, and the
// reader runs under the model inventory's lock on the path every model call
// and registration takes: opening a pipe for reading blocks until something
// writes to it, so one stray FIFO named *.conf would hold the whole worker
// behind it. systemd skips anything that is not a regular file too. os.Stat
// follows a link, so a drop-in that is a link to a file still counts, and a
// link to a pipe or to nothing does not.
func readRegularFile(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return os.ReadFile(path)
}
