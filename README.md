# deploy

An interactive terminal UI script for managing Docker Compose deployments — locally or over SSH.

## Requirements

- `bash` 4.3+ (uses namerefs)
- `docker` with the Compose plugin
- `whiptail` — for interactive menus (`sudo apt install whiptail`)
- `yq` — for parsing compose files (`sudo snap install yq`)
- `envsubst` — for env var substitution (typically pre-installed via `gettext`)

## Usage

```bash
./deploy [profile]
```

| Argument  | Description                                                 |
|-----------|-------------------------------------------------------------|
| _(none)_  | Sets `COMPOSE_PROFILES=dev`                                 |
| `profile` | Sets `COMPOSE_PROFILES` to the given value (e.g. `prod`)    |

On launch the script walks you through three interactive prompts:

1. **Select `.env` file** — scans the current directory (up to 2 levels deep) for `*.env` and `.env.*` files; prefers `./.env`
2. **Select compose file** — scans for both `*.yml` and `*.yaml` files; prefers `./docker-compose.yml`, `./docker-compose.yaml`, `./compose.yml`, then `./compose.yaml`
3. **Select deployment method** — local Docker daemon, a pre-configured `SSH_URI` from your env file, or a custom SSH connection

## Main Menu Actions

| Action                    | Description                                                         |
|---------------------------|---------------------------------------------------------------------|
| Deploy All Services       | Builds all images and starts all containers (`up -d --force-recreate --remove-orphans`) |
| Redeploy Service          | Rebuilds and restarts a single selected service                     |
| Restart Service           | Restarts a single selected service (no rebuild)                     |
| Undeploy Service          | Stops and removes a single service container and its volumes        |
| Service Shell             | Opens an interactive `sh` shell inside a selected container         |
| Service Logs              | Prints logs for a selected service                                  |
| Live Service Log Viewer   | Tails live logs for a selected service (`Ctrl+C` to stop)           |
| Undeploy All Services     | Runs `docker compose down -v` to remove all containers and Compose-managed volumes |
| Undeploy All Services (Keep Volumes) | Runs `docker compose down` to remove containers and networks but keep volumes |
| Create External Networks  | Creates any external Docker networks declared in the compose file   |
| Host Shell                | Opens an SSH session to the remote host (only shown for SSH targets)|

## Environment File

The `.env` file is sourced with `set -a` so all variables are automatically exported. Key variables:

| Variable   | Purpose                                                          |
|------------|------------------------------------------------------------------|
| `SSH_URI`  | Remote host in `user@host` format; enables SSH deployment options |
| `PROJECT`  | Required project identifier; the script shows a warning and exits if it is missing |

Variables defined in the `.env` file are substituted into a temporary copy of the compose file before any Docker Compose command runs. The renderer supports `${VAR}`, `${VAR:?message}`, and `${VAR:-default}` forms, errors out if required values are missing, and reports missing env/compose candidates or cancelled selections cleanly instead of exiting abruptly.

## How It Works

1. The selected `.env` file is sourced into the shell environment.
2. The selected compose file is processed by `envsubst`, producing a temp file (`.docker-compose-XXXXXX.yml`) alongside the original so relative build context paths resolve correctly.
3. All `docker compose` commands run against the temp file. It is automatically deleted on exit.
4. When an SSH deployment target is selected, `DOCKER_HOST` is set to the SSH URI so Docker commands are forwarded to the remote daemon transparently.
