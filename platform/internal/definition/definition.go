// Package definition computes content digests for bundle files. The algorithm
// must match agen-engine's bundle::digest_files (shared golden vector).
package definition

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strings"
)

var ignoredDirs = map[string]bool{".git": true, "node_modules": true, "__pycache__": true, ".venv": true}

// Ignored reports whether a bundle-relative path is excluded from bundles.
func Ignored(rel string) bool {
	parts := strings.Split(rel, "/")
	for _, p := range parts {
		if ignoredDirs[p] {
			return true
		}
	}
	name := parts[len(parts)-1]
	return name == ".DS_Store" || strings.HasSuffix(name, "~") ||
		strings.HasSuffix(name, ".swp") || strings.HasSuffix(name, ".pyc")
}

// Digest hashes files (path -> bytes) in byte order of path as
// path || 0x00 || u64le(len) || bytes. Ignored paths are skipped.
func Digest(files map[string][]byte) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		if !Ignored(p) {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths) // Go string comparison is byte-wise.
	h := sha256.New()
	var n [8]byte
	for _, p := range paths {
		h.Write([]byte(p))
		h.Write([]byte{0})
		binary.LittleEndian.PutUint64(n[:], uint64(len(files[p])))
		h.Write(n[:])
		h.Write(files[p])
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
