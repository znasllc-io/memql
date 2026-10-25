package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
)

// The installation declares which component and companion artifact supply its
// facts. Names, paths and release-selection policy never come from engine code.
type overlayNativeConfiguration struct {
	Component          string         `json:"component"`
	CompanionComponent string         `json:"companionComponent"`
	CompanionArtifact  string         `json:"companionArtifact"`
	Images             []overlayImage `json:"images"`
}

func (c overlayNativeConfiguration) validate() error {
	for _, value := range []string{c.Component, c.CompanionComponent, c.CompanionArtifact} {
		if !candidateTargetName.MatchString(value) {
			return errors.New("native overlay evidence requires exact component and artifact names")
		}
	}
	if err := validateOverlayImages(c.Images); err != nil {
		return err
	}
	for _, image := range c.Images {
		if image.Component != c.Component || !candidateTargetName.MatchString(image.Name) {
			return errors.New("native manifest image keys must bind the selected component")
		}
	}
	return nil
}

type overlayJSONScalar struct {
	Pointer string `json:"pointer"`
	Fact    string `json:"fact"`
}

// Values are populated only by the same-owner verified native journal reader.
// Neither clients nor installed recipes can submit a replacement value.
type overlayScalarFacts struct {
	Version, Commit, CompanionDigest string
	CompanionSize                    int64
}

var overlayStableVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func overlayPointer(pointer string) ([]string, error) {
	if !strings.HasPrefix(pointer, "/") || len(pointer) > 256 || strings.ContainsAny(pointer, "\x00\r\n") {
		return nil, errors.New("JSON binding needs a bounded absolute key pointer")
	}
	parts := strings.Split(pointer[1:], "/")
	if len(parts) > 16 {
		return nil, errors.New("JSON binding exceeds its depth bound")
	}
	for n, part := range parts {
		if part == "" {
			return nil, errors.New("JSON binding cannot name an empty object key")
		}
		for i := 0; i < len(part); i++ {
			if part[i] == '~' {
				if i+1 == len(part) || (part[i+1] != '0' && part[i+1] != '1') {
					return nil, errors.New("JSON binding has an invalid pointer escape")
				}
				i++
			}
		}
		parts[n] = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
	}
	return parts, nil
}

func validateOverlayJSONScalars(bindings []overlayJSONScalar) error {
	if len(bindings) == 0 || len(bindings) > 16 {
		return errors.New("JSON binding set must contain one to sixteen scalar facts")
	}
	for n, binding := range bindings {
		if _, err := overlayPointer(binding.Pointer); err != nil {
			return err
		}
		switch binding.Fact {
		case "componentVersion", "componentTag", "componentCommit", "companionSHA256", "companionSize":
		default:
			return errors.New("JSON binding names an unsupported verified fact")
		}
		for _, prior := range bindings[:n] {
			if binding.Pointer == prior.Pointer || strings.HasPrefix(binding.Pointer, prior.Pointer+"/") || strings.HasPrefix(prior.Pointer, binding.Pointer+"/") {
				return errors.New("JSON scalar bindings overlap")
			}
		}
	}
	return nil
}

func pinOverlayJSONScalars(before []byte, bindings []overlayJSONScalar, facts overlayScalarFacts) ([]byte, error) {
	if len(before) == 0 || len(before) > 1<<20 || validateOverlayJSONScalars(bindings) != nil || !overlayStableVersion.MatchString(facts.Version) || !installationCommit.MatchString(facts.Commit) || !stringsHasDigest(facts.CompanionDigest) || facts.CompanionSize <= 0 {
		return nil, errors.New("JSON overlay needs bounded source and verified native facts")
	}
	unique := json.NewDecoder(bytes.NewReader(before))
	unique.UseNumber()
	if candidateUniqueJSON(unique, 0) != nil {
		return nil, errors.New("JSON overlay source is ambiguous or too deeply nested")
	}
	decoder := json.NewDecoder(bytes.NewReader(before))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, errors.New("JSON overlay source must be an object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("JSON overlay source must contain exactly one object")
	}
	for _, binding := range bindings {
		parts, _ := overlayPointer(binding.Pointer)
		parent := object
		for _, key := range parts[:len(parts)-1] {
			child, ok := parent[key].(map[string]any)
			if !ok {
				return nil, errors.New("JSON overlay binding has no existing object path")
			}
			parent = child
		}
		key := parts[len(parts)-1]
		var value any
		switch binding.Fact {
		case "companionSize":
			old, ok := parent[key].(json.Number)
			if !ok {
				return nil, errors.New("JSON numeric fact requires an existing integer slot")
			}
			if n, err := old.Int64(); err != nil || n < 0 {
				return nil, errors.New("JSON numeric fact requires a nonnegative integer slot")
			}
			value = facts.CompanionSize
		default:
			if _, ok := parent[key].(string); !ok {
				return nil, errors.New("JSON text fact requires an existing string slot")
			}
			switch binding.Fact {
			case "componentVersion":
				value = facts.Version
			case "componentTag":
				value = "v" + facts.Version
			case "componentCommit":
				value = facts.Commit
			case "companionSHA256":
				value = strings.TrimPrefix(facts.CompanionDigest, "sha256:")
			}
		}
		parent[key] = value
	}
	result, err := json.MarshalIndent(object, "", "  ")
	if err != nil || len(result) >= 1<<20 {
		return nil, errors.New("JSON overlay result exceeds its bound")
	}
	return append(result, '\n'), nil
}
