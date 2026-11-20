//go:build !unix

package fsnotify

import "os"

// No portable file ID available; dedup falls back to os.SameFile.
func fileIDOf(fi os.FileInfo) (fileID, bool) { return fileID{}, false }
