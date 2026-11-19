#!/bin/sh
set -eu
dest=$1
mkdir -p "$dest/third_party/go-yamux" "$dest/third_party/go-substrate-rpc-client/gethrpc"
cp LICENSE THIRD_PARTY_LICENSES.md "$dest/"
cp third_party/go-yamux/LICENSE third_party/go-yamux/LICENSE-BSD "$dest/third_party/go-yamux/"
cp third_party/go-substrate-rpc-client/LICENSE "$dest/third_party/go-substrate-rpc-client/"
cp third_party/go-substrate-rpc-client/gethrpc/COPYING third_party/go-substrate-rpc-client/gethrpc/COPYING.LESSER third_party/go-substrate-rpc-client/gethrpc/AUTHORS "$dest/third_party/go-substrate-rpc-client/gethrpc/"
