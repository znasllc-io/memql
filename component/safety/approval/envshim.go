package approval

import "os"

// getenv exists as a thin wrapper around os.Getenv so the envLookup
// var (which tests override) has something stable to point at. Don't
// inline os.Getenv into envLookup -- doing so would mean tests have
// to swap the entire os package, which they can't.
func getenv(k string) string { return os.Getenv(k) }
