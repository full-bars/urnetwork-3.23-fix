package urnettools

import "fmt"

// FreeBSD's rc.d scripts and sysrc commands are not OpenBSD rcctl scripts.
// Refuse lifecycle changes rather than installing an incompatible service.
func setAutoStart(_ Provider, _ bool) error {
	return fmt.Errorf("auto-start is not supported on OpenBSD; configure an rc.d service with rcctl")
}

func setAutoUpdateSchedule(_ Provider, _, _ string) error {
	return fmt.Errorf("auto-update scheduling is not supported on OpenBSD; configure cron manually")
}

func cleanupLifecycle(_ Provider) {}
