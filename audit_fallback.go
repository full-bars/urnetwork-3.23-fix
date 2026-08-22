//go:build !linux

package connect

func RunSystemAudit(skipDisk bool) (slowDisk bool, lowSpace bool) {
	return false, false
}
