#!/usr/bin/env bash
set -euo pipefail

version=v1.0.100
revision=ac22a60a23d66c37d43a7ec6d9e99b99a4320356

if [[ $# != 1 ]]; then
  echo "Usage: bash tests/lark_cli/build.sh OUTPUT_BINARY" >&2
  exit 2
fi

adapter_dir=$(cd -- "$(dirname -- "$0")/adapter" && pwd -P)
mkdir -p -- "$(dirname -- "$1")"
output=$(cd -- "$(dirname -- "$1")" && pwd -P)/$(basename -- "$1")
checkout=$(mktemp -d)
trap 'rm -rf -- "$checkout"' EXIT
git -c advice.detachedHead=false clone --quiet --depth 1 --branch "$version" https://github.com/larksuite/cli.git "$checkout"
if [[ $(git -C "$checkout" rev-parse HEAD) != "$revision" ]]; then
  echo "Unexpected lark-cli revision for $version" >&2
  exit 1
fi

cd -- "$checkout"
CGO_ENABLED=0 go build -trimpath -tags lark_cli_probe -ldflags "-X github.com/larksuite/cli/internal/build.Version=$version" \
  -o "$output" "$adapter_dir/main.go"
