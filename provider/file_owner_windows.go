//go:build windows

package main

import "os"

// sourceOwnerOf reports no owner: Windows files carry ACLs, not uid and gid,
// and a replacement inherits the folder's ACL.
func sourceOwnerOf(info os.FileInfo) (*fileOwner, bool) {
	return nil, false
}
