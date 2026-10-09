#!/usr/bin/env sh
# Installs agen + agen-host into $AGEN_INSTALL_DIR (default ~/.agen/bin) from a
# release archive (path or URL), or removes them:
#   sh install.sh <archive.tar.gz|URL> [--sha256 HEX]
#   sh install.sh --uninstall [--purge]   # --purge also deletes ~/.agen data
# The archive is checked against --sha256, or else <archive>.sha256 next to
# it (required for URLs).
set -eu
dir="${AGEN_INSTALL_DIR:-$HOME/.agen/bin}"
home="${AGEN_HOME:-$HOME/.agen}"

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

if [ "${1:-}" = "--uninstall" ]; then
  if [ -x "$dir/agen" ]; then "$dir/agen" down >/dev/null 2>&1 || true; fi
  rm -f "$dir/agen" "$dir/agen-host"
  rmdir "$dir" 2>/dev/null || true
  if [ "${2:-}" = "--purge" ]; then purge_home; fi
  echo "agen uninstalled"
  exit 0
fi
src="${1:?usage: install.sh <archive|URL> [--sha256 HEX] | --uninstall [--purge]}"
want=""
if [ "${2:-}" = "--sha256" ]; then want="${3:?--sha256 needs a value}"; fi
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
case "$src" in
  http://*|https://*)
    curl -fsSL "$src" -o "$tmp/agen.tar.gz"
    if [ -z "$want" ]; then
      curl -fsSL "$src.sha256" -o "$tmp/sum" || { echo "no $src.sha256: pass --sha256" >&2; exit 1; }
      want="$(cut -d' ' -f1 < "$tmp/sum")"
    fi
    src="$tmp/agen.tar.gz" ;;
  *)
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
case ":$PATH:" in *":$dir:"*) ;; *) echo "add $dir to PATH, then run: agen up" ;; esac
