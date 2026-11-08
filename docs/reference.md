# dagon reference

## Commands

| Command | Description |
|---|---|
| `dagon [init] [--workflow <ref>] [dir]` | Runs the setup wizard for the project in `dir`. The default `dir` is the current directory. |
| `dagon setup [--host <host>] [--user <user>] [--apex <domain>] [--app <name>] [dir]` | Prepares the VPS, the GitHub secrets, and the encrypted secret files with no questions. Each flag defaults to the value from the last run. `--app` defaults to the name of the `origin` repository. |
| `dagon rollback <slot> [sha]` | Starts the `rollback` workflow for `production` or `staging`. Without `sha`, the VPS picks the release before the one that serves now. |
| `dagon env edit <env>` | Opens `deploy/env/<env>.enc.env` in sops. |
| `dagon env list <env>` | Prints the variable names in `deploy/env/<env>.enc.env`. |
| `dagon server-script` | Prints the script that dagon installs on the VPS. |
| `dagon version` | Prints the version. |

`<env>` is `production`, `staging`, or `preview`.

`--workflow` sets the reusable workflow that the generated workflows call. The default is `thewatcherlab/dagon/.github/workflows/deploy.yml@main`.

An app name has 2 to 40 characters. It uses lowercase letters, digits, and dashes, and it starts with a letter.

## Files in the app repository

| File | Written | Purpose |
|---|---|---|
| `.github/workflows/staging.yml` | Always | Deploys `main` to staging. |
| `.github/workflows/production.yml` | Always | Deploys `v*` tags and releases to production. |
| `.github/workflows/preview.yml` | Always | Deploys a pull request on a `/deploy` comment, then comments the result. |
| `.github/workflows/preview-close.yml` | Always | Removes the preview when its pull request closes. |
| `.github/workflows/rollback.yml` | Always | Rolls `production` or `staging` back. Runs from the Actions tab or `dagon rollback`. |
| `Dockerfile` | If missing, and the framework is not **My own Dockerfile** | Builds the app image. |
| `.dockerignore` | If missing, same condition as `Dockerfile` | Keeps `.env*`, `deploy/env`, and `.git` out of the image. |
| `deploy/nginx.conf` | If missing, static sites only | Serves the build on port 3000 and falls back to `index.html`. |
| `deploy/build.env` | If missing | Public build-time values. Not encrypted. |
| `.sops.yaml` | On setup | Maps each `deploy/env/<env>.enc.env` to your age key and that environment's server key. |
| `deploy/env/<env>.enc.env` | On setup, if missing | Encrypted secrets for one environment. The first version holds `APP_ENV=<env>`. |

dagon overwrites the five workflows on every run. It never overwrites the other files. A second setup run re-encrypts the existing secret files for the current keys with `sops updatekeys`.

## Frameworks

| Wizard choice | Detected from `package.json` | Runtime image |
|---|---|---|
| Next.js | `next` | `node:<version>-alpine` running `node server.js`. Needs `output: "standalone"` in `next.config`. |
| Static site | `vite`, `astro`, `@vue/cli-service`, `parcel`, `react-scripts`, `@angular/core`, `@sveltejs/adapter-static` | `nginxinc/nginx-unprivileged:stable-alpine` serving the build output. |
| Node server | `nuxt`, `@remix-run/*`, `@react-router/serve`, `@sveltejs/adapter-node`, `@astrojs/node`, `express`, `fastify`, `hono` | `node:<version>-alpine` running `npm start`. |
| My own Dockerfile | No `package.json`, or a `Dockerfile` and no known framework | Your image. |

The default build output is `build` for `react-scripts` and `@sveltejs/adapter-static`, `dist/<app>/browser` for Angular, and `dist` for the rest.

| Lockfile | Install command |
|---|---|
| `package-lock.json` | `npm ci` |
| `pnpm-lock.yaml` | `pnpm install --frozen-lockfile` |
| `yarn.lock` | `yarn install --frozen-lockfile` |
| `yarn.lock` with `.yarnrc.yml` | `yarn install --immutable` |

The generated Dockerfile does not support Bun. Choose **My own Dockerfile** for a Bun project.

Every generated Dockerfile loads `deploy/build.env` into the environment before the build.

## Container contract

- The container serves HTTP on port 3000.
- `GET /` returns a 2xx or 3xx status. The VPS retries every second, 30 times.
- The container gets the decrypted variables of its environment as environment variables.

## Reusable workflow

`thewatcherlab/dagon/.github/workflows/deploy.yml` has two jobs. `build` picks the commit, then builds and pushes `ghcr.io/<owner>/<repo>:<sha>`. It skips the build if that tag exists. For a rollback with no `sha`, `build` asks the VPS for the commit. `deploy` sends the deploy, rollback, or remove command to the VPS over SSH.

| Input | Required | Default | Description |
|---|---|---|---|
| `slot` | Yes | | `staging`, `production`, or `pr-<number>`. |
| `sha` | For `deploy` | `""` | The 40-character commit SHA to deploy. For `rollback`, empty means the release before the one that serves now. |
| `command` | No | `deploy` | `deploy`, `rollback`, or `remove`. Only `staging` and `production` accept `rollback`. Only a `pr-<number>` slot accepts `remove`. |
| `dockerfile` | No | `Dockerfile` | Path of the Dockerfile in the repository. |
| `context` | No | `.` | Docker build context in the repository. |

| Secret | Value |
|---|---|
| `SSH_HOST` | The VPS host. |
| `SSH_USER` | `deploy`. |
| `SSH_PRIVATE_KEY` | The app's deploy key. |
| `SSH_KNOWN_HOSTS` | The VPS host keys. |

| Output | Description |
|---|---|
| `line` | The result line from the VPS. |

The calling workflow needs `contents: read` and `packages: write`. A rollback builds the image again only if its tag is gone from the registry. `preview.yml` also needs `pull-requests: write`.

The concurrency group is `dagon-<slot>`. A second deploy to the same slot waits for the first.

## Hostnames

| Slot | Hostname |
|---|---|
| `production` | `<domain>` |
| `staging` | `staging.<domain>` |
| `pr-<number>` | `pr-<number>.<domain>` |

## Server commands

GitHub Actions runs these through the SSH entry of the app's deploy key. The `sweep` command runs from the systemd timer.

| Command | Description |
|---|---|
| `serve <app> deploy <slot> <sha>` | Deploys `sha` to the slot. |
| `serve <app> deploy <slot> <sha> rollback` | Deploys `sha` and marks it as a rollback in the history. |
| `serve <app> previous <slot>` | Prints the commit that a rollback restores. Exits with 1 if there is none. |
| `serve <app> remove pr-<number>` | Removes a preview. |
| `sweep` | Removes each preview whose last deploy is more than 24 hours old, and prints `expired pr-<number> <url>` for each. |

## Paths on the VPS

| Path | Contents |
|---|---|
| `/usr/local/sbin/dagon` | The server script. |
| `/usr/local/lib/dagon/entry` | The SSH forced command for deploy keys. |
| `/etc/sudoers.d/dagon` | Lets `deploy` run `/usr/local/sbin/dagon serve` as root. |
| `/etc/dagon/apps/<app>/apex` | The app's domain. |
| `/etc/dagon/apps/<app>/image` | The app's image repository, such as `ghcr.io/acme/site`. |
| `/etc/dagon/apps/<app>/age/<env>.key` | The server age key for one environment. |
| `/etc/dagon/ports/<app>_<slot>` | The local port of one slot. |
| `/var/lib/dagon/<app>/<slot>/state` | The image ID and secrets hash of the running container. |
| `/var/lib/dagon/<app>/<slot>/history` | One `<sha> <unix time> <deploy or rollback>` line per published deploy, newest last. Keeps 20 lines. |
| `/var/lib/dagon/<app>/<slot>/deployed` | Its modification time is the time of the last deploy, published or unchanged. |
| `/etc/systemd/system/dagon-sweep.timer` | Runs `dagon sweep` every hour. |
| `/etc/systemd/system/dagon-sweep.service` | Runs `/usr/local/sbin/dagon sweep`. |
| `/etc/caddy/Caddyfile` | Imports `sites/*.caddy`. dagon backs up the previous file as `Caddyfile.bak.<timestamp>`. |
| `/etc/caddy/sites/<app>_<slot>.caddy` | The route of one slot. |
| `/run/dagon/` | Temporary decrypted files during a deploy. dagon deletes them when the deploy ends. |

Containers are named `<app>_<slot>`. A previous container is `<app>_<slot>_prev` during a switch. A container that failed its health check is `<app>_<slot>_failed`. Ports come from the range 20000 to 29999.

## Paths on your machine

| Path | Contents |
|---|---|
| `~/.config/dagon/<app>/settings.json` | Host, SSH user, and domain from the last setup. |
| `~/.config/dagon/<app>/actions_ed25519` | The app's deploy key. |
| `$SOPS_AGE_KEY_FILE`, or `$XDG_CONFIG_HOME/sops/age/keys.txt` | Your age key, if either variable is set. |
| `~/Library/Application Support/sops/age/keys.txt` | Your age key on macOS. |
| `~/.config/sops/age/keys.txt` | Your age key on Linux. |
