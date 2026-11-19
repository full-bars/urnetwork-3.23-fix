# Patched go-yamux v5.1.0

This is github.com/libp2p/go-yamux/v5 v5.1.0 with the upstream test files, the
spec and the benchmarks left out, and one change in `session.go`
(`incomingStream`). `refusal_test.go` is added here and covers the change.

Upstream returns an error when the memory manager refuses the span or the
initial stream window of an incoming stream. That error stops the receive loop
and closes the whole session. The patch resets only the refused stream and
carries on, the same way a stream past `MaxIncomingStreams` is handled. A refused SYN that reuses the ID of a registered stream is a protocol error and still returns the cause, as before, so its frame never reaches the existing stream.

`refusal_test.go` fails against the unpatched `session.go` and passes with the
patch. Run it from this directory with `go test -race .`. The gossip level test
is `TestGossipNodeMemoryRefusalKeepsExistingStream` in
`gossip/node_yamux_refusal_test.go`.

The code is MPL-2.0 (LICENSE) with BSD parts (LICENSE-BSD). The modified file
stays under MPL-2.0. Drop this directory and the `replace` in go.mod when
upstream fixes the behavior.
