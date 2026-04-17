# deploy

A cross-platform Go CLI for managing Docker Compose deployments locally or over SSH, with a built-in terminal UI.

## Requirements

- Go 1.24+ to build from source
- `docker` with the Compose plugin

No `bash`, `whiptail`, `yq`, `envsubst`, or `perl` are required at runtime.

## Build

```bash
go build -o deploy .
```

On Windows:

```powershell
go build -o deploy.exe .
```

To build release binaries for Linux, macOS, and Windows from one machine:

```bash
./build.sh
```

To embed a release version in those binaries:

```bash
VERSION=v1.2.3 ./build.sh
```

Artifacts are written to `dist/`.
`dist/` is generated build output and is gitignored; it should not be treated as a release source directory in the repo.

## Usage

```bash
./deploy [profile]
./deploy [flags]
```

| Argument  | Description                                              |
|-----------|----------------------------------------------------------|
| _(none)_  | Sets `COMPOSE_PROFILES=dev`                              |
| `profile` | Sets `COMPOSE_PROFILES` to the given value, e.g. `prod`  |

Supported flags:

- `-h`, `--help` show help output and exit
- `-i`, `--install` install the current `deploy` binary into a user executable path and exit
- `-p`, `--profile` set `COMPOSE_PROFILES`
- `-u`, `--update` update `deploy` to the latest stable release and exit
- `-e`, `--env` preselect the env file
- `-f`, `--file` preselect the compose file
- `-l`, `--local` use local Docker even if `SSH_URI` is set in the env file (skips the deployment-method prompt)
- `--remote` use `SSH_URI` without prompting for the deployment method
- `--deploy-all` run the Deploy All Services action directly
- `--undeploy-all` run the Undeploy All Services action directly and keep volumes by default
- `--delete-volumes` with `--undeploy-all`, run `docker compose down -v`
- `--no-cache` rebuild images without using Docker build cache
- `-v`, `--version` show the binary version and exit

Examples:

```bash
./deploy --help
./deploy --install
./deploy -i
./deploy --update
./deploy prod
./deploy -p prod
./deploy -e .env.prod -f compose.yml
./deploy -p dev -f docker-compose.yaml -e liftorai.env --local
./deploy --deploy-all --local
./deploy --deploy-all --remote
./deploy --undeploy-all
./deploy --undeploy-all --delete-volumes --remote
./deploy --version
```

## Install

`deploy --install` and `deploy -i` both install the currently running binary into a user-scoped executable directory without `sudo`, then exit.

Released builds also check once per day for a newer stable GitHub Release when the CLI starts. If one is available, `deploy` offers to download and install it before continuing.

`deploy --update` and `deploy -u` skip the normal app flow, install the latest stable release immediately, and then exit.

The installer uses the first writable user-owned directory already on `PATH`. If none exists, it falls back to:

- Linux: `${XDG_BIN_HOME:-$HOME/.local/bin}`
- macOS: `$HOME/.local/bin`
- Windows: `%USERPROFILE%\bin`

If the fallback directory is not already on `PATH`, `deploy` prints the exact command to add it for the current shell session.

On launch the CLI opens an interactive terminal UI and walks through the same flow as the old script:

1. Select `.env` file
2. Select compose file
3. Validate compose metadata
4. Select deployment method
5. Run one action from the main menu

When `--deploy-all` or `--undeploy-all` is passed, the CLI skips the main menu and runs that action directly. If the selected env file sets `SSH_URI`, direct actions require either `--local` or `--remote`.

## Main Menu Actions

| Action | Description |
|--------|-------------|
| Deploy All Services | Runs `docker compose build` for source-build services or `docker compose pull` for image-only services, then starts containers with `up -d --force-recreate --remove-orphans` |
| Redeploy Service | Builds one selected source-build service or pulls one selected image-only service, then restarts it |
| Restart Service | Restarts one selected service without rebuilding |
| Undeploy Service | Stops and removes one selected service and its volumes |
| Service Shell | Opens an interactive `sh` shell inside one selected service |
| Service Logs | Prints logs for one selected service |
| Live Service Log Viewer | Follows logs for one selected service until interrupted |
| Database Management Interface | Starts the fixed `postgres-db-admin` container, opens `http://127.0.0.1:15432` in the browser, and stops the container when the action exits; remote deployments use an SSH tunnel |
| Undeploy All Services | Runs `docker compose down -v` |
| Undeploy All Services (Keep Volumes) | Runs `docker compose down` |
| Create External Networks | Creates any external Docker networks declared in the compose file |
| Host Shell | Opens an SSH shell to the selected remote host using the local `ssh` client |
| Host Shell (Unavailable) | Shown when SSH deployment is not available |

## Environment File

The selected env file must be a standard dotenv file. Supported syntax:

- `KEY=VALUE`
- blank lines
- comments
- single-quoted values
- double-quoted values

Shell syntax such as `export`, command substitution, or chained commands is rejected.

Key variables:

| Variable | Purpose |
|----------|---------|
| `PROJECT` | Required project identifier. Compose `name` must match it exactly. |
| `SSH_URI` | Optional remote target in `user@host`, `user@host:port`, `ssh://user@host`, or `ssh://user@host:port` form. |

Docker Compose performs interpolation directly via `--env-file`. The CLI validates the resolved model with `docker compose config` before any action is available.

## Release Artifacts

`build.sh` produces:

- `dist/linux/deploy`
- `dist/macos-amd64/deploy`
- `dist/macos-arm64/deploy`
- `dist/windows/deploy.exe`

## GitHub Releases

The repository includes a GitHub Actions workflow that publishes a Release when you push a tag that matches `v*`, for example:

```bash
git tag v0.1.0
git push origin v0.1.0
```

That workflow:

- runs `go test ./...`
- builds the release binaries with the tag embedded as the CLI version
- uploads direct binary assets for Linux, macOS, and Windows
- uploads `checksums.txt` so downloads can be verified and used by the built-in updater

## How It Works

1. The CLI reads the selected env file and checks that `PROJECT` is present.
2. The compose file is validated with `docker compose --env-file <env> -f <compose> config --quiet`.
3. The resolved compose model is loaded with `docker compose config --format json`.
4. A warning is shown if the raw compose file still contains a top-level `version` key.
5. The resolved compose `name` must exist and exactly match env `PROJECT`.
6. If `SSH_URI` is set, the user can choose between local deployment and the configured remote Docker host.
7. Deploy and redeploy actions use normal Docker build cache by default for services with `build:`. Services with `image:` and no `build:` are pulled instead of rebuilt. Pass `--no-cache` to the CLI when you want source rebuilds to ignore cached layers for that run.
8. When using a remote Docker daemon (`DOCKER_HOST=ssh://...`), file-based compose `secrets` are copied from **this machine** (paths relative to the compose file) into `/home/<ssh-user>/.deploy_secrets/<project>/` on the SSH host, then referenced through a temporary compose override. The copied files are set to `0644` so non-root containers can read Docker-mounted secrets.
9. All Compose actions run directly against the selected compose file and env file; no permanent modified compose file is written.
10. The Host Shell action uses the local `ssh` client, so it follows your existing SSH config, agent state, and identity selection.
11. The Database Management Interface action starts the fixed `postgres-db-admin` container directly with `docker start`, opens the browser with a per-launch session token, requests UI shutdown when the action exits, and then stops the container. It does not run Compose build/up. Remote mode disables SSH connection sharing and runs `ssh -N -S none -o ControlMaster=no -o ExitOnForwardFailure=yes -o ServerAliveInterval=5 -o ServerAliveCountMax=1 -L 15432:127.0.0.1:15432` so stale SSH mux processes do not keep the port open. If `postgres-db-admin` does not exist, deploy the Postgres stack first.
