package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/znasllc-io/memql/core/filetxn"
)

// This narrow native helper shares the Cockpit's kernel lock. Secrets travel
// over stdin, never argv or logs. It performs no cluster connection or login.
func runRegistryUpdate(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "registry-update accepts one JSON request on stdin")
		return 2
	}
	return registryUpdate(os.Stdin, os.Stdout, os.Stderr)
}

func registryUpdate(input io.Reader, output, diagnostic io.Writer) int {
	var request struct {
		Path     string  `json:"path"`
		Expected *string `json:"expected"`
		Data     string  `json:"data"`
	}
	decoder := json.NewDecoder(io.LimitReader(input, 8*filetxn.MaxBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		fmt.Fprintln(diagnostic, "invalid registry update request")
		return 2
	}
	err := filetxn.CompareAndSwap(context.Background(), request.Path, request.Expected, []byte(request.Data))
	if errors.Is(err, filetxn.ErrConflict) {
		_ = json.NewEncoder(output).Encode(map[string]bool{"written": false, "conflict": true})
		return 0
	}
	if err != nil {
		fmt.Fprintln(diagnostic, "registry update failed:", err)
		return 1
	}
	_ = json.NewEncoder(output).Encode(map[string]bool{"written": true})
	return 0
}
