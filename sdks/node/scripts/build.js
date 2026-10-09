// Build the native addon with cargo and copy it to ./agen.node.
const { execFileSync } = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');

const release = process.argv.includes('--release');
const root = path.join(__dirname, '..');
execFileSync('cargo', ['build', ...(release ? ['--release'] : [])], { cwd: root, stdio: 'inherit' });
const dir = path.join(root, 'target', release ? 'release' : 'debug');
const lib = { win32: 'agen_node.dll', darwin: 'libagen_node.dylib' }[process.platform] || 'libagen_node.so';
fs.copyFileSync(path.join(dir, lib), path.join(root, 'agen.node'));
console.log(`built ${path.join(root, 'agen.node')}`);
