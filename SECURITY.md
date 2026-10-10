# Security

## Reporting a vulnerability

Please do not open a public issue for a security problem. Report it privately
through GitHub: **Security → Report a vulnerability** on
[prjvvl/agen](https://github.com/prjvvl/agen/security/advisories/new).

Include what you found, how to reproduce it, and what an attacker could do
with it. You will get a reply within a week. Once a fix is released, the
advisory is published with credit to you, unless you prefer otherwise.

## Supported versions

Security fixes go to the `main` branch and the latest release.

## Scope

In scope: the engine and `agen-host`, the SDKs, the Hub, Nests (Manager and
Gateway), the CLI, the web UI, and the deployment files in `deploy/`.

Especially relevant: anything that lets a caller act outside its token's
scope or namespace, read another namespace's data, read secrets or the Hub's
keys, approve its own run, or make an agent repeat a side effect.

How to run Agen securely is described in [docs/security.md](docs/security.md).
