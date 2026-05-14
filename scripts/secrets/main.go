// scripts/secrets is a small operator tool for the env-var refactor.
// It reads the committed manifest at scripts/secrets/manifest.yaml,
// the developer's personal ~/.memql/dev-secrets.yaml, and talks to a
// running memQL over gRPC to seed / list / set encrypted secrets and
// plaintext variables.
//
// Subcommands:
//
//	secrets init     interactive; walks the manifest and writes the
//	                 user's dev-secrets.yaml (prompting only for the
//	                 entries that don't already have a value)
//	secrets seed     reads the user's yaml and upserts every entry
//	                 into the right concept on the running memQL
//	                 (encrypts secrets first via component/secret)
//	secrets list     prints a table of manifest entries, their scope,
//	                 and whether a value exists in the yaml
//	secrets set      one-off set (name + value + scope) without
//	                 touching the yaml
//	secrets delete   soft-delete by name + scope
//
// Usage is almost always via the Makefile (make secrets-init, etc.);
// Go-level invocation is:
//
//	go run ./scripts/secrets init
//	MEMQL_MASTER_KEY=... go run ./scripts/secrets seed
//
// Requires:
//   - MEMQL_MASTER_KEY in env for seed / set (needed to encrypt secrets)
//   - a running memQL reachable at $MEMQL_GRPC_ENDPOINT (default
//     localhost:50051)
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
	"github.com/visionarys-io/memql/component/secret"
)

// manifestEntry is one row in scripts/secrets/manifest.yaml. Both
// secrets and variables share the shape; `Kind` is meaningful only
// for secrets.
type manifestEntry struct {
	Name        string `yaml:"name"`
	Scope       string `yaml:"scope"` // "global" | "partition"
	Kind        string `yaml:"kind,omitempty"`
	Description string `yaml:"description,omitempty"`
	Default     string `yaml:"default,omitempty"`
}

type manifest struct {
	Secrets   []manifestEntry `yaml:"secrets"`
	Variables []manifestEntry `yaml:"variables"`
}

// devSecretEntry is the user's local value for a manifest entry. The
// manifest tells the tool which concept to write; this file holds the
// actual string.
type devSecretEntry struct {
	Name      string `yaml:"name"`
	Scope     string `yaml:"scope"`
	Kind      string `yaml:"kind,omitempty"`
	Partition string `yaml:"partition,omitempty"` // only for scope=partition
	Value     string `yaml:"value"`
}

type devSecretsFile struct {
	// MasterKey is the 32-byte hex-encoded encryption key used to
	// seal v1:platform:globalSecret / v1:platform:partitionSecret rows. Stored in this
	// gitignored yaml so the dev workflow has a single source of
	// truth -- `make secrets-init` generates it on first run, every
	// other Make target reads it from here and exports MEMQL_MASTER_KEY
	// into the environment for child processes (memQL containers,
	// scripts/secrets subcommands).
	MasterKey string           `yaml:"masterKey,omitempty"`
	Secrets   []devSecretEntry `yaml:"secrets"`
	Variables []devSecretEntry `yaml:"variables"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	sub := os.Args[1]
	args := os.Args[2:]

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	var err error
	switch sub {
	case "init":
		err = cmdInit()
	case "seed":
		err = cmdSeed(logger)
	case "list":
		err = cmdList(logger)
	case "set":
		err = cmdSet(logger, args)
	case "delete":
		err = cmdDelete(logger, args)
	case "export":
		err = cmdExport(logger)
	case "master-key":
		err = cmdMasterKey()
	case "health":
		err = cmdHealth()
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", sub)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `memQL dev-secrets tool

Subcommands:
  init     Interactively populate ~/.memql/dev-secrets.yaml from the
           committed manifest, prompting for any missing values.
  seed     Read ~/.memql/dev-secrets.yaml and push every entry to a
           running memQL (requires MEMQL_MASTER_KEY for secrets).
  export   Pull every secret + variable from a running memQL, decrypt
           secrets locally, and merge into ~/.memql/dev-secrets.yaml
           (memQL wins on conflict). Used by 'make dev-fresh' to
           back up before purging the database. Exits 0 with a
           warning if memQL is unreachable -- so the surrounding
           pipeline continues.
  list     Show manifest vs yaml vs memQL state as a summary table.
  master-key
           Print the master key stored in ~/.memql/dev-secrets.yaml
           to stdout. Used by Make targets that need to export
           MEMQL_MASTER_KEY into env before invoking docker compose
           or the memQL containers. Exits non-zero if the yaml has
           no masterKey field -- run 'secrets-init' first.
  health   Quick check that the running memQL is reachable. Connects
           via gRPC + handshake. Exits 0 with "ok" on success, 1 with
           a short error on failure. Used by 'make dev-refresh' to
           confirm readiness before seeding (port-open alone isn't
           enough; the server might still be loading concepts).
  set      Upsert one entry. By default also records the value in
           ~/.memql/dev-secrets.yaml so it survives db-purge-and-reseed.
           Pass --no-persist for a transient memQL-only write.
             scripts/secrets set <variable|secret> <name> <value>
                  --scope=global|partition [--partition=<name>]
                  [--kind=<kind>] [--no-persist]
  delete   Soft-delete (active=false). Also removes the entry from
           the yaml unless --no-persist is given.
             scripts/secrets delete <variable|secret> <name>
                  --scope=global|partition [--partition=<name>]
                  [--no-persist]

Env:
  MEMQL_MASTER_KEY      required for seed/set on secret rows
  MEMQL_GRPC_ENDPOINT   default localhost:50051
`)
}

// -----------------------------------------------------------------
// File paths
// -----------------------------------------------------------------

func manifestPath() string {
	// The manifest lives next to this main.go in the repo.
	wd, _ := os.Getwd()
	return filepath.Join(wd, "scripts", "secrets", "manifest.yaml")
}

func devSecretsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".memql")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "dev-secrets.yaml"), nil
}

func loadManifest() (*manifest, error) {
	b, err := os.ReadFile(manifestPath())
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m manifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

func loadDevSecrets() (*devSecretsFile, string, error) {
	path, err := devSecretsPath()
	if err != nil {
		return nil, "", err
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &devSecretsFile{}, path, nil
	}
	if err != nil {
		return nil, path, err
	}
	var d devSecretsFile
	if err := yaml.Unmarshal(b, &d); err != nil {
		return nil, path, fmt.Errorf("parse %s: %w", path, err)
	}
	// Auto-load the master key from yaml into the process env if not
	// already set. This means every subcommand that calls
	// loadDevSecrets() can rely on os.Getenv(MEMQL_MASTER_KEY) being
	// populated, without each one needing its own bootstrap dance.
	if d.MasterKey != "" && os.Getenv(secret.EnvMasterKey) == "" {
		_ = os.Setenv(secret.EnvMasterKey, strings.TrimSpace(d.MasterKey))
	}
	return &d, path, nil
}

func saveDevSecrets(d *devSecretsFile) error {
	path, err := devSecretsPath()
	if err != nil {
		return err
	}
	b, err := yaml.Marshal(d)
	if err != nil {
		return err
	}
	// 0600: user-only read/write.
	return os.WriteFile(path, b, 0o600)
}

// -----------------------------------------------------------------
// init
// -----------------------------------------------------------------

func cmdInit() error {
	m, err := loadManifest()
	if err != nil {
		return err
	}
	d, path, err := loadDevSecrets()
	if err != nil {
		return err
	}

	fmt.Printf("Populating %s\n", path)
	fmt.Println("Press enter to accept defaults / skip. Ctrl-C to abort.")
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)

	// Master key first. This unlocks every encrypted secret stored
	// in v1:platform:globalSecret / v1:platform:partitionSecret -- losing it means
	// every encrypted row becomes unrecoverable. The default is to
	// auto-generate; an operator who already has a key (e.g.
	// migrating from a previous install or syncing across machines)
	// can paste it.
	if strings.TrimSpace(d.MasterKey) == "" {
		fmt.Println("[bootstrap] MEMQL_MASTER_KEY")
		fmt.Println("  32-byte hex-encoded master key. Used to seal every encrypted")
		fmt.Println("  secret. Press enter to auto-generate; paste an existing key")
		fmt.Println("  if you're cloning state from another install.")
		fmt.Print("  value (hidden, blank = auto-generate): ")
		raw, err := readMasked()
		if err != nil {
			return err
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			generated, err := generateMasterKey()
			if err != nil {
				return fmt.Errorf("generate master key: %w", err)
			}
			d.MasterKey = generated
			fmt.Println("  (auto-generated, saved to yaml)")
		} else {
			if !looksLikeHex32(raw) {
				return fmt.Errorf("master key must be 32 bytes hex-encoded (64 hex chars), got %d chars", len(raw))
			}
			d.MasterKey = raw
			fmt.Println("  (saved to yaml)")
		}
		// Make it available to the rest of this process so secret
		// encryption works on the same run.
		_ = os.Setenv(secret.EnvMasterKey, d.MasterKey)
		// Persist immediately so a Ctrl-C halfway through the rest of
		// the prompts doesn't lose the master key.
		if err := saveDevSecrets(d); err != nil {
			return err
		}
		fmt.Println()
	} else {
		fmt.Println("[bootstrap] MEMQL_MASTER_KEY already set in yaml; skipping")
		fmt.Println()
	}

	// Secrets next (masked input).
	for _, ent := range m.Secrets {
		existing := findDevEntry(d.Secrets, ent.Name, ent.Scope)
		if existing != nil && existing.Value != "" {
			fmt.Printf("[secret/%s] %-30s already set in yaml; skipping\n", ent.Scope, ent.Name)
			continue
		}
		fmt.Printf("[secret/%s] %s\n", ent.Scope, ent.Name)
		if ent.Description != "" {
			fmt.Printf("  %s\n", ent.Description)
		}
		fmt.Print("  value (hidden): ")
		val, err := readMasked()
		if err != nil {
			return err
		}
		if val == "" {
			fmt.Println("  (skipped)")
			fmt.Println()
			continue
		}
		if existing != nil {
			existing.Value = val
			existing.Kind = ent.Kind
		} else {
			d.Secrets = append(d.Secrets, devSecretEntry{
				Name:  ent.Name,
				Scope: ent.Scope,
				Kind:  ent.Kind,
				Value: val,
			})
		}
		fmt.Println()
	}

	// Variables (plain input).
	for _, ent := range m.Variables {
		existing := findDevEntry(d.Variables, ent.Name, ent.Scope)
		if existing != nil && existing.Value != "" {
			fmt.Printf("[variable/%s] %-30s already set in yaml; skipping\n", ent.Scope, ent.Name)
			continue
		}
		fmt.Printf("[variable/%s] %s\n", ent.Scope, ent.Name)
		if ent.Description != "" {
			fmt.Printf("  %s\n", ent.Description)
		}
		prompt := "  value"
		if ent.Default != "" {
			prompt = fmt.Sprintf("  value [default: %s]", ent.Default)
		}
		fmt.Print(prompt + ": ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		val := strings.TrimSpace(line)
		if val == "" {
			val = ent.Default
		}
		if val == "" {
			fmt.Println("  (skipped)")
			fmt.Println()
			continue
		}
		if existing != nil {
			existing.Value = val
		} else {
			d.Variables = append(d.Variables, devSecretEntry{
				Name:  ent.Name,
				Scope: ent.Scope,
				Value: val,
			})
		}
		fmt.Println()
	}

	if err := saveDevSecrets(d); err != nil {
		return err
	}
	fmt.Printf("Wrote %s\n", path)
	fmt.Println("Run `make secrets-seed` to push these values into memQL.")
	return nil
}

func findDevEntry(list []devSecretEntry, name, scope string) *devSecretEntry {
	for i := range list {
		if list[i].Name == name && list[i].Scope == scope {
			return &list[i]
		}
	}
	return nil
}

// generateMasterKey returns a fresh 32-byte hex-encoded encryption
// key suitable for the masterKey field in dev-secrets.yaml. The
// underlying randomness is crypto/rand so it's safe for production
// use too -- the only thing dev-specific about this is the storage
// location.
func generateMasterKey() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// looksLikeHex32 reports whether `s` decodes to 32 bytes of hex
// (64 hex chars). Quick sanity check on a paste.
func looksLikeHex32(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// cmdMasterKey prints the master key from the yaml to stdout.
// Used by Make targets to extract the value via shell command
// substitution and pass it to docker compose / scripts/secrets.
func cmdMasterKey() error {
	d, _, err := loadDevSecrets()
	if err != nil {
		return err
	}
	if strings.TrimSpace(d.MasterKey) == "" {
		return fmt.Errorf("no masterKey in %s; run 'make secrets-init' first", mustDevSecretsPath())
	}
	fmt.Println(strings.TrimSpace(d.MasterKey))
	return nil
}

// cmdHealth connects to the running memQL via gRPC and runs the
// ClientHello / ServerHello handshake. Used by Make targets to
// confirm readiness with stronger semantics than a simple
// `nc -z localhost 50050` check -- the port can be open while the
// server is still loading concepts and rejecting Stream RPCs.
//
// Exits 0 + prints "ok" on a successful handshake.
// Exits 1 + prints a one-line error otherwise.
// Either way, output goes to stdout so Make targets can capture it.
func cmdHealth() error {
	conn, err := connectGRPC()
	if err != nil {
		fmt.Printf("not-ready: dial: %v\n", err)
		return err
	}
	defer conn.Close()

	client := memqlv1.NewMemqlServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.Stream(withOperatorAuth(ctx))
	if err != nil {
		fmt.Printf("not-ready: open stream: %v\n", err)
		return err
	}
	defer stream.CloseSend()

	if err := handshake(stream); err != nil {
		fmt.Printf("not-ready: handshake: %v\n", err)
		return err
	}
	fmt.Println("ok")
	return nil
}

func readMasked() (string, error) {
	// term.ReadPassword needs a TTY. If stdin isn't a terminal (e.g.
	// piped input), fall back to reading a line plainly.
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		return strings.TrimSpace(line), err
	}
	raw, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// -----------------------------------------------------------------
// seed
// -----------------------------------------------------------------

func cmdSeed(logger *slog.Logger) error {
	d, path, err := loadDevSecrets()
	if err != nil {
		return err
	}
	if len(d.Secrets) == 0 && len(d.Variables) == 0 {
		return fmt.Errorf("%s is empty; run `make secrets-init` first", path)
	}
	if err := ensureMasterKey(d); err != nil {
		return err
	}

	conn, err := connectGRPC()
	if err != nil {
		return err
	}
	defer conn.Close()
	client := memqlv1.NewMemqlServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stream, err := client.Stream(withOperatorAuth(ctx))
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}
	defer stream.CloseSend()

	if err := handshake(stream); err != nil {
		return err
	}

	applied := 0
	skipped := 0
	for _, e := range d.Secrets {
		if e.Value == "" {
			skipped++
			continue
		}
		ct, fp, err := secret.Encrypt(e.Value)
		if err != nil {
			return fmt.Errorf("encrypt %s: %w", e.Name, err)
		}
		mutationName, id := secretMutationFor(e)
		payload := map[string]any{
			"id":             id,
			"name":           e.Name,
			"encryptedValue": ct,
			"fingerprint":    fp,
			"kind":           strOrEmpty(e.Kind),
			"description":    "",
			"addedBy":        "make secrets-seed",
			"active":         true,
		}
		if err := runMutation(stream, mutationName, payload, e.Partition); err != nil {
			return fmt.Errorf("seed secret %s: %w", e.Name, err)
		}
		logger.Info("secret seeded", "name", e.Name, "scope", e.Scope, "fingerprint", fp)
		applied++
	}
	for _, e := range d.Variables {
		if e.Value == "" {
			skipped++
			continue
		}
		mutationName, id := variableMutationFor(e)
		payload := map[string]any{
			"id":          id,
			"name":        e.Name,
			"value":       e.Value,
			"description": "",
			"active":      true,
		}
		if err := runMutation(stream, mutationName, payload, e.Partition); err != nil {
			return fmt.Errorf("seed variable %s: %w", e.Name, err)
		}
		logger.Info("variable seeded", "name", e.Name, "scope", e.Scope)
		applied++
	}

	fmt.Printf("\nApplied %d entries (skipped %d empty). See `make secrets-list` for verification.\n", applied, skipped)
	return nil
}

func secretMutationFor(e devSecretEntry) (mutation string, id string) {
	if e.Scope == "global" {
		return "mutationSetGlobalSecret", "secret-global-" + slugify(e.Name)
	}
	return "mutationSetPartitionSecret", "secret-" + slugify(e.Name)
}

func variableMutationFor(e devSecretEntry) (mutation string, id string) {
	if e.Scope == "global" {
		return "mutationSetGlobalVariable", "var-global-" + slugify(e.Name)
	}
	return "mutationSetPartitionVariable", "var-" + slugify(e.Name)
}

func slugify(name string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), "_", "-"))
}

func strOrEmpty(s string) string {
	if s == "" {
		return ""
	}
	return s
}

// -----------------------------------------------------------------
// list
// -----------------------------------------------------------------

func cmdList(_ *slog.Logger) error {
	m, err := loadManifest()
	if err != nil {
		return err
	}
	d, path, err := loadDevSecrets()
	if err != nil {
		return err
	}
	fmt.Printf("Manifest:    scripts/secrets/manifest.yaml\n")
	fmt.Printf("Local yaml:  %s\n\n", path)

	fmt.Printf("SECRETS\n%-30s %-10s %-18s %s\n", "NAME", "SCOPE", "KIND", "IN_YAML")
	for _, ent := range m.Secrets {
		hit := findDevEntry(d.Secrets, ent.Name, ent.Scope) != nil
		fmt.Printf("%-30s %-10s %-18s %v\n", ent.Name, ent.Scope, ent.Kind, hit)
	}
	fmt.Printf("\nVARIABLES\n%-30s %-10s %s\n", "NAME", "SCOPE", "IN_YAML")
	for _, ent := range m.Variables {
		hit := findDevEntry(d.Variables, ent.Name, ent.Scope) != nil
		fmt.Printf("%-30s %-10s %v\n", ent.Name, ent.Scope, hit)
	}
	return nil
}

// -----------------------------------------------------------------
// set / delete
//
// Both `set` and `delete` keep ~/.memql/dev-secrets.yaml in sync with
// what's pushed to memQL so the value survives `make
// db-purge-and-reseed`. Pass `--no-persist` to skip the yaml update
// when you really do want a transient one-off (e.g. testing a value
// you don't want to commit to your local config).
// -----------------------------------------------------------------

func cmdSet(logger *slog.Logger, args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("usage: set <variable|secret> <name> <value> [--scope=...] [--partition=...] [--kind=...] [--no-persist]")
	}
	kind := args[0]
	name := args[1]
	value := args[2]
	flags := parseFlags(args[3:])
	scope := flags["scope"]
	if scope == "" {
		scope = "global"
	}
	partition := flags["partition"]
	kindTag := flags["kind"]
	_, noPersist := flags["no-persist"]

	conn, err := connectGRPC()
	if err != nil {
		return err
	}
	defer conn.Close()
	client := memqlv1.NewMemqlServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.Stream(withOperatorAuth(ctx))
	if err != nil {
		return err
	}
	defer stream.CloseSend()
	if err := handshake(stream); err != nil {
		return err
	}

	switch kind {
	case "secret":
		if os.Getenv(secret.EnvMasterKey) == "" {
			return fmt.Errorf("%s is required to set a secret", secret.EnvMasterKey)
		}
		ct, fp, err := secret.Encrypt(value)
		if err != nil {
			return err
		}
		entry := devSecretEntry{Name: name, Scope: scope, Partition: partition, Kind: kindTag}
		mutationName, id := secretMutationFor(entry)
		payload := map[string]any{
			"id":             id,
			"name":           name,
			"encryptedValue": ct,
			"fingerprint":    fp,
			"kind":           kindTag,
			"addedBy":        "make secret-set",
			"active":         true,
		}
		if err := runMutation(stream, mutationName, payload, partition); err != nil {
			return err
		}
		logger.Info("secret set", "name", name, "scope", scope, "fingerprint", fp)
		if !noPersist {
			if err := persistSecret(name, scope, kindTag, partition, value); err != nil {
				return fmt.Errorf("update local yaml: %w", err)
			}
			logger.Info("yaml updated", "path", mustDevSecretsPath())
		}
	case "variable":
		entry := devSecretEntry{Name: name, Scope: scope, Partition: partition}
		mutationName, id := variableMutationFor(entry)
		payload := map[string]any{
			"id":     id,
			"name":   name,
			"value":  value,
			"active": true,
		}
		if err := runMutation(stream, mutationName, payload, partition); err != nil {
			return err
		}
		logger.Info("variable set", "name", name, "scope", scope)
		if !noPersist {
			if err := persistVariable(name, scope, partition, value); err != nil {
				return fmt.Errorf("update local yaml: %w", err)
			}
			logger.Info("yaml updated", "path", mustDevSecretsPath())
		}
	default:
		return fmt.Errorf("kind must be 'secret' or 'variable', got %q", kind)
	}
	return nil
}

// persistSecret upserts the given secret into the user's yaml so it
// will be re-applied by `make secrets-seed` (and therefore survive
// `make db-purge-and-reseed`). Idempotent: an existing entry with the
// same name+scope is updated in place.
func persistSecret(name, scope, kindTag, partition, value string) error {
	d, _, err := loadDevSecrets()
	if err != nil {
		return err
	}
	if existing := findDevEntry(d.Secrets, name, scope); existing != nil {
		existing.Value = value
		if kindTag != "" {
			existing.Kind = kindTag
		}
		if partition != "" {
			existing.Partition = partition
		}
	} else {
		d.Secrets = append(d.Secrets, devSecretEntry{
			Name:      name,
			Scope:     scope,
			Kind:      kindTag,
			Partition: partition,
			Value:     value,
		})
	}
	return saveDevSecrets(d)
}

// persistVariable upserts the given variable into the user's yaml.
// Same idempotent semantics as persistSecret.
func persistVariable(name, scope, partition, value string) error {
	d, _, err := loadDevSecrets()
	if err != nil {
		return err
	}
	if existing := findDevEntry(d.Variables, name, scope); existing != nil {
		existing.Value = value
		if partition != "" {
			existing.Partition = partition
		}
	} else {
		d.Variables = append(d.Variables, devSecretEntry{
			Name:      name,
			Scope:     scope,
			Partition: partition,
			Value:     value,
		})
	}
	return saveDevSecrets(d)
}

// removeFromSecretsYaml drops an entry from the yaml. Used by delete
// so that purge-and-reseed doesn't reanimate a soft-deleted secret.
func removeFromSecretsYaml(kind, name, scope string) error {
	d, _, err := loadDevSecrets()
	if err != nil {
		return err
	}
	switch kind {
	case "secret":
		d.Secrets = filterDevEntries(d.Secrets, name, scope)
	case "variable":
		d.Variables = filterDevEntries(d.Variables, name, scope)
	}
	return saveDevSecrets(d)
}

func filterDevEntries(list []devSecretEntry, name, scope string) []devSecretEntry {
	out := make([]devSecretEntry, 0, len(list))
	for _, e := range list {
		if e.Name == name && e.Scope == scope {
			continue
		}
		out = append(out, e)
	}
	return out
}

func mustDevSecretsPath() string {
	p, _ := devSecretsPath()
	return p
}

func cmdDelete(logger *slog.Logger, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: delete <variable|secret> <name> [--scope=...] [--partition=...] [--no-persist]")
	}
	kind := args[0]
	name := args[1]
	flags := parseFlags(args[2:])
	scope := flags["scope"]
	if scope == "" {
		scope = "global"
	}
	partition := flags["partition"]
	_, noPersist := flags["no-persist"]

	// Soft-delete = set active=false. Reuse set machinery with an
	// empty value so the mutation runs.
	conn, err := connectGRPC()
	if err != nil {
		return err
	}
	defer conn.Close()
	client := memqlv1.NewMemqlServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.Stream(withOperatorAuth(ctx))
	if err != nil {
		return err
	}
	defer stream.CloseSend()
	if err := handshake(stream); err != nil {
		return err
	}

	entry := devSecretEntry{Name: name, Scope: scope, Partition: partition}
	var mutationName, id string
	var payload map[string]any
	switch kind {
	case "secret":
		mutationName, id = secretMutationFor(entry)
		payload = map[string]any{"id": id, "name": name, "encryptedValue": "", "fingerprint": "", "active": false}
	case "variable":
		mutationName, id = variableMutationFor(entry)
		payload = map[string]any{"id": id, "name": name, "value": "", "active": false}
	default:
		return fmt.Errorf("kind must be 'secret' or 'variable', got %q", kind)
	}
	if !noPersist {
		if err := removeFromSecretsYaml(kind, name, scope); err != nil {
			return fmt.Errorf("update local yaml: %w", err)
		}
	}
	if err := runMutation(stream, mutationName, payload, partition); err != nil {
		return err
	}
	logger.Info("deleted (soft)", "name", name, "scope", scope)
	return nil
}

func parseFlags(args []string) map[string]string {
	out := map[string]string{}
	for _, a := range args {
		a = strings.TrimPrefix(a, "--")
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			continue
		}
		out[k] = v
	}
	return out
}

// -----------------------------------------------------------------
// gRPC plumbing
// -----------------------------------------------------------------

func connectGRPC() (*grpc.ClientConn, error) {
	// MEMQL_GRPC_ENDPOINT is the override knob. Two shapes are
	// recognized:
	//
	//   https://bff.<domain>      TLS, port 443 implied
	//   https://bff.<domain>:443  TLS, explicit port
	//   bff.<domain>:443          TLS (auto-detected via :443 suffix)
	//   bff.<domain>:50051        plaintext gRPC (cluster-mode direct)
	//   localhost:50051           plaintext gRPC (legacy / debug path)
	//
	// Default targets the dev cluster's nginx :443 entry point at
	// bff.local.znas.io -- IDENTITY_BOOTSTRAP_DOMAIN overrides the
	// hostname suffix when an operator runs against a different
	// domain. Mkcert installed its root CA in the system trust
	// store, so Go's tls.Config{} (no RootCAs override) verifies
	// the dev cert without warnings.
	endpoint := os.Getenv("MEMQL_GRPC_ENDPOINT")
	useTLS := false
	if endpoint == "" {
		domain := os.Getenv("IDENTITY_BOOTSTRAP_DOMAIN")
		if strings.TrimSpace(domain) == "" {
			domain = "local.znas.io"
		}
		endpoint = "bff." + domain + ":443"
		useTLS = true
	} else {
		// Strip an optional https:// prefix and detect TLS by
		// scheme or port suffix. http:// is treated as plaintext.
		switch {
		case strings.HasPrefix(endpoint, "https://"):
			endpoint = strings.TrimPrefix(endpoint, "https://")
			useTLS = true
		case strings.HasPrefix(endpoint, "http://"):
			endpoint = strings.TrimPrefix(endpoint, "http://")
		}
		if !useTLS && strings.HasSuffix(endpoint, ":443") {
			useTLS = true
		}
		// Default port: TLS endpoints get :443, plaintext get :50051.
		if !strings.Contains(endpoint, ":") {
			if useTLS {
				endpoint += ":443"
			} else {
				endpoint += ":50051"
			}
		}
	}
	if useTLS {
		return grpc.NewClient(endpoint, grpc.WithTransportCredentials(credentials.NewTLS(nil)))
	}
	return grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
}

// withOperatorAuth stamps the operator credential onto outgoing
// gRPC metadata so the cluster's operator-aware stream interceptor
// admits the stream as a synthetic cluster owner. The credential
// is the cluster master key (MEMQL_MASTER_KEY); the secrets tool
// already requires the same value to encrypt secret payloads, so
// no additional credential surface is introduced.
//
// When MEMQL_MASTER_KEY is empty the call is a no-op -- the
// downstream RPC will then fail with codes.Unauthenticated, which
// is the right outcome for a cluster that has no operator
// credential configured.
func withOperatorAuth(ctx context.Context) context.Context {
	key := strings.TrimSpace(os.Getenv(secret.EnvMasterKey))
	if key == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Operator "+key)
}

func handshake(stream memqlv1.MemqlService_StreamClient) error {
	hello := &memqlv1.MemqlClientMessage{
		MessageId: uuid.NewString(),
		Payload: &memqlv1.MemqlClientMessage_ClientHello{
			ClientHello: &memqlv1.ClientHello{
				ClientId:   "make-secrets",
				SdkName:    "memql-secrets-tool",
				SdkVersion: "1",
			},
		},
	}
	if err := stream.Send(hello); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}
	// Drain the ServerHello.
	srvMsg, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("recv server hello: %w", err)
	}
	if srvMsg.GetServerHello() == nil {
		return fmt.Errorf("expected ServerHello, got %T", srvMsg.Payload)
	}
	return nil
}

func runMutation(stream memqlv1.MemqlService_StreamClient, mutationName string, args map[string]any, partition string) error {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("marshal args: %w", err)
	}
	query := fmt.Sprintf("%s(%s)", mutationName, string(argsJSON))
	msgId := uuid.NewString()
	msg := &memqlv1.MemqlClientMessage{
		MessageId: msgId,
		Partition: partition,
		Payload: &memqlv1.MemqlClientMessage_ExecuteQuery{
			ExecuteQuery: &memqlv1.ExecuteQueryMsg{
				RequestId: uuid.NewString(),
				Query:     query,
			},
		},
	}
	if err := stream.Send(msg); err != nil {
		return fmt.Errorf("send mutation: %w", err)
	}
	// Wait for the matching response.
	for {
		resp, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("recv: %w", err)
		}
		if resp.CorrelateTo == msgId {
			if errorMsg := resp.GetQueryError(); errorMsg != nil {
				return fmt.Errorf("%s", errorMsg.String())
			}
			return nil
		}
	}
}

// runQuery sends an ExecuteQuery and accumulates the result nodes from
// every chunk until done=true. Used by cmdExport to read the raw
// payloads of v1:platform:globalSecret / v1:platform:partitionSecret /
// v1:platform:globalVariable / v1:platform:partitionVariable rows.
func runQuery(stream memqlv1.MemqlService_StreamClient, query, partition string) ([]*memqlv1.MemoryNode, error) {
	msgId := uuid.NewString()
	msg := &memqlv1.MemqlClientMessage{
		MessageId: msgId,
		Partition: partition,
		Payload: &memqlv1.MemqlClientMessage_ExecuteQuery{
			ExecuteQuery: &memqlv1.ExecuteQueryMsg{
				RequestId: uuid.NewString(),
				Query:     query,
			},
		},
	}
	if err := stream.Send(msg); err != nil {
		return nil, fmt.Errorf("send query: %w", err)
	}
	var nodes []*memqlv1.MemoryNode
	for {
		resp, err := stream.Recv()
		if err != nil {
			return nil, fmt.Errorf("recv: %w", err)
		}
		if resp.CorrelateTo != msgId {
			continue
		}
		if errorMsg := resp.GetQueryError(); errorMsg != nil {
			return nil, fmt.Errorf("%s", errorMsg.String())
		}
		chunk := resp.GetQueryResult()
		if chunk == nil {
			continue
		}
		if r := chunk.Result; r != nil {
			if b := r.GetBundle(); b != nil {
				nodes = append(nodes, b.GetNodes()...)
			}
		}
		if chunk.Done {
			return nodes, nil
		}
	}
}

// -----------------------------------------------------------------
// helpers
// -----------------------------------------------------------------

func ensureMasterKey(d *devSecretsFile) error {
	if os.Getenv(secret.EnvMasterKey) != "" {
		return nil
	}
	// Only fatal if we actually have secrets to write. Variable-only
	// seed runs work without the master key.
	hasSecrets := false
	for _, e := range d.Secrets {
		if e.Value != "" {
			hasSecrets = true
			break
		}
	}
	if !hasSecrets {
		return nil
	}
	return masterKeyMissingError()
}

// masterKeyMissingError formats the standard "set it somewhere"
// guidance. Centralized so the message is consistent across all
// subcommands that need the key.
func masterKeyMissingError() error {
	return fmt.Errorf(`%s is not set.

Either:
  1. Generate one and add it to your shell rc (recommended for the
     "make dev-fresh" workflow):

       echo "export %s=$(openssl rand -hex 32)" >> ~/.zshrc
       (or ~/.bashrc, then re-source it: source ~/.zshrc)

  2. Or add it to .env.local in the memql repo:

       echo "%s=$(openssl rand -hex 32)" >> .env.local

  3. Or run "make bootstrap" once -- that drops a generated key into
     .env.local. (Pick exactly one of these; if you have it in BOTH
     the shell rc AND .env.local make sure they MATCH or your
     decryption will fail.)`,
		secret.EnvMasterKey, secret.EnvMasterKey, secret.EnvMasterKey)
}

// -----------------------------------------------------------------
// export
// -----------------------------------------------------------------
//
// cmdExport pulls every active secret + variable row from the
// running memQL into ~/.memql/dev-secrets.yaml. Used by
// `make dev-fresh` to back up state before the database gets nuked,
// so a subsequent `secrets-seed` puts the same values back.
//
// Conflict resolution: memQL wins. If the yaml has a value for a
// name that's also in memQL, the yaml value is overwritten with what
// memQL has. Local-only edits (in yaml but not yet seeded) survive
// because they aren't in memQL to overwrite them.
//
// Failure mode: if memQL is unreachable (likely cause: it's not up
// yet on a fresh clone), cmdExport prints a warning and exits 0 so
// the surrounding `make dev-fresh` pipeline can continue. The seed
// step at the end will use whatever's already in the yaml.

func cmdExport(logger *slog.Logger) error {
	// Master key is required because we decrypt locally.
	if os.Getenv(secret.EnvMasterKey) == "" {
		return masterKeyMissingError()
	}

	d, _, err := loadDevSecrets()
	if err != nil {
		return err
	}

	conn, err := connectGRPC()
	if err != nil {
		fmt.Fprintln(os.Stderr, "       no running memQL detected; skipping backup (yaml left as-is)")
		return nil
	}
	defer conn.Close()
	client := memqlv1.NewMemqlServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.Stream(withOperatorAuth(ctx))
	if err != nil {
		fmt.Fprintln(os.Stderr, "       no running memQL detected; skipping backup (yaml left as-is)")
		return nil
	}
	defer stream.CloseSend()
	if err := handshake(stream); err != nil {
		fmt.Fprintln(os.Stderr, "       running memQL did not finish booting in time; skipping backup")
		return nil
	}

	exportedSecrets := 0
	exportedVars := 0

	// ----- Global rows (v1:platform:*) -----
	{
		nodes, err := runQuery(stream, `concept==v1:platform:globalVariable;payload.active==true`, "")
		if err != nil {
			return fmt.Errorf("query v1:platform:globalVariable: %w", err)
		}
		for _, n := range nodes {
			name, value := readVariableNode(n)
			if name == "" {
				continue
			}
			upsertDevVariable(d, name, "global", "", value)
			exportedVars++
		}
	}
	{
		nodes, err := runQuery(stream, `concept==v1:platform:globalSecret;payload.active==true`, "")
		if err != nil {
			return fmt.Errorf("query v1:platform:globalSecret: %w", err)
		}
		for _, n := range nodes {
			name, ciphertext, kind := readSecretNode(n)
			if name == "" || ciphertext == "" {
				continue
			}
			plaintext, decErr := secret.Decrypt(ciphertext)
			if decErr != nil {
				logger.Warn("decrypt failed; skipping (master key mismatch?)", "name", name, "error", decErr)
				continue
			}
			upsertDevSecret(d, name, "global", kind, "", plaintext)
			exportedSecrets++
		}
	}

	// ----- Partition-scoped rows (v1:memql:*) -----
	// Default partition only -- a multi-partition install would need
	// to iterate the partition list; skip that until the workflow
	// actually shows up.
	{
		nodes, err := runQuery(stream, `concept==v1:platform:partitionVariable;payload.active==true`, "")
		if err != nil {
			return fmt.Errorf("query v1:platform:partitionVariable: %w", err)
		}
		for _, n := range nodes {
			name, value := readVariableNode(n)
			if name == "" {
				continue
			}
			upsertDevVariable(d, name, "partition", "", value)
			exportedVars++
		}
	}
	{
		nodes, err := runQuery(stream, `concept==v1:platform:partitionSecret;payload.active==true`, "")
		if err != nil {
			return fmt.Errorf("query v1:platform:partitionSecret: %w", err)
		}
		for _, n := range nodes {
			name, ciphertext, kind := readSecretNode(n)
			if name == "" || ciphertext == "" {
				continue
			}
			plaintext, decErr := secret.Decrypt(ciphertext)
			if decErr != nil {
				logger.Warn("decrypt failed; skipping", "name", name, "error", decErr)
				continue
			}
			upsertDevSecret(d, name, "partition", kind, "", plaintext)
			exportedSecrets++
		}
	}

	if err := saveDevSecrets(d); err != nil {
		return err
	}
	fmt.Printf("Exported %d secrets + %d variables from memQL into %s\n",
		exportedSecrets, exportedVars, mustDevSecretsPath())
	return nil
}

// readVariableNode extracts (name, value) from a *:variable row.
func readVariableNode(n *memqlv1.MemoryNode) (string, string) {
	if n == nil || n.Payload == nil {
		return "", ""
	}
	fields := n.Payload.GetFields()
	if fields == nil {
		return "", ""
	}
	name := strings.TrimSpace(structValueString(fields["name"]))
	value := structValueString(fields["value"])
	return name, value
}

// readSecretNode extracts (name, encryptedValue, kind) from a *:secret row.
func readSecretNode(n *memqlv1.MemoryNode) (string, string, string) {
	if n == nil || n.Payload == nil {
		return "", "", ""
	}
	fields := n.Payload.GetFields()
	if fields == nil {
		return "", "", ""
	}
	name := strings.TrimSpace(structValueString(fields["name"]))
	ciphertext := structValueString(fields["encryptedValue"])
	kind := structValueString(fields["kind"])
	return name, ciphertext, kind
}

func structValueString(v *structpb.Value) string {
	if v == nil {
		return ""
	}
	if s, ok := v.Kind.(*structpb.Value_StringValue); ok {
		return s.StringValue
	}
	return ""
}

func upsertDevVariable(d *devSecretsFile, name, scope, partition, value string) {
	if existing := findDevEntry(d.Variables, name, scope); existing != nil {
		existing.Value = value
		if partition != "" {
			existing.Partition = partition
		}
		return
	}
	d.Variables = append(d.Variables, devSecretEntry{
		Name:      name,
		Scope:     scope,
		Partition: partition,
		Value:     value,
	})
}

func upsertDevSecret(d *devSecretsFile, name, scope, kind, partition, value string) {
	if existing := findDevEntry(d.Secrets, name, scope); existing != nil {
		existing.Value = value
		if kind != "" {
			existing.Kind = kind
		}
		if partition != "" {
			existing.Partition = partition
		}
		return
	}
	d.Secrets = append(d.Secrets, devSecretEntry{
		Name:      name,
		Scope:     scope,
		Kind:      kind,
		Partition: partition,
		Value:     value,
	})
}

// Compile-time use of unused imports so they remain valid even if a
// branch above changes shape.
var _ = base64.StdEncoding
