//go:build !darwin && !linux

package selfupdate

import "errors"

func lockUpdate(string) (func(), error) {
	return nil, errors.New("Automatic updates are unsupported on this platform")
}
func updateLocked(string) bool { return false }
func writableDirectory(string) error {
	return errors.New("Automatic updates are unsupported on this platform")
}
func processAlive(int) bool   { return true }
func ownedByUser(string) bool { return false }
