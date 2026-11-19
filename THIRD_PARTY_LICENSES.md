# Third-Party Licenses

This repository itself is licensed under the Mozilla Public License 2.0 (see
`LICENSE`). This file lists the third-party components that ship in, or link
into, builds from this repository, with their licenses, source locations and
license files.

## go-substrate-rpc-client (vendored substrate RPC client)

- **Component**: `github.com/centrifuge/go-substrate-rpc-client/v4`, the
  substrate RPC transport and codec used by the provider's substrate-based
  commands (the `provider fleet` group and the settlement-v2 chain reads).
- **Vendored at**: `third_party/go-substrate-rpc-client/`, taken from upstream
  `v4.2.2-0.20240919131012-e3b938563803` (commit
  `e3b938563803c6a71043c4e7ba2e0d27c400f514`), with the pending-reply transport
  patch recorded in `third_party/go-substrate-rpc-client/UPSTREAM.md` and
  `third_party/go-substrate-rpc-client/UPSTREAM.patch`; byte checksums of every
  imported file are in `third_party/go-substrate-rpc-client/UPSTREAM.sha256`.
- **Licenses**: Apache License 2.0 for the tree
  (`third_party/go-substrate-rpc-client/LICENSE`); the `gethrpc` package is
  LGPL-3.0, with its license files kept in place at
  `third_party/go-substrate-rpc-client/gethrpc/COPYING` (GPL-3.0),
  `third_party/go-substrate-rpc-client/gethrpc/COPYING.LESSER` (LGPL-3.0) and
  `third_party/go-substrate-rpc-client/gethrpc/AUTHORS`.
- **Static linking**: `gethrpc` is statically linked into provider binaries
  built from this repository. The complete source of the linked version is
  available in this repository at the vendored path above; the pristine
  upstream source is at
  <https://github.com/centrifuge/go-substrate-rpc-client>.

## npipe (in-tree Windows named-pipe adapter)

`third_party/npipe/` is not upstream code: it is this repository's minimal
in-tree replacement for the archived `gopkg.in/natefinch/npipe.v2` module
(which carries no license file), implemented on
`github.com/Microsoft/go-winio` (MIT), resolved through the module graph. It
exists so the vendored RPC client's Windows IPC transport builds without the
unlicensed upstream module.

## go-yamux (vendored stream multiplexer)

- **Component**: `github.com/libp2p/go-yamux/v5` v5.1.0.
- **Vendored at**: `third_party/go-yamux/`; `PATCH.md` describes the modification.
- **Licenses**: MPL-2.0 (`third_party/go-yamux/LICENSE`) and BSD
  (`third_party/go-yamux/LICENSE-BSD`).

The Source Code Form of the modified file is in the public source tree at `third_party/go-yamux/session.go` and in the release tag’s source archive.
