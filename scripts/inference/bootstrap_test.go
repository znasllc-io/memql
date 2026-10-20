package inference_test

import (
	"os/exec"
	"testing"
)

func TestMLXMemoryBootstrap(t *testing.T) {
	output, err := exec.Command("python3", "test_mlx_bootstrap.py").CombinedOutput()
	if err != nil {
		t.Fatalf("MLX bootstrap: %v\n%s", err, output)
	}
}
