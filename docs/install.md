# Install

Agen is two programs, `agen` (the CLI, Hub and Nest) and `agen-host` (which
runs an agent). The installer puts both in `~/.agen/bin` and adds that
directory to your `PATH`.

## macOS and Linux

```sh
curl -fsSL https://prjvvl.github.io/agen/install.sh | sh
```

Builds exist for Linux on x86-64 and arm64 (glibc 2.34 or newer: Ubuntu
22.04, Debian 12, RHEL 9 and later) and for Apple-silicon Macs. On an Intel
Mac, [build from source](development.md).

## Windows

In PowerShell:

```powershell
irm https://prjvvl.github.io/agen/install.ps1 | iex
```

The build is for x86-64; on Windows on Arm it runs under emulation.

## Check it

Open a new terminal (so it sees the new `PATH`), then:

```sh
agen version
```

It prints the version, for example `agen v0.1.1`. If the shell says
`agen: command not found`, the new `PATH` is not loaded yet: open another
terminal, or add `~/.agen/bin` to `PATH` yourself.

Next: [Getting started](getting-started.md).

## Options

| Variable | Effect |
|---|---|
| `AGEN_VERSION` | Install that release (`v0.1.1`) instead of the latest. |
| `AGEN_INSTALL_DIR` | Install somewhere other than `~/.agen/bin`. |
| `AGEN_NO_MODIFY_PATH=1` | Leave your shell profile (Windows: your user `PATH`) alone. |

For example `curl -fsSL https://prjvvl.github.io/agen/install.sh | AGEN_VERSION=v0.1.1 sh`,
or in PowerShell `$env:AGEN_VERSION = "v0.1.1"` before the command above.

The installer downloads the archive for your machine from
[GitHub Releases](https://github.com/prjvvl/agen/releases) and checks it
against its published SHA-256 before installing. To install an archive you
downloaded or built yourself, pass it to the script: `sh install.sh
agen_v0.1.1_linux_amd64.tar.gz` or `.\install.ps1 agen_v0.1.1_windows_amd64.zip`.

## Upgrade

Run the install command again. Stop a running local fleet first
(`agen down`); on Windows a running `agen.exe` cannot be replaced.

## Uninstall

```sh
curl -fsSL https://prjvvl.github.io/agen/install.sh | sh -s -- --uninstall
```

```powershell
& ([scriptblock]::Create((irm https://prjvvl.github.io/agen/install.ps1))) -Uninstall
```

This removes the programs and the `PATH` entry, and keeps your data in
`~/.agen`. Add `--purge` (`-Purge`) to delete that too.

## What it creates

| Path | What |
|---|---|
| `~/.agen/bin` | `agen` and `agen-host`. |
| `~/.agen` | Created by `agen up`: the local store (`agen.db`), the admin token (`local.json`), and the Nest's data (`nest/`). Delete it (after `agen down`) to start over. |

`agen up` listens on `127.0.0.1:7070` (Hub, web UI, API and MCP) and
`127.0.0.1:7071` (the A2A Gateway); change them with `--listen` and
`--gateway-listen`. Agen installs no services and starts nothing at login.

## SDKs

The Python, Node.js and Go SDKs are not on a package registry yet; they are
built from the repository and need a Rust toolchain. See
[Embed an agent](embed.md).
