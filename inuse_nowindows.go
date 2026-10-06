//go:build !windows

package codexcli

// firstFileInUse reports nothing held off Windows: unix lets npm unlink and
// replace a file a running process has open, and no unix plan asks for the
// check. See inuse_windows.go.
func firstFileInUse([]string) (string, error) { return "", nil }
