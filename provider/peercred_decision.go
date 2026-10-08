package main

// peerAllowed reports whether a peer UID may use the control socket: the
// provider's own UID or root.
//
// Root is allowed because it can already manage the provider by other means —
// it has filesystem access to the state dir and can signal the process — so
// refusing it would only stop systemd, cron and fleet scripts from working,
// without closing any hole. Everyone else is refused, which is what keeps an
// unprivileged local user from driving the socket.
//
// This is the shared decision behind every platform's peer-credential check,
// kept platform-independent so each platform's syscall wrapper cannot drift on
// what it decides.
func peerAllowed(peerUID, providerUID uint32) bool {
	return peerUID == providerUID || peerUID == 0
}
