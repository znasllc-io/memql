package models

import (
	"encoding/json"
	"math"
	"strings"
)

type ollamaTensor struct {
	Name  string   `json:"name"`
	Shape []uint64 `json:"shape"`
}

// Ollama exposes GGUF metadata and tensor shapes through /api/show. Routed
// expert tensors have a final expert dimension; shared experts, attention and
// embeddings remain fully active. Multiplying the whole model by used/experts
// would incorrectly discount those shared weights.
// Sources: https://github.com/ollama/ollama/blob/main/server/routes.go
// and https://github.com/ollama/ollama/blob/main/fs/ggml/gguf.go (tensor
// element sum supplies general.parameter_count). GGUF *_exps tensors
// place the expert dimension last.
// Unknown layouts and incomplete evidence produce no active-count claim.
func ollamaActiveParams(show ollamaShowResponse) int64 {
	total := ollamaParameterCount(show.ModelInfo["general.parameter_count"])
	arch, _ := show.ModelInfo["general.architecture"].(string)
	experts := ollamaParameterCount(show.ModelInfo[arch+".expert_count"])
	used := ollamaParameterCount(show.ModelInfo[arch+".expert_used_count"])
	if total <= 0 || arch == "" || experts <= 1 || used <= 0 || used > experts {
		return 0
	}
	var all, inactive int64
	for _, tensor := range show.Tensors {
		if len(tensor.Shape) == 0 {
			return 0
		}
		count := int64(1)
		for _, dim := range tensor.Shape {
			if dim == 0 || dim > uint64(total) || count > total/int64(dim) {
				return 0
			}
			count *= int64(dim)
		}
		if all > total-count {
			return 0
		}
		all += count
		if strings.Contains(tensor.Name, "_exps.") {
			if len(tensor.Shape) != 3 || tensor.Shape[2] != uint64(experts) {
				return 0
			}
			inactive += (count / experts) * (experts - used)
		}
	}
	if all != total || inactive <= 0 || inactive >= total {
		return 0
	}
	return total - inactive
}

// Counts are exact nonnegative integers bounded below float64's precision
// limit. NaN, infinities, fractions and overflowing metadata make no claim.
func ollamaParameterCount(v any) int64 {
	var n float64
	switch value := v.(type) {
	case float64:
		n = value
	case json.Number:
		var err error
		n, err = value.Float64()
		if err != nil {
			return 0
		}
	default:
		return 0
	}
	if math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 || n > 1e15 || math.Trunc(n) != n {
		return 0
	}
	return int64(n)
}
