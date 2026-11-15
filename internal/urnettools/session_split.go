package urnettools

// classicalSessions is the number of clients that are not on a post-quantum
// session: every client except the ones counted as post-quantum.
//
// The provider counts a session only after an end to end key exchange has
// finished. A client that never starts one, such as an older client or one that
// falls back to plaintext, is in neither of the provider's two counts, so the
// provider's classical count alone is almost always 0. The counted classical
// sessions are kept as a floor so the figure never drops below what the
// provider did count.
func classicalSessions(clients, pqe, counted int) int {
	return max(clients-pqe, counted, 0)
}
