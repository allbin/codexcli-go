//go:build windows

package codexcli

import (
	"errors"
	"io/fs"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// firstFileInUse walks paths — files, or directory trees — and reports the
// first file another process holds in a way that stops npm replacing it.
// It returns (file, nil) for a held file, (file, err) when a file could not be
// probed, and ("", nil) when nothing is held. A path that does not exist is
// skipped: npm will create it, not replace it.
//
// Why this is needed, verified with npm 11.6.0 over codex 0.159.0: npm
// replaces a global package by renaming its directory aside, extracting the
// new one, and deleting the renamed copy. Windows allows the rename while
// codex.exe runs from inside the tree, so npm extracts the new version, fails
// to delete the running image (EPERM), prints a cleanup warning and exits 0 —
// leaving the old tree behind as `node_modules\@openai\.codex-<hash>`, and the
// running codex resolving its helper executables by a path that now holds the
// new version's. A file held open without delete sharing fails the rename
// instead, and npm rolls back. Neither is an update this package should start.
//
// The probe opens each file for write and delete with full sharing and closes
// it at once, writing nothing. A mapped image refuses the write access and a
// handle without delete sharing refuses the delete access, both as a sharing
// violation — the same two conditions npm trips over.
func firstFileInUse(paths []string) (string, error) {
	for _, root := range paths {
		var held string
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if p == root && errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				held = p
				return err
			}
			if d.IsDir() {
				return nil
			}
			inUse, err := fileHeld(p)
			if err != nil || inUse {
				held = p
				if err != nil {
					return err
				}
				return fs.SkipAll
			}
			return nil
		})
		if err != nil || held != "" {
			return held, err
		}
	}
	return "", nil
}

// fileHeld reports whether another process holds name against write or
// delete. A symlink is opened as itself, not followed: npm removes the link.
func fileHeld(name string) (bool, error) {
	u, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return false, err
	}
	h, err := windows.CreateFile(u,
		windows.FILE_WRITE_DATA|windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return true, nil
	}
	if err != nil {
		return false, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return false, windows.CloseHandle(h)
}
