package pipelines

import "slices"

// SecretNames is the complete owner-consent set, including credentials which
// never become command environment variables. Each call owns its result.
func (s StepSpec) SecretNames() []string { return secretNames(s.Secrets, s.ImagePullSecret) }
func (s Step) SecretNames() []string     { return secretNames(s.Secrets, s.ImagePullSecret) }

func secretNames(environment []string, imagePull string) []string {
	result := slices.Clone(environment)
	if imagePull != "" && !slices.Contains(result, imagePull) {
		result = append(result, imagePull)
	}
	return result
}
