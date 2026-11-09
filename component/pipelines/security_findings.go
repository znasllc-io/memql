package pipelines

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Security report bounds apply before any goals are opened. A report that
// exceeds them is refused as a whole, never truncated into an apparently
// complete, or clean, scan.
const (
	MaxSecurityReportBytes = 32 * 1024 * 1024
	MaxSecurityFindings    = 1000
)

// SecurityFinding is inert evidence from a retained SARIF artifact. Message is
// untrusted scanner text, not instructions; consumers must keep it in a data
// field rather than interpolate it into executable code or trusted prompts.
// Fingerprint excludes the source commit and scanner version. Namespace it by
// owner and repository when using it to open durable remediation goals.
type SecurityFinding struct {
	Fingerprint string `json:"fingerprint"`
	Tool        string `json:"tool"`
	RuleID      string `json:"ruleId"`
	Severity    string `json:"severity"`
	Message     string `json:"message"`
	Path        string `json:"path"`
	Line        int    `json:"line"`
	HelpURI     string `json:"helpUri,omitempty"`
}

type securitySARIF struct {
	Version string             `json:"version"`
	Runs    []securitySARIFRun `json:"runs"`
}

type securitySARIFRun struct {
	Tool struct {
		Driver     securitySARIFComponent   `json:"driver"`
		Extensions []securitySARIFComponent `json:"extensions"`
	} `json:"tool"`
	Results     []securitySARIFResult `json:"results"`
	Invocations []struct {
		ExecutionSuccessful *bool `json:"executionSuccessful"`
	} `json:"invocations"`
	Artifacts []struct {
		Location securitySARIFArtifact `json:"location"`
	} `json:"artifacts"`
}

type securitySARIFComponent struct {
	Name  string              `json:"name"`
	GUID  string              `json:"guid"`
	Rules []securitySARIFRule `json:"rules"`
}

type securitySARIFRuleReference struct {
	ID            string `json:"id"`
	Index         *int   `json:"index"`
	ToolComponent *struct {
		Index *int   `json:"index"`
		Name  string `json:"name"`
		GUID  string `json:"guid"`
	} `json:"toolComponent"`
}

type securitySARIFRule struct {
	ID                   string `json:"id"`
	HelpURI              string `json:"helpUri"`
	DefaultConfiguration struct {
		Level string `json:"level"`
	} `json:"defaultConfiguration"`
	Properties securitySARIFProperties `json:"properties"`
}

type securitySARIFProperties struct {
	SecuritySeverity json.RawMessage `json:"security-severity"`
}

type securitySARIFArtifact struct {
	URI   string `json:"uri"`
	Index *int   `json:"index"`
}

type securitySARIFResult struct {
	RuleID        string                      `json:"ruleId"`
	RuleIndex     *int                        `json:"ruleIndex"`
	Rule          *securitySARIFRuleReference `json:"rule"`
	Kind          string                      `json:"kind"`
	Level         string                      `json:"level"`
	BaselineState string                      `json:"baselineState"`
	Message       struct {
		Text     string `json:"text"`
		Markdown string `json:"markdown"`
	} `json:"message"`
	Suppressions []struct {
		Kind   string `json:"kind"`
		Status string `json:"status"`
	} `json:"suppressions"`
	Locations []struct {
		PhysicalLocation *struct {
			ArtifactLocation securitySARIFArtifact `json:"artifactLocation"`
			Region           struct {
				StartLine   int `json:"startLine"`
				StartColumn int `json:"startColumn"`
				Snippet     struct {
					Text string `json:"text"`
				} `json:"snippet"`
			} `json:"region"`
		} `json:"physicalLocation"`
		LogicalLocations []struct {
			FullyQualifiedName string `json:"fullyQualifiedName"`
		} `json:"logicalLocations"`
	} `json:"locations"`
	Fingerprints        map[string]string       `json:"fingerprints"`
	PartialFingerprints map[string]string       `json:"partialFingerprints"`
	Properties          securitySARIFProperties `json:"properties"`
}

type securityFindingCandidate struct {
	finding SecurityFinding
	column  int
	basis   string
	native  bool
}

// ParseSecurityFindings normalizes SARIF 2.1.0 without a GitHub API or any other
// effect. Empty results are a valid clean scan; missing results, failed scanner
// invocations and malformed consumed fields are failures. Some scanners,
// notably Scorecard, omit invocations; the caller must additionally verify the
// producer's successful retained-artifact receipt before treating this as work.
//
// Accepted suppressions and findings absent from the baseline do not create
// work. Quality notes remain actionable alongside security findings. Native
// scanner fingerprints are preferred. Without one, the fallback uses the rule,
// path and stable message/snippet, then an occurrence number among identical
// locations ordered by position. Moving lines does not duplicate every goal;
// adding/removing indistinguishable sites can change their occurrence numbers.
func ParseSecurityFindings(body []byte) ([]SecurityFinding, error) {
	if len(body) == 0 || len(body) > MaxSecurityReportBytes || !utf8.Valid(body) {
		return nil, errors.New("security SARIF is empty, oversized or not UTF-8")
	}
	var report securitySARIF
	if err := json.Unmarshal(body, &report); err != nil {
		// Decoder errors may quote scanner-controlled strings. Keep refusals
		// bounded and avoid echoing report content into logs or UI notices.
		return nil, errors.New("security SARIF is not a valid report object")
	}
	if report.Version != "2.1.0" || len(report.Runs) == 0 || len(report.Runs) > 32 {
		return nil, errors.New("security SARIF requires version 2.1.0 and 1 to 32 runs")
	}
	var candidates []securityFindingCandidate
	resultCount := 0
	for runIndex, run := range report.Runs {
		if !securityIdentifier(run.Tool.Driver.Name, 256) || run.Results == nil || len(run.Tool.Extensions) > 100 || len(run.Artifacts) > 100000 || len(run.Invocations) > 100 {
			return nil, fmt.Errorf("security SARIF run %d has missing or oversized analysis metadata", runIndex+1)
		}
		for _, invocation := range run.Invocations {
			if invocation.ExecutionSuccessful == nil || !*invocation.ExecutionSuccessful {
				return nil, fmt.Errorf("security SARIF run %d does not confirm successful analysis", runIndex+1)
			}
		}
		resultCount += len(run.Results)
		if resultCount > MaxSecurityFindings {
			return nil, errors.New("security SARIF exceeds the finding count limit")
		}
		rules, err := securityRuleCatalog(run)
		if err != nil {
			return nil, fmt.Errorf("security SARIF run %d: %w", runIndex+1, err)
		}
		for resultIndex, result := range run.Results {
			candidate, keep, err := normalizeSecurityResult(run, rules, result)
			if err != nil {
				return nil, fmt.Errorf("security SARIF run %d result %d: %w", runIndex+1, resultIndex+1, err)
			}
			if keep {
				candidates = append(candidates, candidate)
			}
		}
	}
	return fingerprintSecurityFindings(candidates), nil
}

func normalizeSecurityResult(run securitySARIFRun, rules map[string][]securitySARIFRule, result securitySARIFResult) (securityFindingCandidate, bool, error) {
	var candidate securityFindingCandidate
	bad := func(message string) (securityFindingCandidate, bool, error) {
		return candidate, false, errors.New(message)
	}
	if len(result.Locations) > 100 || len(result.Suppressions) > 100 {
		return bad("locations or suppressions exceed their limit")
	}
	suppressed := false
	for _, suppression := range result.Suppressions {
		if suppression.Kind != "inSource" && suppression.Kind != "external" {
			return bad("invalid suppression kind")
		}
		switch suppression.Status {
		case "", "accepted":
			suppressed = true
		case "underReview", "rejected":
		default:
			return bad("invalid suppression status")
		}
	}
	switch result.BaselineState {
	case "", "new", "updated", "unchanged", "absent":
	default:
		return bad("invalid baseline state")
	}
	switch result.Kind {
	case "", "fail", "review", "open", "pass", "notApplicable", "informational":
	default:
		return bad("invalid result kind")
	}
	if suppressed || result.BaselineState == "absent" || result.Kind == "pass" || result.Kind == "notApplicable" || result.Kind == "informational" {
		return candidate, false, nil
	}
	rule, ruleID, err := resolveSecurityRule(run, rules, result)
	if err != nil {
		return bad(err.Error())
	}
	if !securityIdentifier(ruleID, 512) {
		return bad("finding has no valid rule identity")
	}
	message := result.Message.Text
	if message == "" {
		message = result.Message.Markdown
	}
	if strings.TrimSpace(message) == "" || len(message) > 16384 {
		return bad("finding has no bounded message text")
	}
	severity, err := securitySeverity(result, rule)
	if err != nil {
		return bad(err.Error())
	}
	if severity == "none" {
		return candidate, false, nil
	}
	finding := SecurityFinding{Tool: run.Tool.Driver.Name, RuleID: ruleID, Severity: severity, Message: securityPlainText(message), HelpURI: securityHelpURI(rule.HelpURI)}
	snippet, logical := "", ""
	for index, location := range result.Locations {
		if len(location.LogicalLocations) > 100 {
			return bad("logical locations exceed their limit")
		}
		for _, item := range location.LogicalLocations {
			if len(item.FullyQualifiedName) > 2048 {
				return bad("logical location exceeds its limit")
			}
		}
		if index == 0 && len(location.LogicalLocations) > 0 {
			logical = securityPlainText(location.LogicalLocations[0].FullyQualifiedName)
		}
		if location.PhysicalLocation == nil {
			continue
		}
		physical := location.PhysicalLocation
		uri := physical.ArtifactLocation.URI
		if artifactIndex := physical.ArtifactLocation.Index; artifactIndex != nil {
			if *artifactIndex < 0 || *artifactIndex >= len(run.Artifacts) {
				return bad("artifact index does not name a declared artifact")
			}
			indexedURI := run.Artifacts[*artifactIndex].Location.URI
			if uri != "" && uri != indexedURI {
				return bad("artifact URI disagrees with its index")
			}
			uri = indexedURI
		}
		filePath, err := securityPath(uri)
		if err != nil {
			return bad(err.Error())
		}
		region := physical.Region
		if region.StartLine < 0 || region.StartLine > 100000000 || region.StartColumn < 0 || region.StartColumn > 10000000 || len(region.Snippet.Text) > 16384 {
			return bad("location coordinates or snippet exceed their limits")
		}
		if index == 0 {
			finding.Path, finding.Line, candidate.column = filePath, region.StartLine, region.StartColumn
			snippet = strings.Join(strings.Fields(securityPlainText(region.Snippet.Text)), " ")
		}
	}
	native, err := securityNativeFingerprint(result)
	if err != nil {
		return bad(err.Error())
	}
	candidate.finding = finding
	candidate.native = native != ""
	if candidate.native {
		candidate.basis = securityDigest("native", finding.Tool, finding.RuleID, finding.Path, native)
	} else {
		candidate.basis = securityDigest("fallback", finding.Tool, finding.RuleID, finding.Path, strings.Join(strings.Fields(finding.Message), " "), snippet, logical)
	}
	return candidate, true, nil
}

func securityRuleCatalog(run securitySARIFRun) (map[string][]securitySARIFRule, error) {
	rules := map[string][]securitySARIFRule{}
	count := 0
	for _, component := range append([]securitySARIFComponent{run.Tool.Driver}, run.Tool.Extensions...) {
		count += len(component.Rules)
		if !securityIdentifier(component.Name, 256) || count > 10000 {
			return nil, errors.New("tool components have missing or oversized rule metadata")
		}
		seen := map[string]bool{}
		for _, rule := range component.Rules {
			if !securityIdentifier(rule.ID, 512) || seen[rule.ID] {
				return nil, errors.New("tool component has invalid or duplicate rule identities")
			}
			seen[rule.ID] = true
			rules[rule.ID] = append(rules[rule.ID], rule)
		}
	}
	return rules, nil
}

// CodeQL may keep its query metadata in tool.extensions instead of driver.rules.
// Resolve the SARIF descriptor reference so those findings retain their security
// severity. An ambiguous or inconsistent reference is never guessed.
func resolveSecurityRule(run securitySARIFRun, rules map[string][]securitySARIFRule, result securitySARIFResult) (securitySARIFRule, string, error) {
	var empty securitySARIFRule
	bad := func(message string) (securitySARIFRule, string, error) { return empty, "", errors.New(message) }
	id, index, selected := result.RuleID, result.RuleIndex, false
	component := run.Tool.Driver
	if reference := result.Rule; reference != nil {
		if reference.ID != "" {
			if id != "" && id != reference.ID {
				return bad("rule identity disagrees with its reference")
			}
			id = reference.ID
		}
		if reference.Index != nil {
			if index != nil && *index != *reference.Index {
				return bad("rule index disagrees with its reference")
			}
			index = reference.Index
		}
		if selector := reference.ToolComponent; selector != nil {
			selected = true
			if result.RuleIndex != nil {
				return bad("legacy rule index cannot name a tool extension")
			}
			if selector.Index != nil {
				if *selector.Index < 0 || *selector.Index >= len(run.Tool.Extensions) {
					return bad("rule refers to an unknown tool extension")
				}
				component = run.Tool.Extensions[*selector.Index]
			} else {
				matches := 0
				for _, option := range append([]securitySARIFComponent{run.Tool.Driver}, run.Tool.Extensions...) {
					if (selector.Name != "" || selector.GUID != "") && (selector.Name == "" || option.Name == selector.Name) && (selector.GUID == "" || option.GUID == selector.GUID) {
						component = option
						matches++
					}
				}
				if matches != 1 {
					return bad("rule has an ambiguous or unknown tool component")
				}
			}
			if (selector.Name != "" && selector.Name != component.Name) || (selector.GUID != "" && selector.GUID != component.GUID) {
				return bad("rule tool component disagrees with its index")
			}
		}
	}
	if index != nil {
		if *index < 0 || *index >= len(component.Rules) {
			return bad("rule index does not name a declared rule")
		}
		rule := component.Rules[*index]
		if id != "" && id != rule.ID {
			return bad("rule identity disagrees with its index")
		}
		return rule, rule.ID, nil
	}
	if selected {
		for _, rule := range component.Rules {
			if rule.ID == id {
				return rule, id, nil
			}
		}
		return bad("rule identity is absent from its tool component")
	}
	if len(rules[id]) > 1 {
		return bad("rule identity is ambiguous without its tool component")
	}
	if matches := rules[id]; len(matches) == 1 {
		return matches[0], id, nil
	}
	return empty, id, nil
}

func securityIdentifier(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && strings.TrimSpace(value) == value && !strings.ContainsFunc(value, func(r rune) bool { return unicode.IsControl(r) || unicode.In(r, unicode.Cf) })
}

func securityPlainText(value string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cf) || (unicode.IsControl(r) && r != '\n' && r != '\t') {
			return ' '
		}
		return r
	}, value))
}

// Only repository-relative paths leave the parser. Absolute paths, authorities,
// traversal and non-file URLs must never become a goal's suggested source path.
// SARIF source-root base IDs are deliberately not followed: they are metadata
// supplied by the scanner, not authority to read elsewhere on a worker.
func securityPath(value string) (string, error) {
	if value == "" || len(value) > 4096 {
		return "", errors.New("physical location requires a bounded repository-relative URI")
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", errors.New("physical location is not a repository-relative URI")
	}
	decoded := u.Path
	if !securityIdentifier(decoded, 2048) || strings.ContainsAny(decoded, "\\:") || strings.HasPrefix(decoded, "/") {
		return "", errors.New("physical location is not a safe repository-relative path")
	}
	for _, segment := range strings.Split(decoded, "/") {
		if segment == ".." {
			return "", errors.New("physical location contains parent traversal")
		}
	}
	decoded = path.Clean(decoded)
	if decoded == "." {
		return "", errors.New("physical location does not name a file")
	}
	// Scorecard uses this literal for repository-level checks; retaining it as
	// a filename would send the remediation agent looking for a fictitious file.
	if decoded == "no file associated with this alert" {
		return "", nil
	}
	return decoded, nil
}

func securityHelpURI(value string) string {
	if !securityIdentifier(value, 2048) {
		return ""
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return ""
	}
	return value
}

func securitySeverity(result securitySARIFResult, rule securitySARIFRule) (string, error) {
	level := result.Level
	if level == "" {
		level = rule.DefaultConfiguration.Level
	}
	if level == "" {
		level = "warning"
	}
	if level != "none" && level != "note" && level != "warning" && level != "error" {
		return "", errors.New("finding has an unknown severity level")
	}
	raw := result.Properties.SecuritySeverity
	if len(raw) == 0 {
		raw = rule.Properties.SecuritySeverity
	}
	if len(raw) == 0 {
		return level, nil
	}
	value := string(raw)
	if len(value) > 32 {
		return "", errors.New("finding has an invalid security severity")
	}
	if strings.HasPrefix(value, "\"") {
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", errors.New("finding has an invalid security severity")
		}
	}
	score, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(score) || math.IsInf(score, 0) || score < 0 || score > 10 {
		return "", errors.New("finding has an invalid security severity")
	}
	switch {
	case score >= 9:
		return "critical", nil
	case score >= 7:
		return "high", nil
	case score >= 4:
		return "medium", nil
	case score > 0:
		return "low", nil
	default:
		return level, nil
	}
}

func securityNativeFingerprint(result securitySARIFResult) (string, error) {
	for _, values := range []map[string]string{result.Fingerprints, result.PartialFingerprints} {
		if len(values) > 32 {
			return "", errors.New("finding has too many fingerprint fields")
		}
		for key, value := range values {
			if !securityIdentifier(key, 256) || (value != "" && !securityIdentifier(value, 1024)) {
				return "", errors.New("finding has an invalid fingerprint field")
			}
		}
	}
	for _, values := range []map[string]string{result.Fingerprints, result.PartialFingerprints} {
		// CodeQL's line hash is stable when surrounding lines move and carries
		// an occurrence suffix for identical source lines. Scorecard emits an
		// empty line hash for some checks; absence selects the fallback identity.
		if value := values["primaryLocationLineHash"]; value != "" {
			return "primaryLocationLineHash=" + value, nil
		}
		keys := make([]string, 0, len(values))
		for key, value := range values {
			if value != "" {
				keys = append(keys, key)
			}
		}
		if len(keys) != 0 {
			slices.Sort(keys)
			return keys[0] + "=" + values[keys[0]], nil
		}
	}
	return "", nil
}

func securityDigest(parts ...string) string {
	// JSON's lengths and escaping make boundaries unambiguous even when a
	// scanner puts separators in a message, path or fingerprint.
	encoded, _ := json.Marshal(parts)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func fingerprintSecurityFindings(candidates []securityFindingCandidate) []SecurityFinding {
	slices.SortFunc(candidates, func(a, b securityFindingCandidate) int {
		if comparison := strings.Compare(a.basis, b.basis); comparison != 0 {
			return comparison
		}
		// Multiple report runs can repeat one native identity with different
		// detail. Preserve the most severe observation, independent of order.
		severity := securitySeverityOrder(a.finding.Severity) - securitySeverityOrder(b.finding.Severity)
		if a.native && severity != 0 {
			return severity
		}
		if a.finding.Line != b.finding.Line {
			return a.finding.Line - b.finding.Line
		}
		if a.column != b.column {
			return a.column - b.column
		}
		if severity != 0 {
			return severity
		}
		return strings.Compare(a.finding.Message, b.finding.Message)
	})
	findings := make([]SecurityFinding, 0, len(candidates))
	seen := map[string]bool{}
	occurrences := map[string]int{}
	for _, candidate := range candidates {
		fingerprint := candidate.basis
		if !candidate.native {
			// Exact repeated locations in one or multiple SARIF runs are one
			// finding; distinct locations with identical evidence keep separate
			// occurrence identities without hashing their moving line numbers.
			exact := securityDigest(candidate.basis, strconv.Itoa(candidate.finding.Line), strconv.Itoa(candidate.column))
			if seen[exact] {
				continue
			}
			seen[exact] = true
			occurrences[candidate.basis]++
			fingerprint = securityDigest(candidate.basis, strconv.Itoa(occurrences[candidate.basis]))
		}
		if seen[fingerprint] {
			continue
		}
		seen[fingerprint] = true
		candidate.finding.Fingerprint = fingerprint
		findings = append(findings, candidate.finding)
	}
	return findings
}

func securitySeverityOrder(severity string) int {
	switch severity {
	case "critical":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	case "low":
		return 3
	case "error":
		return 4
	case "warning":
		return 5
	default:
		return 6
	}
}
