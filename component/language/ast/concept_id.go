package ast

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Concept-ID assembly for the DSL import-model refactor
// (docs/dsl-import-model-refactor.md decision #11).
//
// Authors declare three things on a concept:
//
//	@version("1.0.0")
//	@namespace("cognition")
//	concept participant { ... }
//
// The engine assembles the canonical row-data ID:
//
//	v1:cognition:participant
//
// The version is strict semver (MAJOR.MINOR.PATCH). Only the major
// component flows into the concept ID prefix -- minor and patch are
// documentation for additive / non-breaking schema evolution within
// a major version. Bumping major changes the ID prefix and is the
// breaking-change signal.
//
// Namespaces are colon-separated identifier segments. Each segment
// must be a lowercase-leading identifier. The same separator the
// concept ID uses, so the author surface mirrors the on-the-wire
// format.
//
// This file is the single source of truth for assembly. The concept
// registry, the row-ID encoder, the event-topic builder, and the
// validator all route through AssembleConceptId /
// ValidateAssemblyInputs so the format stays consistent.

// semverPattern enforces strict three-segment semver. Each segment
// is a non-negative integer; leading zeros allowed except on
// multi-digit (so "1.0.0" valid, "01.0.0" invalid -- caught by the
// integer-parse step).
var semverPattern = regexp.MustCompile(`^([0-9]+)\.([0-9]+)\.([0-9]+)$`)

// namespacePattern enforces colon-separated lowercase-identifier
// segments. Each segment between colons must independently match the
// lowercase-identifier shape. Examples:
//
//	"cognition"                -> v1:cognition:<name>
//	"cognition:client:tool"    -> v1:cognition:client:tool:<name>
var namespacePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(:[a-z][a-z0-9_]*)*$`)

// conceptNamePattern enforces the concept-name-must-be-lowercase-first
// (camelCase by convention) rule from decision #13d.
var conceptNamePattern = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*$`)

// SemverVersion is the parsed @version("X.Y.Z") triple. Only Major
// flows into the assembled concept ID; Minor and Patch are stored
// for documentation + lint surfaces.
type SemverVersion struct {
	Major int64
	Minor int64
	Patch int64
}

// String renders the triple as "X.Y.Z". Round-trips through
// ParseSemver.
func (v SemverVersion) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// ParseSemver parses a strict "X.Y.Z" string into a SemverVersion.
// Rejects any other form (single integer, two-component, suffixes
// like "-rc1" or "+build").
func ParseSemver(s string) (SemverVersion, error) {
	m := semverPattern.FindStringSubmatch(s)
	if m == nil {
		return SemverVersion{}, fmt.Errorf("@version %q must be strict semver \"MAJOR.MINOR.PATCH\" (e.g. \"1.0.0\")", s)
	}
	major, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return SemverVersion{}, fmt.Errorf("@version major segment %q: %w", m[1], err)
	}
	minor, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		return SemverVersion{}, fmt.Errorf("@version minor segment %q: %w", m[2], err)
	}
	patch, err := strconv.ParseInt(m[3], 10, 64)
	if err != nil {
		return SemverVersion{}, fmt.Errorf("@version patch segment %q: %w", m[3], err)
	}
	if major < 1 {
		return SemverVersion{}, fmt.Errorf("@version major must be >= 1, got %d", major)
	}
	if minor < 0 || patch < 0 {
		return SemverVersion{}, fmt.Errorf("@version minor + patch must be >= 0")
	}
	return SemverVersion{Major: major, Minor: minor, Patch: patch}, nil
}

// ValidateAssemblyInputs returns an error if any of the three inputs
// to a concept-ID assembly violates the documented format rules.
// Returns nil if all three are well-formed.
//
// Used at concept-registration time (engine startup) and at lint time
// (memql-cockpit lint).
func ValidateAssemblyInputs(version SemverVersion, namespace, name string) error {
	if version.Major < 1 {
		return fmt.Errorf("@version major must be >= 1, got %d", version.Major)
	}
	if !namespacePattern.MatchString(namespace) {
		return fmt.Errorf("@namespace %q must match %s (colon-separated lowercase identifiers)", namespace, namespacePattern.String())
	}
	if !conceptNamePattern.MatchString(name) {
		return fmt.Errorf("concept name %q must start with a lowercase letter (camelCase for multi-word concepts)", name)
	}
	return nil
}

// AssembleConceptId returns the canonical concept ID string given
// the three structural inputs. Format: `v<MAJOR>:<namespace>:<name>`.
// Minor + patch are intentionally not in the prefix -- bumping major
// is the only operation that changes the ID surface.
//
// Examples:
//
//	AssembleConceptId(SemverVersion{1,0,0}, "cognition", "participant")
//	  ==> "v1:cognition:participant"
//
//	AssembleConceptId(SemverVersion{1,2,3}, "cognition:client:tool", "request")
//	  ==> "v1:cognition:client:tool:request"
func AssembleConceptId(version SemverVersion, namespace, name string) string {
	return fmt.Sprintf("v%d:%s:%s", version.Major, namespace, name)
}

// ExtractVersionAttribute scans a concept's attributes for
// @version("X.Y.Z") and returns the parsed semver. Returns
// zero-value + ok=false when the attribute is absent. Returns an
// error when the attribute is present but its value is not a strict
// semver string.
func ExtractVersionAttribute(attrs []*Attribute) (SemverVersion, bool, error) {
	for _, a := range attrs {
		if a == nil || a.Name != "version" {
			continue
		}
		s, ok := a.Value.(string)
		if !ok {
			return SemverVersion{}, true, fmt.Errorf("@version must be a string literal of the form \"MAJOR.MINOR.PATCH\", got %T", a.Value)
		}
		v, err := ParseSemver(strings.TrimSpace(s))
		if err != nil {
			return SemverVersion{}, true, err
		}
		return v, true, nil
	}
	return SemverVersion{}, false, nil
}

// ExtractNamespaceAttribute scans a concept's attributes for
// @namespace("...") and returns the string value. Returns ""
// + ok=false when absent. Returns an error when the attribute
// is present but its value is not a string or fails the
// colon-separated lowercase-identifier pattern.
func ExtractNamespaceAttribute(attrs []*Attribute) (string, bool, error) {
	for _, a := range attrs {
		if a == nil || a.Name != "namespace" {
			continue
		}
		s, ok := a.Value.(string)
		if !ok {
			return "", true, fmt.Errorf("@namespace must be a string literal, got %T", a.Value)
		}
		if !namespacePattern.MatchString(s) {
			return "", true, fmt.Errorf("@namespace %q must match %s (colon-separated lowercase identifiers)", s, namespacePattern.String())
		}
		return s, true, nil
	}
	return "", false, nil
}

// AssembleConceptIdFromDecl is the convenience entry point for the
// loader: pulls @version + @namespace off a ConceptDecl, validates
// the name, and returns the assembled ID + a structured error path
// for each individual failure.
//
// Returns the empty string when @version or @namespace is missing
// (transitional state -- many concept files don't carry these yet).
// The caller decides whether absence is an error: at lint time it
// might be a warning; at engine-startup it might be a hard fail
// once migration completes.
func AssembleConceptIdFromDecl(decl *ConceptDecl) (string, error) {
	if decl == nil {
		return "", fmt.Errorf("concept declaration is nil")
	}

	version, hasVersion, vErr := ExtractVersionAttribute(decl.Attributes)
	if vErr != nil {
		return "", fmt.Errorf("concept %q: %w", decl.Name, vErr)
	}
	namespace, hasNamespace, nErr := ExtractNamespaceAttribute(decl.Attributes)
	if nErr != nil {
		return "", fmt.Errorf("concept %q: %w", decl.Name, nErr)
	}
	if !hasVersion || !hasNamespace {
		return "", nil
	}

	if err := ValidateAssemblyInputs(version, namespace, decl.Name); err != nil {
		return "", fmt.Errorf("concept %q: %w", decl.Name, err)
	}
	return AssembleConceptId(version, namespace, decl.Name), nil
}
