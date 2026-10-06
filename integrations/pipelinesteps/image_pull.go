package pipelinesteps

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

type registryCredential struct {
	Auth     string `json:"auth,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// imagePullConfig accepts inline Docker registry credentials only. Helpers,
// URL paths, wildcard hosts and unrelated configuration never reach kubelet.
// Errors deliberately contain no part of the credential, including JSON's
// normal syntax error detail. The caller owns and cleans the resulting Secret.
func imagePullConfig(value string) (map[string]registryCredential, []string, error) {
	invalid := errors.New("Image pull secret must contain Docker auths with exact registry hosts and inline username/password credentials.")
	if len(value) == 0 || len(value) > 64<<10 {
		return nil, nil, invalid
	}
	var config struct {
		Auths map[string]registryCredential `json:"auths"`
	}
	d := json.NewDecoder(strings.NewReader(value))
	d.DisallowUnknownFields()
	if d.Decode(&config) != nil || d.Decode(new(any)) != io.EOF || len(config.Auths) == 0 || len(config.Auths) > 8 {
		return nil, nil, invalid
	}
	var sensitive []string
	for host, credential := range config.Auths {
		u, err := url.Parse("https://" + host)
		if err != nil || u.Host != host || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(host, "* \\\t\r\n") {
			return nil, nil, invalid
		}
		user, password := credential.Username, credential.Password
		if credential.Auth != "" {
			decoded, err := base64.StdEncoding.DecodeString(credential.Auth)
			u, p, ok := strings.Cut(string(decoded), ":")
			if err != nil || !ok || (user != "" && user != u) || (password != "" && password != p) {
				return nil, nil, invalid
			}
			user, password = u, p
			sensitive = append(sensitive, credential.Auth, string(decoded))
		}
		if user == "" || password == "" || strings.ContainsAny(user, ":\x00\r\n") || strings.ContainsAny(password, "\x00\r\n") {
			return nil, nil, invalid
		}
		sensitive = append(sensitive, password)
	}
	return config.Auths, sensitive, nil
}

func checkImagePull(run StepRun) (map[string]registryCredential, error) {
	if run.Execution != pl.ExecutionContainer || len(run.Needs) != 0 {
		return nil, errors.New("Image pull credentials require a cluster container step.")
	}
	if _, exported := run.Env[run.ImagePullSecret]; exported {
		return nil, errors.New("An image pull credential cannot also be a command environment variable.")
	}
	auths, _, err := imagePullConfig(run.Secrets[run.ImagePullSecret])
	if err != nil {
		return nil, err
	}
	hosts := map[string]bool{imageRegistry(run.Image): true}
	for _, service := range run.Services {
		hosts[imageRegistry(service.Image)] = true
	}
	for host := range auths {
		if !hosts[host] {
			return nil, errors.New("Image pull credentials may name only registries used by the step or its declared services.")
		}
	}
	return auths, nil
}

func imageRegistry(image string) string {
	first, _, slash := strings.Cut(image, "/")
	if slash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return first
	}
	return "docker.io"
}
