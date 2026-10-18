package worker

import (
	"bytes"
	"context"
	"fmt"
	"github.com/znasllc-io/memql-cockpit/internal/worker/inference"
	"github.com/znasllc-io/memql-cockpit/internal/worker/workbench"
	"gopkg.in/yaml.v3"
	"io"
	"path/filepath"
	"time"
)

func setupWorkbench(ctx context.Context, configPath string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, workbenchPullWithin)
	defer cancel()
	fmt.Fprintln(out, "Preparing Fleet workbenches…")
	runtime := workbench.Docker{}
	if err := runtime.Pull(ctx); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("the workbench image did not finish downloading within %s; check the network and retry", workbenchPullWithin)
		}
		return fmt.Errorf("start Docker with Linux containers, then retry: %w", err)
	}
	if _, err := runtime.Ready(ctx); err != nil {
		return err
	}
	policy := filepath.Join(filepath.Dir(configPath), "policy.yaml")
	if err := inference.EditPolicy(policy, enableWorkbenchPolicy); err != nil {
		return err
	}
	fmt.Fprintln(out, "Fleet workbenches enabled: two isolated workspaces, each limited to 1 CPU and 1.5 GiB.")
	fmt.Fprintln(out, "Workspaces have no access to your files, apps, desktop or local network. Choose who can use them in Fleet → Machine settings → Sharing.")
	if err := inference.Readvertise(ctx); err != nil {
		fmt.Fprintln(out, "Restart Cockpit to make this available in Fleet.")
	} else {
		fmt.Fprintln(out, "Cockpit will report the new feature when it reconnects.")
	}
	return nil
}

// workbenchPullWithin bounds the workbench image pull. An installer waits
// on it, so a stalled pull ends with a sentence rather than a hang.
const workbenchPullWithin = 15 * time.Minute

func enableWorkbenchPolicy(body string) (string, bool, error) {
	// Decode once to reject duplicate keys or malformed YAML without writing.
	var values map[string]any
	if err := yaml.Unmarshal([]byte(body), &values); err != nil {
		return "", false, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		return "", false, err
	}
	if len(doc.Content) == 0 {
		doc.Kind = yaml.DocumentNode
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return "", false, fmt.Errorf("policy must be a YAML mapping")
	}
	mapping := func(parent *yaml.Node, key string) (*yaml.Node, error) {
		for i := 0; i < len(parent.Content); i += 2 {
			if parent.Content[i].Value == key {
				v := parent.Content[i+1]
				if v.Kind != yaml.MappingNode {
					return nil, fmt.Errorf("%s must be a mapping", key)
				}
				return v, nil
			}
		}
		v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
		return v, nil
	}
	block, err := mapping(root, "workbenches")
	if err != nil {
		return "", false, err
	}
	var flag *yaml.Node
	for i := 0; i < len(block.Content); i += 2 {
		if block.Content[i].Value == "allow" {
			flag = block.Content[i+1]
			break
		}
	}
	if flag == nil {
		flag = &yaml.Node{}
		block.Content = append(block.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "allow"}, flag)
	}
	if flag.Tag == "!!bool" && flag.Value == "true" {
		return body, false, nil
	}
	flag.Kind = yaml.ScalarNode
	flag.Tag = "!!bool"
	flag.Value = "true"
	flag.Content = nil
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return "", false, err
	}
	return buf.String(), true, nil
}
