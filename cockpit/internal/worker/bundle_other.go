//go:build !darwin || !computeruse

package worker

func currentWorkerBundle() (string, string) { return "", "" }
