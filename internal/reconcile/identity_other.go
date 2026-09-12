//go:build !unix

package reconcile

import "os"

// Reporting remains available, but deletion rejects this missing identity.
func systemIdentity(os.FileInfo) string { return "" }
