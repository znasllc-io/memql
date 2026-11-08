package pipelines

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func securityReportForTest(results ...map[string]any) map[string]any {
	return map[string]any{
		"version": "2.1.0",
		"runs": []any{map[string]any{
			"tool": map[string]any{"driver": map[string]any{
				"name": "CodeQL",
				"rules": []any{map[string]any{
					"id": "go/sql-injection", "helpUri": "https://codeql.github.com/codeql-query-help/go/go-sql-injection/",
					"properties": map[string]any{"security-severity": "9.1"},
				}},
			}},
			"invocations": []any{map[string]any{"executionSuccessful": true}},
			"results":     results,
		}},
	}
}

func securityResultForTest(line int) map[string]any {
	return map[string]any{
		"ruleId": "go/sql-injection", "level": "warning", "message": map[string]any{"text": "Untrusted input reaches a query."},
		"locations": []any{map[string]any{"physicalLocation": map[string]any{
			"artifactLocation": map[string]any{"uri": "component/example/query.go", "uriBaseId": "%SRCROOT%"},
			"region":           map[string]any{"startLine": line, "startColumn": 7},
		}}},
	}
}

func securityRunForTest(report map[string]any) map[string]any {
	return report["runs"].([]any)[0].(map[string]any)
}

func securityPhysicalForTest(result map[string]any) map[string]any {
	return result["locations"].([]any)[0].(map[string]any)["physicalLocation"].(map[string]any)
}

func parseSecurityReportForTest(t *testing.T, report map[string]any) ([]SecurityFinding, error) {
	t.Helper()
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	return ParseSecurityFindings(body)
}

func TestSecurityFindingsPreserveActionableEvidence(t *testing.T) {
	result := securityResultForTest(42)
	result["partialFingerprints"] = map[string]any{"primaryLocationLineHash": "abc:1"}
	got, err := parseSecurityReportForTest(t, securityReportForTest(result))
	if err != nil || len(got) != 1 {
		t.Fatalf("findings = %#v, error = %v", got, err)
	}
	finding := got[0]
	if finding.Tool != "CodeQL" || finding.RuleID != "go/sql-injection" || finding.Severity != "critical" || finding.Path != "component/example/query.go" || finding.Line != 42 || finding.Message != "Untrusted input reaches a query." || finding.HelpURI != "https://codeql.github.com/codeql-query-help/go/go-sql-injection/" || len(finding.Fingerprint) != 64 {
		t.Fatalf("unexpected normalized evidence: %#v", finding)
	}
}

func TestSecurityFindingNativeIdentitySurvivesLineAndMessageChanges(t *testing.T) {
	before := securityResultForTest(42)
	after := securityResultForTest(1042)
	before["partialFingerprints"] = map[string]any{"primaryLocationLineHash": "abc:1"}
	after["partialFingerprints"] = map[string]any{"primaryLocationLineHash": "abc:1", "newScannerMetadata": "ignored"}
	after["message"] = map[string]any{"text": "The scanner improved its explanation."}
	a, err := parseSecurityReportForTest(t, securityReportForTest(before))
	if err != nil {
		t.Fatal(err)
	}
	b, err := parseSecurityReportForTest(t, securityReportForTest(after))
	if err != nil || len(b) != 1 || a[0].Fingerprint != b[0].Fingerprint {
		t.Fatalf("moving source produced another identity: before=%#v, after=%#v, error=%v", a, b, err)
	}
	duplicate, err := parseSecurityReportForTest(t, securityReportForTest(before, after))
	if err != nil || len(duplicate) != 1 {
		t.Fatalf("native duplicate creates multiple goals: %#v, %v", duplicate, err)
	}
}

func TestSecurityFindingFallbackSurvivesLineMovementWithoutMergingDistinctSites(t *testing.T) {
	before, err := parseSecurityReportForTest(t, securityReportForTest(securityResultForTest(20), securityResultForTest(10), securityResultForTest(10)))
	if err != nil || len(before) != 2 {
		t.Fatalf("distinct source locations were lost or duplicated: %#v, %v", before, err)
	}
	after, err := parseSecurityReportForTest(t, securityReportForTest(securityResultForTest(110), securityResultForTest(120)))
	if err != nil || len(after) != 2 {
		t.Fatalf("moved sites: %#v, %v", after, err)
	}
	for index := range before {
		if before[index].Fingerprint != after[index].Fingerprint {
			t.Fatalf("line movement changed identity: %#v -> %#v", before, after)
		}
	}
}

func TestSecurityFindingIdentitySeparatesToolRuleAndFile(t *testing.T) {
	base := securityResultForTest(42)
	base["fingerprints"] = map[string]any{"issue/v1": "same"}
	original, err := parseSecurityReportForTest(t, securityReportForTest(base))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"tool", "rule", "file"} {
		t.Run(field, func(t *testing.T) {
			result := securityResultForTest(42)
			result["fingerprints"] = map[string]any{"issue/v1": "same"}
			report := securityReportForTest(result)
			switch field {
			case "tool":
				securityRunForTest(report)["tool"].(map[string]any)["driver"].(map[string]any)["name"] = "Another scanner"
			case "rule":
				result["ruleId"] = "another-rule"
			case "file":
				securityPhysicalForTest(result)["artifactLocation"] = map[string]any{"uri": "another.go"}
			}
			got, err := parseSecurityReportForTest(t, report)
			if err != nil || len(got) != 1 || got[0].Fingerprint == original[0].Fingerprint {
				t.Fatalf("different %s reused another finding: %#v, %v", field, got, err)
			}
		})
	}
}

func TestSecurityFindingsHonorTriageWithoutDroppingQualityNotes(t *testing.T) {
	for _, test := range []struct {
		name   string
		update map[string]any
		keep   bool
	}{
		{"accepted", map[string]any{"suppressions": []any{map[string]any{"kind": "external", "status": "accepted"}}}, false},
		{"source suppression", map[string]any{"suppressions": []any{map[string]any{"kind": "inSource"}}}, false},
		{"rejected suppression", map[string]any{"suppressions": []any{map[string]any{"kind": "external", "status": "rejected"}}}, true},
		{"review suppression", map[string]any{"suppressions": []any{map[string]any{"kind": "external", "status": "underReview"}}}, true},
		{"baseline absent", map[string]any{"baselineState": "absent"}, false},
		{"baseline unchanged", map[string]any{"baselineState": "unchanged"}, true},
		{"pass", map[string]any{"kind": "pass"}, false},
		{"not applicable", map[string]any{"kind": "notApplicable"}, false},
		{"informational", map[string]any{"kind": "informational"}, false},
		{"quality note", map[string]any{"ruleId": "py/unused-import", "level": "note"}, true},
		{"no level", map[string]any{"ruleId": "scanner/no-level", "level": "none"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := securityResultForTest(1)
			for key, value := range test.update {
				result[key] = value
			}
			got, err := parseSecurityReportForTest(t, securityReportForTest(result))
			if err != nil || (len(got) == 1) != test.keep {
				t.Fatalf("findings = %#v, error = %v; keep = %v", got, err, test.keep)
			}
			if test.name == "quality note" && got[0].Severity != "note" {
				t.Fatalf("quality note gained a security rating: %#v", got)
			}
		})
	}
}

func TestSecurityFindingsPermitVerifiedCleanReportAndScorecardWithoutInvocations(t *testing.T) {
	report := securityReportForTest()
	securityRunForTest(report)["results"] = []any{}
	findings, err := parseSecurityReportForTest(t, report)
	if err != nil || len(findings) != 0 {
		t.Fatalf("valid clean scan = %#v, %v", findings, err)
	}
	result := securityResultForTest(1)
	result["ruleId"] = "CITestsID"
	securityPhysicalForTest(result)["artifactLocation"] = map[string]any{"uri": "no%20file%20associated%20with%20this%20alert"}
	report = securityReportForTest(result)
	delete(securityRunForTest(report), "invocations")
	securityRunForTest(report)["tool"] = map[string]any{"driver": map[string]any{"name": "Scorecard"}}
	findings, err = parseSecurityReportForTest(t, report)
	if err != nil || len(findings) != 1 || findings[0].Path != "" || findings[0].Tool != "Scorecard" {
		t.Fatalf("repository-level Scorecard finding = %#v, %v", findings, err)
	}
}

func TestSecurityFindingsResolveRuleAndArtifactIndices(t *testing.T) {
	result := securityResultForTest(9)
	delete(result, "ruleId")
	result["ruleIndex"] = 0
	securityPhysicalForTest(result)["artifactLocation"] = map[string]any{"index": 0}
	report := securityReportForTest(result)
	securityRunForTest(report)["artifacts"] = []any{map[string]any{"location": map[string]any{"uri": "./source/a%20file.go"}}}
	got, err := parseSecurityReportForTest(t, report)
	if err != nil || len(got) != 1 || got[0].RuleID != "go/sql-injection" || got[0].Path != "source/a file.go" {
		t.Fatalf("indexed evidence = %#v, %v", got, err)
	}
}

func TestSecurityFindingsResolveCodeQLQueryPackSeverity(t *testing.T) {
	// The retained Go analysis stores all rule metadata in an extension. JS
	// and Python may put it on the driver; both describe the same scanner.
	result := securityResultForTest(9)
	result["rule"] = map[string]any{"id": "go/sql-injection", "index": 0, "toolComponent": map[string]any{"index": 0}}
	report := securityReportForTest(result)
	tool := securityRunForTest(report)["tool"].(map[string]any)
	driver := tool["driver"].(map[string]any)
	tool["extensions"] = []any{map[string]any{"name": "codeql/go-queries", "rules": driver["rules"]}}
	delete(driver, "rules")
	got, err := parseSecurityReportForTest(t, report)
	if err != nil || len(got) != 1 || got[0].Severity != "critical" || got[0].HelpURI == "" {
		t.Fatalf("query-pack security metadata lost: %#v, %v", got, err)
	}
	result["rule"].(map[string]any)["toolComponent"] = map[string]any{"name": "codeql/go-queries"}
	byName, err := parseSecurityReportForTest(t, report)
	if err != nil || !reflect.DeepEqual(byName, got) {
		t.Fatalf("named query-pack reference differs: %#v, %v", byName, err)
	}
	result["rule"].(map[string]any)["toolComponent"] = map[string]any{"index": 1}
	if findings, err := parseSecurityReportForTest(t, report); err == nil || findings != nil {
		t.Fatalf("unknown query-pack reference accepted: %#v, %v", findings, err)
	}
}

func TestSecurityFindingsTreatEmptyScorecardFingerprintAsUnavailable(t *testing.T) {
	result := securityResultForTest(1)
	expected, err := parseSecurityReportForTest(t, securityReportForTest(result))
	if err != nil {
		t.Fatal(err)
	}
	result["partialFingerprints"] = map[string]any{"primaryLocationLineHash": ""}
	got, err := parseSecurityReportForTest(t, securityReportForTest(result))
	if err != nil || !reflect.DeepEqual(got, expected) {
		t.Fatalf("empty Scorecard hash failed or changed fallback identity: %#v, %v", got, err)
	}
}

func TestSecurityFindingsRejectIncompleteOrFailedScans(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"version", func(r map[string]any) { r["version"] = "2.0.0" }},
		{"no runs", func(r map[string]any) { r["runs"] = []any{} }},
		{"null run", func(r map[string]any) { r["runs"] = []any{nil} }},
		{"missing tool", func(r map[string]any) { delete(securityRunForTest(r), "tool") }},
		{"missing results", func(r map[string]any) { delete(securityRunForTest(r), "results") }},
		{"null results", func(r map[string]any) { securityRunForTest(r)["results"] = nil }},
		{"failed invocation", func(r map[string]any) {
			securityRunForTest(r)["invocations"] = []any{map[string]any{"executionSuccessful": true}, map[string]any{"executionSuccessful": false}}
		}},
		{"missing invocation outcome", func(r map[string]any) {
			securityRunForTest(r)["invocations"] = []any{map[string]any{}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			report := securityReportForTest(securityResultForTest(1))
			test.change(report)
			findings, err := parseSecurityReportForTest(t, report)
			if err == nil || findings != nil {
				t.Fatalf("broken scan supplied usable work: %#v, %v", findings, err)
			}
		})
	}
	for _, body := range []string{"", "null", "[]", "{}", `{"version":"2.1.0","runs":[]} {}`, "\xff"} {
		if got, err := ParseSecurityFindings([]byte(body)); err == nil || got != nil {
			t.Fatalf("invalid report %q accepted: %#v, %v", body, got, err)
		}
	}
}

func TestSecurityFindingsRefuseHostAndTraversalPaths(t *testing.T) {
	for _, uri := range []string{"../secret", "a/../secret", "%2e%2e/secret", "a/%2E%2E/secret", "/etc/passwd", "file:///workspace/query.go", "https://example.com/x", "//host/file", "C:/windows/file", "a\\b", "x%00.go", "x%0a.go", "foo.go?x=y", "foo.go#fragment", ".", "", "%zz"} {
		t.Run(uri, func(t *testing.T) {
			result := securityResultForTest(1)
			securityPhysicalForTest(result)["artifactLocation"] = map[string]any{"uri": uri}
			if findings, err := parseSecurityReportForTest(t, securityReportForTest(result)); err == nil || findings != nil {
				t.Fatalf("unsafe path became source evidence: %#v, %v", findings, err)
			}
		})
	}
}

func TestSecurityFindingsRejectMalformedAndOverLimitEvidenceWithoutPartialGoals(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"missing rule", func(r map[string]any) { delete(r, "ruleId") }},
		{"unknown rule index", func(r map[string]any) { r["ruleIndex"] = 2 }},
		{"mismatched rule index", func(r map[string]any) { r["ruleId"], r["ruleIndex"] = "other", 0 }},
		{"missing message", func(r map[string]any) { delete(r, "message") }},
		{"large message", func(r map[string]any) { r["message"] = map[string]any{"text": strings.Repeat("m", 16385)} }},
		{"unknown level", func(r map[string]any) { r["level"] = "urgent" }},
		{"invalid score", func(r map[string]any) { r["properties"] = map[string]any{"security-severity": "NaN"} }},
		{"oversized score", func(r map[string]any) { r["properties"] = map[string]any{"security-severity": 11} }},
		{"negative line", func(r map[string]any) { securityPhysicalForTest(r)["region"] = map[string]any{"startLine": -1} }},
		{"invalid artifact index", func(r map[string]any) { securityPhysicalForTest(r)["artifactLocation"] = map[string]any{"index": 0} }},
		{"invalid baseline", func(r map[string]any) { r["baselineState"] = "clean" }},
		{"invalid suppression", func(r map[string]any) {
			r["suppressions"] = []any{map[string]any{"kind": "external", "status": "ignore"}}
		}},
		{"invalid fingerprint", func(r map[string]any) { r["partialFingerprints"] = map[string]any{"hash": "a\x00b"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := securityResultForTest(1)
			test.change(result)
			got, err := parseSecurityReportForTest(t, securityReportForTest(securityResultForTest(10), result))
			if err == nil || got != nil || len(err.Error()) > 200 {
				t.Fatalf("malformed report supplied partial work or unbounded error: %#v, %v", got, err)
			}
		})
	}
	results := make([]map[string]any, MaxSecurityFindings+1)
	for index := range results {
		results[index] = securityResultForTest(index + 1)
	}
	if got, err := parseSecurityReportForTest(t, securityReportForTest(results...)); err == nil || got != nil {
		t.Fatalf("finding overflow supplied truncated work: %d, %v", len(got), err)
	}
	if got, err := ParseSecurityFindings([]byte(strings.Repeat(" ", MaxSecurityReportBytes+1))); err == nil || got != nil {
		t.Fatalf("oversized report accepted: %#v, %v", got, err)
	}
}

func TestSecurityFindingsKeepUntrustedContentAsPlainEvidence(t *testing.T) {
	result := securityResultForTest(1)
	result["message"] = map[string]any{"text": "  Evidence\x00\u202e\n$(do-not-execute) <script>example</script>  "}
	report := securityReportForTest(result)
	rule := securityRunForTest(report)["tool"].(map[string]any)["driver"].(map[string]any)["rules"].([]any)[0].(map[string]any)
	rule["helpUri"] = "javascript:alert(1)"
	got, err := parseSecurityReportForTest(t, report)
	if err != nil || len(got) != 1 || got[0].Message != "Evidence  \n$(do-not-execute) <script>example</script>" || got[0].HelpURI != "" {
		t.Fatalf("untrusted text/URL normalization = %#v, %v", got, err)
	}
	// JSON encoding is the consumer's safe boundary. The parser does not turn
	// arbitrary prose into commands, HTML, paths or an instruction template.
	encoded, err := json.Marshal(got[0])
	if err != nil || strings.Contains(string(encoded), "<script>") {
		t.Fatalf("unsafe serialized evidence: %s, %v", encoded, err)
	}
}

func TestSecurityFindingsHaveDeterministicFingerprintOrder(t *testing.T) {
	first, second := securityResultForTest(20), securityResultForTest(10)
	second["ruleId"] = "go/another-rule"
	a, err := parseSecurityReportForTest(t, securityReportForTest(first, second))
	if err != nil {
		t.Fatal(err)
	}
	b, err := parseSecurityReportForTest(t, securityReportForTest(second, first))
	if err != nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("report order changes normalized evidence: %#v, %#v, %v", a, b, err)
	}
}

func TestSecurityFindingsRetainWorstSeverityOfDuplicateNativeEvidence(t *testing.T) {
	low, high := securityResultForTest(1), securityResultForTest(2)
	low["partialFingerprints"] = map[string]any{"primaryLocationLineHash": "same"}
	high["partialFingerprints"] = map[string]any{"primaryLocationLineHash": "same"}
	low["properties"] = map[string]any{"security-severity": "1.0"}
	a, err := parseSecurityReportForTest(t, securityReportForTest(low, high))
	if err != nil || len(a) != 1 || a[0].Severity != "critical" {
		t.Fatalf("lower severity hid a critical duplicate: %#v, %v", a, err)
	}
	b, err := parseSecurityReportForTest(t, securityReportForTest(high, low))
	if err != nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("duplicate ordering changed severity: %#v, %#v, %v", a, b, err)
	}
}
