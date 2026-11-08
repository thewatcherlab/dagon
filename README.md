# dagon

Deploy a frontend from GitHub to your own VPS.

Each app gets three kinds of deploys:

- A pull request preview at `https://pr-<number>.<domain>` when someone comments `/deploy`. The preview lasts 24 hours after its last deploy.
- Staging at `https://staging.<domain>` on every push to `main`.
- Production at `https://<domain>` on every `v*` tag.

GitHub Actions builds a Docker image for each commit and pushes it to the GitHub container registry. The VPS pulls the image, runs it as a container, and serves it over HTTPS with Caddy. Secrets live in your repository, encrypted with sops and age. Only you and the VPS can decrypt them. One VPS can host many apps.

dagon writes a Dockerfile for Next.js, for static sites, and for Node servers. Any other project works if it brings its own Dockerfile.

## Install dagon

```bash
go install github.com/thewatcherlab/dagon/cmd/dagon@latest
brew install gh sops age
```

## Prepare the server and the domain

You need these before you run dagon:

- A Debian or Ubuntu VPS.
- An SSH user on the VPS that logs in with your SSH key and can run `sudo -n true`.
- Two DNS `A` records that point `<domain>` and `*.<domain>` at the VPS. On Cloudflare, set both records to **DNS only**, so that requests reach Caddy on the VPS directly.
- The app's repository on GitHub, with `origin` set to it. Sign in with `gh auth login`.

## Set up an app

1. In the app's repository, run dagon:

   ```bash
   dagon
   ```

2. Check the answers. dagon fills them in from your project and from the last run.

   | Question | Default |
   |---|---|
   | App name | The name of the `origin` repository |
   | Framework | Detected from `package.json` |
   | Build output | Static sites only. Detected from the framework, such as `dist` |
   | Node.js version | `.nvmrc`, `.node-version`, `engines.node`, or `22` |
   | Set up the VPS | Yes. Choose No to write only the files |
   | VPS host, SSH user, Domain | The values from the last run |

   Use the up and down arrows to move between questions. Use the left and right arrows to change a choice.

3. Press Enter on the last question. dagon lists each file it will create, update, or keep, and what it will change on the VPS.

4. Press Enter to apply. dagon shows each step as it runs. The first run on a new VPS takes a few minutes because it installs Docker, Caddy, and sops.

5. Commit the files that dagon lists at the end, and push to `main`. The push deploys staging.

You can run dagon again at any time. It keeps your Dockerfile and any other file that you changed. It also keeps the existing keys, so the GitHub secrets stay valid.

To set up from a script, with no questions:

```bash
dagon setup --host 203.0.113.10 --user ubuntu --apex example.com
```

`dagon setup` changes the VPS, the GitHub secrets, and the encrypted secret files. It does not write the workflows or the Dockerfile.

## Deploy

- To deploy a pull request, comment `/deploy` on it. The comment must contain only `/deploy`, and its author must be an owner, a member, or a collaborator of the repository. Each later `/deploy` updates the same URL.
- A preview stops 24 hours after its last `/deploy`, and the VPS deletes its container, route, and image. Comment `/deploy` again to bring it back. When the pull request closes, the `preview-close` workflow removes the preview at once.
- To deploy staging, push to `main`.
- To deploy production, push a tag that starts with `v`, or publish a release with such a tag.

Each deploy prints one line. A preview also posts the line as a comment on its pull request:

```text
published pr-12 0123456789abcdef0123456789abcdef01234567 https://pr-12.example.com
```

The first word is the result:

| Word | Meaning |
|---|---|
| `published` | The new container passed its health check and now serves the URL. |
| `unchanged` | The same image with the same secrets already serves the URL. Nothing restarted. |
| `rejected` | The deploy failed. The previous container still serves the URL. |
| `removed` | The preview container and its route are gone. |

## Roll back a release

To put staging or production back one release, run this in the app's repository:

```bash
dagon rollback production
```

dagon starts the `rollback` workflow, which redeploys the release before the one that serves now. Run it again to go back one more release. A rollback never returns to the release that it replaced. To move forward again, deploy as usual or roll back to a named commit:

```bash
dagon rollback production 0123456789abcdef0123456789abcdef01234567
```

You can also run the `rollback` workflow from the **Actions** tab on GitHub.

A rollback restores the image and the secrets file of that commit. If you changed a secret since that release, the rollback brings back the old value. The VPS remembers the last 20 deploys of each environment.

## Edit secrets

```bash
dagon env edit production
```

sops opens the decrypted file in `$EDITOR` and encrypts it again when you save. Commit the file and deploy. The container gets the new values on the next deploy of that environment.

To print only the variable names:

```bash
dagon env list production
```

The environments are `production`, `staging`, and `preview`. Every pull request preview uses `preview`.

Each value must fit on one line. Put public build-time values, such as `NEXT_PUBLIC_*` and `VITE_*`, in `deploy/build.env`. dagon does not encrypt that file, because those values end up in the browser bundle.

If you have no age key, dagon creates one on the first run. On macOS the key is in `~/Library/Application Support/sops/age/keys.txt`. Back up that file. Without it, you cannot edit the secrets.

## Bring your own Dockerfile

Choose **My own Dockerfile** in the wizard and give its path. The image must follow two rules:

- It serves HTTP on port 3000.
- `GET /` returns a 2xx or 3xx status within about 30 seconds of the start.

## Add another app to the same VPS

Run dagon in the other app's repository and give it a different domain. dagon refuses a domain that another app on the VPS already uses.

## Debug a failed deploy

If a new container fails its health check, dagon stops it, keeps it as `<app>_<slot>_failed`, and restarts the previous container. To read its output, run this on the VPS:

```bash
sudo docker logs <app>_<slot>_failed
```

If a `dagon` step fails on your machine, dagon prints the step and the last lines of the error. Fix the cause and run dagon again.

## Learn more

- [Reference](docs/reference.md) lists the commands, the files dagon writes, the workflow inputs, and the paths on the VPS.
- [How dagon works](docs/how-it-works.md) explains the deploy flow and how dagon keeps apps and secrets apart.

## Develop dagon

```bash
go test ./...
bash internal/server/dagon_test.sh
go run ./cmd/dagon
```
