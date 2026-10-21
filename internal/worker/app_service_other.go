//go:build !(darwin && computeruse)

package worker

import "errors"

// Only MemQL.app's executable -- the macOS computer-use build -- has a
// bundle to register services from.
func appServicesAvailable() bool { return false }

func appServiceStatus(string) int { return -1 }

func changeAppService(string, bool) error {
	return errors.New("this build has no MemQL.app services to change")
}
