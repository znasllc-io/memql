package main

import (
	"encoding/json"
	"io"
	"os"
	"testing"
)

func TestUpdateInvalidActionAlwaysReturnsJSONWithoutStarting(t *testing.T) {
	for _, args := range [][]string{{"--install", "--json"}, {"--check", "--install", "--version=1.2.3", "--json"}, {"--unknown", "--json"}, {"--status", "--version=1.2.3", "--json"}, {"--check", "unexpected", "--json"}} {
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		previous := os.Stdout
		os.Stdout = write
		code := handleUpdate(args)
		os.Stdout = previous
		_ = write.Close()
		body, _ := io.ReadAll(read)
		_ = read.Close()
		var reply struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
			Phase string `json:"phase"`
		}
		if err := json.Unmarshal(body, &reply); err != nil || code != 2 || reply.OK || reply.Error == "" || reply.Phase != "error" {
			t.Fatalf("%v: code %d body %s decode %v", args, code, body, err)
		}
	}
}
