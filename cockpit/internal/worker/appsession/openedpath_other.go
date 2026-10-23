//go:build !linux

package appsession

import "os"

// openedPath cannot ask the descriptor where it is here -- there is no
// /proc -- so the caller resolves the path again and checks it names the
// file already open.
func openedPath(*os.File) (string, bool) {
	return "", false
}
