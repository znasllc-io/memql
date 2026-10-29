package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/auth"
	"github.com/znasllc-io/memql/cockpit/internal/config"
	"github.com/znasllc-io/memql/cockpit/internal/crash"
	client "github.com/znasllc-io/memql/sdk/go/client"
)

const canonicalPipeline = "468d37ede87ac969d39c76ffdb678bf8"
const retiredPipeline = "39893d6bd100ff4ce1470beb89c0120c"

type options struct {
	op, profile, commit, cluster, store, tag, run, candidate, approval, target, artifact, configuration string
	output, rerunMode                                                                                   string
	execute                                                                                             bool
	timeout                                                                                             time.Duration
}

func parse() options {
	var o options
	flag.StringVar(&o.op, "operation", "inspect", "inspect, runs, run, steps, work-run, log, profiles, candidate, targets, publications, drafts; rerun-preflight; planned writes: rerun, run-release, assemble, approve, create-draft, publish, promote, seal, disconnect-old")
	flag.StringVar(&o.profile, "profile", "cockpit", "fixed canonical release profile: cockpit or engine")
	flag.StringVar(&o.commit, "commit", "", "exact reviewed 40-character source commit for candidate writes")
	flag.StringVar(&o.cluster, "cluster", "memql-znas-io", "existing canonical production cluster")
	flag.StringVar(&o.store, "credential-store", "", "existing credential store: auto (empty), file, or keyring")
	flag.StringVar(&o.tag, "tag", "", "exact component tag")
	flag.StringVar(&o.run, "run", "", "exact native pipeline/work run identifier")
	flag.StringVar(&o.rerunMode, "rerun-mode", rerunWhole, "rerun mode: whole or failed-only (reviewed fixed source IDs only)")
	flag.StringVar(&o.candidate, "candidate", "", "exact candidate identifier")
	flag.StringVar(&o.approval, "approval", "", "durable native approval identifier")
	flag.StringVar(&o.target, "target", "", "exact configured component target")
	flag.StringVar(&o.artifact, "artifact", "", "exact configured component artifact")
	flag.StringVar(&o.configuration, "configuration", "", "fresh component profile configuration digest")
	flag.StringVar(&o.output, "output", "", "new local directory for candidate-assets; no existing paths are overwritten")
	flag.BoolVar(&o.execute, "execute", false, "perform the selected write; otherwise return generated call without connecting")
	flag.DurationVar(&o.timeout, "timeout", 2*time.Minute, "bounded request lifetime; no automatic write retry")
	flag.Parse()
	return o
}

// selectCall uses generated SDK builders only. There is no arbitrary query,
// mutation, SQL, credential, destination, or repository override.
func selectCall(o options) (name, call string, write bool, err error) {
	if flag.NArg() != 0 {
		return "", "", false, errors.New("positional arguments are refused")
	}
	if o.cluster != "memql-znas-io" {
		return "", "", false, errors.New("only the existing production cluster is admitted")
	}
	if o.profile != "cockpit" && o.profile != "engine" {
		return "", "", false, errors.New("only cockpit and engine profiles are admitted")
	}
	if o.timeout < time.Second || o.timeout > 35*time.Minute {
		return "", "", false, errors.New("timeout must be 1s through 35m")
	}
	require := func(value, label string) error {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", label)
		}
		return nil
	}
	switch o.op {
	case "rerun", "rerun-preflight":
		if _, err = selectedRerun(o); err != nil {
			return
		}
		if o.op == "rerun-preflight" {
			return "rerun-preflight", "", false, nil
		}
		return "pipelinesRerun", rerunCall(o), true, nil
	case "candidate-assets":
		if o.profile != "cockpit" || !validTag(o.profile, o.tag) || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(o.commit) || !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(o.candidate) || o.output == "" {
			return "", "", false, errors.New("candidate-assets requires cockpit profile, exact candidate digest, tag, commit and new output directory")
		}
		return "candidate-assets", "", false, nil
	case "inspect":
		return "inspect", "", false, nil
	case "status":
		return "pipelinesStatus", client.PipelinesStatusBuild(client.PipelinesStatusArgs{}), false, nil
	case "profiles":
		return "releaseCandidateProfiles", client.ReleaseCandidateProfilesBuild(client.ReleaseCandidateProfilesArgs{}), false, nil
	case "runs":
		return "pipelineRunsForOwner", client.PipelineRunsForOwnerBuild(client.PipelineRunsForOwnerArgs{PipelineId: canonicalPipeline}), false, nil
	case "log":
		if err = require(o.run, "work run"); err != nil {
			return
		}
		if err = require(o.target, "exact step key"); err != nil {
			return
		}
		return "log", "", false, nil
	case "steps", "work-run":
		if err = require(o.run, "work run"); err != nil {
			return
		}
		if o.op == "steps" {
			return "workStepsForOwnerRun", client.WorkStepsForOwnerRunBuild(client.WorkStepsForOwnerRunArgs{RunId: o.run}), false, nil
		}
		return "workRunForOwner", client.WorkRunForOwnerBuild(client.WorkRunForOwnerArgs{RunId: o.run}), false, nil
	case "run":
		if err = require(o.run, "run"); err != nil {
			return
		}
		return "pipelineRunForOwner", client.PipelineRunForOwnerBuild(client.PipelineRunForOwnerArgs{RunId: o.run}), false, nil
	case "candidate", "targets", "publications", "drafts", "approve", "create-draft", "publish", "promote", "seal":
		if err = require(o.candidate, "candidate"); err != nil {
			return
		}
	case "run-release":
		if !validTag(o.profile, o.tag) {
			return "", "", false, errors.New("an exact stable tag matching the selected profile is required")
		}
		return "pipelinesRunRelease", client.PipelinesRunReleaseBuild(client.PipelinesRunReleaseArgs{PipelineId: canonicalPipeline, Tag: o.tag}), true, nil
	case "assemble":
		if err = require(o.run, "run"); err != nil {
			return
		}
		if err = require(o.configuration, "configuration"); err != nil {
			return
		}
		return "releaseAssembleProfileCandidate", client.ReleaseAssembleProfileCandidateBuild(client.ReleaseAssembleProfileCandidateArgs{ProfileName: o.profile, Runs: map[string]any{"release": o.run}, ExpectedConfiguration: o.configuration}), true, nil
	case "disconnect-old":
		return "pipelinesDisconnect", client.PipelinesDisconnectBuild(client.PipelinesDisconnectArgs{PipelineId: retiredPipeline}), true, nil
	default:
		return "", "", false, errors.New("unknown operation")
	}
	if o.op == "approve" || o.op == "create-draft" || o.op == "publish" || o.op == "promote" || o.op == "seal" {
		if !validTag(o.profile, o.tag) || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(o.commit) {
			return "", "", false, errors.New("candidate writes require exact matching --tag and reviewed --commit")
		}
	}
	switch o.op {
	case "candidate":
		return "releaseGetCandidate", client.ReleaseGetCandidateBuild(client.ReleaseGetCandidateArgs{CandidateId: o.candidate}), false, nil
	case "targets":
		return "releaseCandidateTargets", client.ReleaseCandidateTargetsBuild(client.ReleaseCandidateTargetsArgs{CandidateId: o.candidate}), false, nil
	case "publications":
		return "releaseCandidatePublications", client.ReleaseCandidatePublicationsBuild(client.ReleaseCandidatePublicationsArgs{CandidateId: o.candidate}), false, nil
	case "drafts":
		return "releaseCandidateDrafts", client.ReleaseCandidateDraftsBuild(client.ReleaseCandidateDraftsArgs{CandidateId: o.candidate}), false, nil
	case "approve":
		return "releaseApproveCandidate", client.ReleaseApproveCandidateBuild(client.ReleaseApproveCandidateArgs{CandidateId: o.candidate}), true, nil
	}
	if err = require(o.approval, "approval"); err != nil {
		return
	}
	if o.op == "seal" {
		return "releaseSealPublishedCandidate", client.ReleaseSealPublishedCandidateBuild(client.ReleaseSealPublishedCandidateArgs{CandidateId: o.candidate, ApprovalId: o.approval}), true, nil
	}
	if err = require(o.target, "target"); err != nil {
		return
	}
	expected, ok := fixedTargets[o.target]
	if !ok || expected.Component != o.profile {
		return "", "", false, errors.New("target is not in the selected fixed profile")
	}
	if (o.op == "create-draft" || o.op == "promote") && expected.Kind != "file" {
		return "", "", false, errors.New("draft operations require a GitHub file target")
	}
	switch o.op {
	case "create-draft":
		return "releaseCreateCandidateDraft", client.ReleaseCreateCandidateDraftBuild(client.ReleaseCreateCandidateDraftArgs{CandidateId: o.candidate, ApprovalId: o.approval, TargetId: o.target}), true, nil
	case "promote":
		return "releasePromoteCandidateDraft", client.ReleasePromoteCandidateDraftBuild(client.ReleasePromoteCandidateDraftArgs{CandidateId: o.candidate, ApprovalId: o.approval, TargetId: o.target}), true, nil
	case "publish":
		if err = require(o.artifact, "artifact"); err != nil {
			return
		}
		if o.artifact != expected.Artifact {
			return "", "", false, errors.New("artifact does not match the fixed target")
		}
		return "releasePublishCandidate", client.ReleasePublishCandidateBuild(client.ReleasePublishCandidateArgs{CandidateId: o.candidate, ApprovalId: o.approval, TargetId: o.target, Component: o.profile, Artifact: o.artifact}), true, nil
	}
	return "", "", false, errors.New("invalid operation")
}

func sanitize(v any, token string) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, v := range x {
			low := strings.ToLower(k)
			if strings.Contains(low, "token") || strings.Contains(low, "password") || strings.Contains(low, "secret") {
				out[k] = "<redacted>"
			} else {
				out[k] = sanitize(v, token)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = sanitize(v, token)
		}
		return out
	case string:
		if token != "" {
			x = strings.ReplaceAll(x, token, "<redacted>")
		}
		return crash.SanitizeForCrashLog(x)
	default:
		return v
	}
}

func perform(ctx context.Context, o options, name, call string, write bool) (map[string]any, error) {
	rerunStage(o, "load-existing-cluster")
	clusters, err := config.LoadClusters()
	if err != nil {
		return nil, errors.New("cannot read existing cluster configuration")
	}
	cluster, ok := clusters.Get(o.cluster)
	if !ok {
		return nil, errors.New("existing production cluster configuration is missing")
	}
	// Legacy registry entries used a bare :443 endpoint. Make its intended TLS
	// transport explicit and confine credentials to the known production service.
	if cluster.Endpoint == "api.memql.znas.io:443" {
		cluster.Endpoint = "https://api.memql.znas.io:443"
	}
	if cluster.Endpoint != "https://api.memql.znas.io" && cluster.Endpoint != "https://api.memql.znas.io:443" {
		return nil, errors.New("unexpected production endpoint; refused")
	}
	if cluster.Issuer != "https://identity.memql.znas.io" {
		return nil, errors.New("unexpected production identity issuer; refused")
	}
	rerunStage(o, "resolve-credential-store")
	store, err := config.Resolve(config.ResolveOptions{Override: o.store})
	if err != nil {
		return nil, errors.New("cannot initialize existing credential store")
	}
	config.SetActiveStore(store)
	rerunStage(o, "resolve-existing-session")
	token, err := auth.EnsureValidTokenNonInteractive(ctx, cluster, nil)
	if err != nil {
		// Only a stable category leaves this process. In particular, a refresh
		// transport failure must not be reported as a revoked login.
		category := "session_resolution_failed"
		var networkError net.Error
		switch {
		case errors.Is(err, auth.ErrLoginRequired):
			category = "login_required"
		case errors.Is(err, context.DeadlineExceeded):
			category = "refresh_deadline"
		case errors.As(err, &networkError):
			category = "refresh_network_failure"
		case strings.HasPrefix(err.Error(), "load cached token:"):
			category = "credential_store_read_failed"
		case strings.HasPrefix(err.Error(), "refresh access token:"):
			category = "refresh_service_failure"
		}
		return nil, fmt.Errorf("existing production session unavailable (%s)", category)
	}
	rerunStage(o, "connect-sdk")
	conn, err := client.Connect(ctx, client.ConnectConfig{Endpoint: cluster.Endpoint, Token: token, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		return nil, fmt.Errorf("production connection failed: %s", sanitize(err.Error(), token))
	}
	defer conn.Close()
	qc := client.NewQueryClient(conn.Dispatcher())
	rerunStage(o, "read-current-owner")
	access, err := qc.GetMyAccess(ctx)
	if err != nil {
		return nil, fmt.Errorf("owner access read failed: %s", sanitize(err.Error(), token))
	}
	if access == nil || access.Role != client.RoleOwner {
		return nil, errors.New("current session is not the production owner")
	}
	output := map[string]any{"operation": o.op, "profile": o.profile, "engineVersion": conn.EngineVersion, "engineCommit": conn.EngineCommit, "role": access.Role, "credentialStore": store.Name(), "write": write}
	if o.op == "candidate-assets" {
		files, e := downloadCandidateAssets(ctx, qc, token, o)
		if e != nil {
			return nil, fmt.Errorf("candidate asset review refused: %s", sanitize(e.Error(), token))
		}
		output["files"] = files
		output["remoteWrites"] = false
		return output, nil
	}
	if o.op == "inspect" {
		p, err := qc.ReleaseCandidateProfiles(ctx, client.ReleaseCandidateProfilesArgs{})
		if err != nil {
			return nil, fmt.Errorf("profile read failed: %s", sanitize(err.Error(), token))
		}
		s, err := qc.PipelinesForOwner(ctx, client.PipelinesForOwnerArgs{})
		if err != nil {
			return nil, fmt.Errorf("pipeline read failed: %s", sanitize(err.Error(), token))
		}
		output["profiles"] = sanitize(p.Raw(), token)
		output["pipelines"] = sanitize(s.Raw(), token)
		return output, nil
	}
	if o.op == "log" {
		steps, e := qc.WorkStepsForOwnerRun(ctx, client.WorkStepsForOwnerRunArgs{RunId: o.run})
		if e != nil {
			return nil, errors.New("cannot read owner work steps")
		}
		body, e := json.Marshal(steps.Raw())
		if e != nil {
			return nil, e
		}
		var rows struct {
			Data []struct {
				Key       string `json:"key"`
				RunID     string `json:"runId"`
				LogFileID string `json:"logFileId"`
			} `json:"data"`
		}
		if e = json.Unmarshal(body, &rows); e != nil {
			return nil, e
		}
		file := ""
		for _, r := range rows.Data {
			if r.Key == o.target && r.RunID == o.run {
				if file != "" {
					return nil, errors.New("ambiguous exact work step")
				}
				file = r.LogFileID
			}
		}
		if file == "" {
			return nil, errors.New("exact work step has no retained log yet")
		}
		result, e := qc.LibraryReadFile(ctx, client.LibraryReadFileArgs{FileId: file})
		if e != nil {
			return nil, fmt.Errorf("retained step log read failed: %s", sanitize(e.Error(), token))
		}
		output["result"] = sanitize(result.Raw(), token)
		return output, nil
	}
	if o.op == "rerun" || o.op == "rerun-preflight" {
		if e := checkRerunEngine(o, conn.EngineVersion, conn.EngineCommit); e != nil {
			return nil, e
		}
		source, e := preflightRerun(ctx, sdkRerunReader{qc: qc}, o)
		if e != nil {
			return nil, fmt.Errorf("rerun preflight refused: %s", sanitize(e.Error(), token))
		}
		delete(output, "profile")
		output["source"] = source
		output["livePreflightPerformed"] = true
		output["failedOnly"] = rerunMode(o) == rerunFailedOnly
		output["rerunMode"] = rerunMode(o)
		output["requires"] = rerunRequirements
		output["carryForward"] = failedOnlySemantics
		if o.op == "rerun-preflight" {
			output["planned"] = true
			output["remoteWrites"] = false
			output["nativeCall"] = rerunCall(o)
			return output, nil
		}
	}
	if write && o.candidate != "" {
		if err := preflightCandidate(ctx, qc, o); err != nil {
			return nil, fmt.Errorf("candidate preflight refused: %s", sanitize(err.Error(), token))
		}
	}
	if o.op == "rerun" && rerunMode(o) == rerunFailedOnly {
		executable, e := os.Executable()
		if e != nil {
			return nil, errors.New("cannot locate failed-only operator intent directory")
		}
		result, e := executeFailedOnlyOnce(ctx, filepath.Join(filepath.Dir(executable), "rerun-intents"), o, func() (any, error) {
			result, err := qc.ExecuteNamed(ctx, name, call)
			if err != nil {
				return nil, err
			}
			return sanitize(result.Raw(), token), nil
		})
		if e != nil {
			return nil, fmt.Errorf("failed-only invocation refused or uncertain; reconcile before retry: %s", sanitize(e.Error(), token))
		}
		output["result"] = result
		return output, nil
	}
	result, err := qc.ExecuteNamed(ctx, name, call)
	if err != nil {
		return nil, fmt.Errorf("native call failed (do not automatically replay writes): %s", sanitize(err.Error(), token))
	}
	output["result"] = sanitize(result.Raw(), token)
	return output, nil
}

func main() {
	o := parse()
	name, call, write, err := selectCall(o)
	if err != nil {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": false, "error": err.Error()})
		os.Exit(2)
	}
	if (write || o.op == "candidate-assets") && !o.execute {
		if o.op == "rerun" {
			_ = json.NewEncoder(os.Stdout).Encode(rerunPlan(o, call))
			return
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": true, "planned": true, "operation": o.op, "nativeCall": call})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
	type answer struct {
		result map[string]any
		err    error
	}
	done := make(chan answer, 1)
	go func() { r, e := perform(ctx, o, name, call, write); done <- answer{r, e} }()
	select {
	case a := <-done:
		if a.err != nil {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": false, "operation": o.op, "uncertain": write, "error": a.err.Error()})
			os.Exit(5)
		}
		a.result["ok"] = true
		_ = json.NewEncoder(os.Stdout).Encode(a.result)
	case <-ctx.Done():
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": false, "operation": o.op, "uncertain": write, "error": "request deadline reached; observe durable state before any write retry"})
		os.Exit(5)
	}
}
