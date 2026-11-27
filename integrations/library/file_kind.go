package library

import (
	_ "embed"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
)

//go:embed file_kinds.json
var fileKindJSON []byte

type directoryKindRules struct {
	Extensions map[string][]string `json:"extensions"`
	Formats    map[string]string   `json:"formats"`
	MIME       []struct {
		Kind     string `json:"kind"`
		Pattern  string `json:"pattern"`
		compiled *regexp.Regexp
	} `json:"mime"`
	Order []string `json:"order"`
}

var directoryKinds = func() directoryKindRules {
	var rules directoryKindRules
	if err := json.Unmarshal(fileKindJSON, &rules); err != nil {
		panic(err)
	}
	for n := range rules.MIME {
		rules.MIME[n].compiled = regexp.MustCompile(rules.MIME[n].Pattern)
	}
	return rules
}()

func directoryFileKind(row map[string]any) string {
	name := strings.ToLower(strings.TrimSpace(asString(row["title"])))
	if dot := strings.LastIndex(name, "."); dot > 0 {
		for kind, extensions := range directoryKinds.Extensions {
			if slices.Contains(extensions, name[dot+1:]) {
				return kind
			}
		}
	}
	mime := strings.TrimSpace(strings.SplitN(strings.ToLower(asString(row["mimeType"])), ";", 2)[0])
	for _, rule := range directoryKinds.MIME {
		if rule.compiled.MatchString(mime) {
			return rule.Kind
		}
	}
	if kind := directoryKinds.Formats[asString(row["format"])]; kind != "" {
		return kind
	}
	return "misc"
}

func directoryKindRank(kind string) int { return slices.Index(directoryKinds.Order, kind) }
func validDirectoryKind(kind string) bool {
	return kind == "all" || kind == "folders" || directoryKindRank(kind) >= 0
}
