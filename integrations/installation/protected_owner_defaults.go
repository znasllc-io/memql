package installation

import (
	"encoding/json"
	"errors"
	"time"
)

// Normalize only qualified API defaults, on owned copies. Every other field
// still participates in the equality check, including undeclared live fields.
// In particular remote keys, vault identity and generated Secret templates
// cannot be hidden by the server applying defaults to their surrounding shape.
func protectedOwnerMaterial(value map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	copy, err := resourceJSON(encoded)
	if err != nil {
		return nil, err
	}
	switch resourceText(copy, "apiVersion") + "/" + resourceText(copy, "kind") {
	case "v1/Namespace":
		if len(resourceMap(copy, "spec")) == 0 {
			copy["spec"] = map[string]any{"finalizers": []any{"kubernetes"}}
		}
	case "external-secrets.io/v1/ExternalSecret":
		spec := resourceMap(copy, "spec")
		if spec == nil {
			return nil, errors.New("protected ExternalSecret has no specification")
		}
		ownerDefault(spec, "refreshInterval", "1h")
		interval, err := time.ParseDuration(resourceText(spec, "refreshInterval"))
		if err != nil || interval < 0 {
			return nil, errors.New("protected ExternalSecret refresh interval is invalid")
		}
		spec["refreshInterval"] = interval.String()
		target := resourceMap(spec, "target")
		if target == nil {
			if _, present := spec["target"]; present {
				return nil, errors.New("protected ExternalSecret target is invalid")
			}
			target = map[string]any{}
			spec["target"] = target
		}
		ownerDefault(target, "creationPolicy", "Owner")
		ownerDefault(target, "deletionPolicy", "Retain")
		if template := resourceMap(target, "template"); template != nil {
			ownerDefault(template, "engineVersion", "v2")
			ownerDefault(template, "mergePolicy", "Replace")
		}
		if data, present := spec["data"]; present {
			entries, ok := data.([]any)
			if !ok {
				return nil, errors.New("protected ExternalSecret data is invalid")
			}
			for _, item := range entries {
				entry, ok := item.(map[string]any)
				remote := resourceMap(entry, "remoteRef")
				if !ok || remote == nil {
					return nil, errors.New("protected ExternalSecret remote reference is invalid")
				}
				ownerDefault(remote, "conversionStrategy", "Default")
				ownerDefault(remote, "decodingStrategy", "None")
				ownerDefault(remote, "metadataPolicy", "None")
				ownerDefault(remote, "nullBytePolicy", "Ignore")
			}
		}
	case "external-secrets.io/v1/SecretStore":
		provider := resourceMap(resourceMap(copy, "spec"), "provider")
		if azure := resourceMap(provider, "azurekv"); azure != nil {
			ownerDefault(azure, "authType", "ServicePrincipal")
			ownerDefault(azure, "environmentType", "PublicCloud")
			ownerDefault(azure, "useAzureSDK", false)
		}
	}
	for _, key := range []string{"apiVersion", "kind", "metadata", "status"} {
		delete(copy, key)
	}
	return copy, nil
}

func ownerDefault(value map[string]any, key string, fallback any) {
	if _, present := value[key]; !present {
		value[key] = fallback
	}
}
