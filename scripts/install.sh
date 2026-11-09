#!/bin/sh
# Install the docket binary to ~/.local/bin (no sudo).
#   curl -fsSL https://raw.githubusercontent.com/blobspire/docket/main/scripts/install.sh | sh
# Set DOCKET_VERSION=v0.1.0 to pin a release; default is the latest.
set -eu
repo=blobspire/docket
dest="${DOCKET_INSTALL_DIR:-$HOME/.local/bin}"
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) echo "docket: unsupported architecture $arch" >&2; exit 1 ;;
esac
case "$os" in linux | darwin) ;; *) echo "docket: unsupported OS $os" >&2; exit 1 ;; esac
version="${DOCKET_VERSION:-}"
if [ -z "$version" ]; then
	version=$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
fi
[ -n "$version" ] || { echo "docket: couldn't find a release" >&2; exit 1; }
name="docket-$os-$arch"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
base="https://github.com/$repo/releases/download/$version"
curl -fsSL -o "$tmp/$name" "$base/$name"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS"
want=$(grep " $name\$" "$tmp/SHA256SUMS" | cut -d' ' -f1)
if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$tmp/$name" | cut -d' ' -f1); else got=$(shasum -a 256 "$tmp/$name" | cut -d' ' -f1); fi
[ -n "$want" ] && [ "$want" = "$got" ] || { echo "docket: checksum mismatch for $name" >&2; exit 1; }
mkdir -p "$dest"
install -m 0755 "$tmp/$name" "$dest/docket"
echo "docket $version installed to $dest/docket"
case ":$PATH:" in *":$dest:"*) ;; *) echo "Add $dest to your PATH." ;; esac
