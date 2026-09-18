# devsys

`devsys` manages containerised development environments using Podman. Each
project gets its own isolated container, built from a shared `devsys-base`
image, with Claude Code, Codex, and GitLab integration baked in.

## Install

```sh
curl -fsSL https://github.com/katakalyst/devsys/releases/latest/download/install.sh | sh
```

On native Windows (PowerShell):

```powershell
irm https://github.com/katakalyst/devsys/releases/latest/download/install.ps1 | iex
```

Then:

```sh
devsys setup
devsys init <path>
devsys enter <project>
```

Podman is the only other thing `devsys` requires — install it first if you
don't already have it: https://podman.io/docs/installation
