# How dagon works

This page explains what happens during a deploy, and how dagon keeps one app, one environment, and one pull request from reaching another.

## A deploy, step by step

1. A push, a tag, or a `/deploy` comment starts a workflow in the app repository. That workflow calls the reusable workflow in this repository.
2. The `build` job builds the Docker image and pushes it to `ghcr.io/<owner>/<repo>:<sha>`. If that tag exists, the job skips the build. So a `v*` tag on a commit that staging already built deploys without a rebuild.
3. The `deploy` job connects to the VPS over SSH as `deploy`. It sends the registry token of the workflow run, then the encrypted secrets file of the slot's environment.
4. On the VPS, `dagon serve` takes a lock for the slot. It pulls the image with the token, then forgets the token.
5. It decrypts the secrets into a file under `/run/dagon/`, which is in memory.
6. If the same image with the same secrets already runs, it prints `unchanged` and stops.
7. Otherwise it renames the running container to `<app>_<slot>_prev` and starts the new one. It then requests `GET /` until the new container answers.
8. If the new container answers, dagon deletes the previous container, writes the Caddy route, reloads Caddy, and prints `published`. If the new container never answers, dagon keeps it as `<app>_<slot>_failed`, restarts the previous container, and prints `rejected`.
9. dagon deletes the decrypted file and removes the app's images that no container uses.

The previous container stops before the new one starts, so a slot is down for the few seconds that the new container takes to start. In exchange, the two containers never share the slot's port, and a failed deploy always returns to the container that worked.

## How rollback picks a release

After each published deploy, the VPS appends the commit to the slot's history and marks it as `deploy` or `rollback`. A rollback without a commit looks only at the `deploy` lines. It finds the commit that serves now, then takes the newest earlier commit that differs from it.

Take the releases A, B, and C, with C serving. The first rollback restores B. The second restores A, because the B line it wrote is a `rollback` line and not a release of its own. A new deploy D starts a new end of the list, and the next rollback restores C.

A secrets-only redeploy writes a second line with the same commit. A rollback skips it, so a rollback always changes the code.

## Why previews expire

Each preview holds a container, a port, a Caddy route, and an image on the VPS. Pull requests that stay open for weeks would fill the disk. The `dagon-sweep.timer` systemd timer runs `dagon sweep` every hour. The sweep removes each preview whose `deployed` file is more than 24 hours old. Both a published and an unchanged deploy touch that file, so any `/deploy` comment resets the clock.

## Why each environment has its own key

Each secrets file has two recipients. One is your age key, and the other is a server key. The VPS has three server keys per app, one each for `production`, `staging`, and `preview`. `.sops.yaml` maps each file to its key.

A pull request can change any file in the repository, including the workflows. Suppose a pull request copies `production.enc.env` over `preview.enc.env`. The VPS decrypts a preview with the preview key, which cannot open a file encrypted for the production key. The deploy fails with `rejected` and the production values stay encrypted.

GitHub never holds a key that decrypts anything. The workflow only carries the encrypted file to the VPS.

## Why the deploy user cannot do more than deploy

The `deploy` user on the VPS is not in the `docker` group, so it cannot start containers itself. Each app has its own SSH key in `deploy`'s `authorized_keys`, and each key has a forced command:

```text
command="/usr/local/lib/dagon/entry <app>",restrict ssh-ed25519 ...
```

The app name comes from that line, not from the workflow. The entry script passes the rest of the SSH command to `sudo /usr/local/sbin/dagon serve <app>`, which accepts only `deploy <slot> <sha>` and `remove pr-<number>`, and checks each argument against a fixed pattern. A leaked deploy key of one app can deploy or remove that app's slots. It cannot reach another app, open a shell, or run any other command.

## Why the VPS keeps no GitHub credentials

The workflow passes `github.token` of its run with each deploy. That token can read the app's images and expires when the run ends. The VPS logs in to the registry with a temporary Docker config for that one pull, and deletes the config right after. No long-lived token sits on the VPS.

## Why the VPS runs a shell script

The server side is one Bash script with no dependencies beyond Docker, sops, Caddy, curl, and coreutils. The `dagon` binary embeds the script, and setup copies it to the VPS over the same SSH connection. The VPS needs no Go toolchain and no release download, and `dagon server-script` shows exactly what runs as root.
