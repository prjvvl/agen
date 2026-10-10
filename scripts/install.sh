#!/usr/bin/env sh
# Installs agen + agen-host into $AGEN_INSTALL_DIR (default ~/.agen/bin):
#   curl -fsSL https://prjvvl.github.io/agen/install.sh | sh   # latest release
#   AGEN_VERSION=v0.1.1 sh install.sh                          # a given release
#   sh install.sh <archive.tar.gz|URL> [--sha256 HEX]          # a given archive
#   sh install.sh --uninstall [--purge]   # --purge also deletes ~/.agen data
# Archives are checked against --sha256, or else <archive>.sha256 next to them
# (required for URLs). Unless AGEN_NO_MODIFY_PATH=1, the install directory is
# added to PATH in your shell's profile.
set -eu
repo="https://github.com/prjvvl/agen"
dir="${AGEN_INSTALL_DIR:-$HOME/.agen/bin}"
home="${AGEN_HOME:-$HOME/.agen}"
marker="# added by the agen installer"

# purge_home deletes AGEN_HOME only when it is clearly Agen's own directory.
purge_home() {
  [ -e "$home" ] || return 0
  case "$home" in ""|/|"$HOME"|"$HOME/") echo "refusing to delete $home (AGEN_HOME)" >&2; exit 1 ;; esac
  if [ ! -e "$home/agen.db" ] && [ ! -e "$home/local.json" ] && [ ! -d "$home/nest" ] && [ ! -d "$home/nests" ] \
     && [ -n "$(ls -A "$home" 2>/dev/null)" ]; then
    echo "refusing to delete $home: it does not look like an Agen home (no agen.db, local.json or nest dirs)" >&2
    exit 1
  fi
  rm -rf "$home"
}

profile() {
  case "$(basename "${SHELL:-sh}")" in
    zsh) echo "$HOME/.zshrc" ;;
    bash) if [ "$(uname -s)" = Darwin ]; then echo "$HOME/.bash_profile"; else echo "$HOME/.bashrc"; fi ;;
    fish) echo "$HOME/.config/fish/config.fish" ;;
    *) echo "$HOME/.profile" ;;
  esac
}

path_line() {
  case "$1" in
    *.fish) echo "fish_add_path \"$dir\" $marker" ;;
    *) echo "export PATH=\"$dir:\$PATH\" $marker" ;;
  esac
}

# latest_archive prints the release archive URL for this machine.
latest_archive() {
  case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) echo "unsupported OS $(uname -s); on Windows use install.ps1" >&2; exit 1 ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) echo "unsupported CPU $(uname -m)" >&2; exit 1 ;;
  esac
  if [ "$os-$arch" = darwin-amd64 ]; then
    echo "there is no release for Intel Macs yet; build from source: $repo#build-from-source" >&2
    exit 1
  fi
  version="${AGEN_VERSION:-}"
  if [ -z "$version" ]; then
    version="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$repo/releases/latest")"
    version="${version##*/}"
  fi
  case "$version" in v*) ;; *) echo "could not determine the latest release (got \"$version\")" >&2; exit 1 ;; esac
  echo "$repo/releases/download/$version/agen_${version}_${os}_${arch}.tar.gz"
}

if [ "${1:-}" = "--uninstall" ]; then
  if [ -x "$dir/agen" ]; then "$dir/agen" down >/dev/null 2>&1 || true; fi
  rm -f "$dir/agen" "$dir/agen-host"
  rmdir "$dir" 2>/dev/null || true
  rc="$(profile)"
  if [ -f "$rc" ] && grep -q "$marker" "$rc"; then
    grep -v "$marker" "$rc" > "$rc.agen-tmp" && cat "$rc.agen-tmp" > "$rc" && rm -f "$rc.agen-tmp"
  fi
  if [ "${2:-}" = "--purge" ]; then purge_home; fi
  echo "agen uninstalled"
  exit 0
fi
src="${1:-}"
want=""
if [ "${2:-}" = "--sha256" ]; then want="${3:?--sha256 needs a value}"; fi
if [ -z "$src" ]; then src="$(latest_archive)"; fi
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
case "$src" in
  http://*|https://*)
    echo "downloading $src"
    curl -fsSL "$src" -o "$tmp/agen.tar.gz"
    if [ -z "$want" ]; then
      curl -fsSL "$src.sha256" -o "$tmp/sum" || { echo "no $src.sha256: pass --sha256" >&2; exit 1; }
      want="$(cut -d' ' -f1 < "$tmp/sum")"
    fi
    src="$tmp/agen.tar.gz" ;;
  *)
    [ -f "$src" ] || { echo "no such archive: $src" >&2; exit 1; }
    if [ -z "$want" ] && [ -f "$src.sha256" ]; then want="$(cut -d' ' -f1 < "$src.sha256")"; fi ;;
esac
if [ -n "$want" ]; then
  if command -v sha256sum >/dev/null; then got="$(sha256sum "$src" | cut -d' ' -f1)"; else got="$(shasum -a 256 "$src" | cut -d' ' -f1)"; fi
  [ "$got" = "$want" ] || { echo "checksum mismatch for $src: got $got, want $want" >&2; exit 1; }
fi
tar -xzf "$src" -C "$tmp"
mkdir -p "$dir"
cp "$tmp"/agen_*/agen "$tmp"/agen_*/agen-host "$dir/"
chmod +x "$dir/agen" "$dir/agen-host"
echo "installed $("$dir/agen" version) to $dir"
case ":$PATH:" in
  *":$dir:"*) ;;
  *)
    rc="$(profile)"
    if [ "${AGEN_NO_MODIFY_PATH:-}" != 1 ]; then
      if ! { [ -f "$rc" ] && grep -q "$marker" "$rc"; }; then
        mkdir -p "$(dirname "$rc")"
        path_line "$rc" >> "$rc"
      fi
      echo "added $dir to PATH in $rc; open a new terminal, or run:"
    else
      echo "add $dir to PATH, for example:"
    fi
    echo "  export PATH=\"$dir:\$PATH\""
    ;;
esac
echo "next: agen up"
