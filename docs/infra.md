# Manage infra and secrets

This how-to covers the files under `.infra/`, how to edit encrypted env with `dagon env` or with sops and age, and how to host a second app on the same VPS. For path and command tables, see [Reference](reference.md). For why each environment has its own key, see [How dagon works](how-it-works.md).

## What's in `.infra/`

- `.infra/env/<env>.enc.env` holds encrypted runtime secrets for `production`, `staging`, or `preview`.
- `.infra/build.env` holds public build-time values such as `NEXT_PUBLIC_*` and `VITE_*`. It is not encrypted.
- `.infra/nginx.conf` serves static sites on port 3000 when present.

`.sops.yaml` at the repository root maps each encrypted env file to your age key and that environment's server key. For Written and Purpose columns, see [Files in the app repository](reference.md#files-in-the-app-repository).

## Edit secrets with `dagon env`

### Edit an environment

```bash
dagon env edit production
```

sops opens the decrypted file in `$EDITOR`. When you save, sops encrypts it again. Commit `.infra/env/<env>.enc.env`, then deploy that environment. Use the same command with `staging` or `preview`. Every pull request preview uses `preview`.

### List variable names

```bash
dagon env list production
```

### Rules for either tool

Put one secret value on each line. Put public build-time values in `.infra/build.env`, not in the encrypted files. You need an age key. The first setup creates one if it is missing. Back up that key. Without it you cannot edit the secrets. Paths are under [Find your age key](#find-your-age-key).

## Edit secrets with sops and age

Use this path when you have sops and your age key but not the `dagon` binary. The files and `.sops.yaml` are the same.

### Find your age key

dagon resolves the key in this order:

1. `$SOPS_AGE_KEY_FILE`, if that variable is set
2. `$XDG_CONFIG_HOME/sops/age/keys.txt`, if `$XDG_CONFIG_HOME` is set
3. `~/Library/Application Support/sops/age/keys.txt` on macOS
4. `~/.config/sops/age/keys.txt` otherwise

### Edit or decrypt with sops

Run these from the repository root, the same place `dagon env` uses:

```bash
sops .infra/env/production.enc.env
```

```bash
sops --decrypt .infra/env/production.enc.env
```

`dagon env edit` runs `sops` on the relative path. `dagon env list` decrypts and prints each name before `=`. The manual path uses the same layout.

### After you change a secret

Commit the encrypted file. Deploy that environment with a push, a tag, or `/deploy` as usual. Do not paste decrypted contents into GitHub secrets.

## Add another app on the same VPS

### Prerequisites

- A VPS that a prior dagon run already set up
- The other app's repository with `origin` and `gh` auth
- A different apex domain
- DNS `A` records for `<domain>` and `*.<domain>` that point at the same host. On Cloudflare, set both to DNS only.

### Run setup in the other repository

```bash
dagon
```

Or without questions:

```bash
dagon setup --host <same-host> --user <ssh-user> --apex other.example.com
```

### What is shared and what is per app

Shared across apps on the VPS:

- Docker
- Caddy
- the `deploy` user
- the sweep timer and script

Created per app:

- age keys under `/etc/dagon/apps/<app>/age/`
- the deploy SSH key and GitHub secrets
- Caddy site files
- containers
- `/etc/dagon/apps/<app>/{apex,image}`

### If install refuses the domain

If the apex already belongs to another app, install fails with:

```text
<apex> already belongs to app <other>
```

Choose another apex. Do not reuse another app's domain.
