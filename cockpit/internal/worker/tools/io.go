package tools

import (
	"errors"
	"os"
	"os/user"
)

// errNotExist is the sentinel returned by readFile when the path
// is missing. Lets callers distinguish from real I/O errors.
var errNotExist = errors.New("policy: file does not exist")

func readFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errNotExist
		}
		return nil, err
	}
	return data, nil
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	if u, err := user.Current(); err == nil {
		return u.HomeDir
	}
	return ""
}
