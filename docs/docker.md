# Self-hosting with Docker

This guide helps you set up and run Gost with Docker Compose.

## Setup

### Install Docker

- Install Docker Engine by following the official guide
- Start the Docker service on your machine

### Configure your environment

All config lives in one `.env` file next to `docker-compose.yaml`. Copy `.env.example` to `.env` and edit it.
Compose reads it, and the app containers get all of it. Inside Docker, Compose sets only the container hosts, ports and the upload folder.

These are the values to set for a deployment:

| Variable | Default | Description |
|----------|---------|-------------|
| `WEB_CLIENT_DOMAIN` | `localhost` | Your domain. Traefik routes it, and email links use it. |
| `APP_ENVIRONMENT` | `local` | `production` on a server with Traefik. `development` on an ARM board, which has no HTTPS. |
| `TRAEFIK_EMAIL` | `support@example.com` | Email for Let's Encrypt. |
| `DATABASE_USER` | `postgres` | Database user. |
| `DATABASE_PASSWORD` | `postgres` | Database password. |
| `DATABASE_NAME` | `gost` | Database name. |
| `REDIS_PASSWORD` | empty | Redis password. Required in production. |
| `SEED_ROOT_USER` | `root@local.host` | Email of the first user. |
| `SEED_ROOT_PASSWORD` | `password` | Password of the first user. |
| `APP_ALLOW_SIGNUP` | `false` | Lets anyone who reaches the app make an account. |
| `MAILER_HOST` | empty | SMTP server for email links. Empty writes the links to the API log. |
| `GRAFANA_PASSWORD` | `admin` | Grafana admin password. |
| `WEB_CLIENT_PORT` | `5173` | ARM only: the host port of the app. |
| `GRAFANA_PORT` | `3000` | ARM only: the host port of Grafana. |

> ⚠️ **Warning:** The defaults are not safe. Set your own passwords before you deploy publicly.

### Images

`docker-compose.yaml` pulls the published images, `nootey/gost` and `nootey/gost-client`.
Each release builds them for amd64 and arm64. `docker-compose.build.yaml` builds them from source instead.

## Run on a server

Traefik gets a TLS certificate from Let's Encrypt, so your domain must point to the server, and ports 80 and 443 must be open.
These commands build the images from source.

For the first time setup, you must run the migrations and seed the root user.
The seed makes the user from `SEED_ROOT_USER` and `SEED_ROOT_PASSWORD`, and skips a user that already exists.

```sh
make docker-migrate
make docker-seed
```

To start all services, including the observability stack, use:

```sh
make docker-up
```

Gost is then available at `https://<your-domain>`.
The observability stack (Prometheus, Loki, Grafana) is included by default.
Grafana is available at `https://grafana.<your-domain>`.

To restart or stop all services, use:

```sh
make docker-restart
make docker-down
```

## Run on an ARM board

For a Raspberry Pi or another ARM board. The board runs no Traefik.
The client serves the app on `WEB_CLIENT_PORT`, and Grafana runs on `GRAFANA_PORT`.
Set `APP_ENVIRONMENT=development`: production needs HTTPS for its cookies.

```sh
make docker-arm-migrate
make docker-arm-seed
make docker-arm-up
```

Gost is then available at `http://<WEB_CLIENT_DOMAIN>:<WEB_CLIENT_PORT>`.
Each command pulls the latest published images, so `make docker-arm-up` also updates the app.
To build on the board instead, add `build=1`, for example `make docker-arm-up build=1`.

To restart or stop all services, use:

```sh
make docker-arm-restart
make docker-arm-down
```

## Users

Sign up is off by default, so only the root user can log in.
To add another user, set `APP_ALLOW_SIGNUP=true`, start the app again, and make the account on the sign up page.
Then set it back to `false` and start the app again.

The new account must confirm its email before it can log in.
Set the `MAILER_*` values to send the email. With no mailer, the API writes the link to its log: `docker compose logs api`.

## Configuration

### File uploads

Uploads are stored on local disk. Docker keeps them in the `uploads` named volume.
The database keeps the owner, name, size and type of each file.

Files go up in 32 MB chunks over the [tus](https://tus.io) protocol at `/api/uploads/`, so a file of any size works, and a broken upload goes on from where it stopped.
Uploads in progress wait in `UPLOAD_DIR/.partial/`. A job removes the ones that get no new bytes for 24 hours.

| Variable | Default | Description |
|----------|---------|-------------|
| `UPLOAD_DIR` | `./storage/uploads` | Folder for uploaded files. In Docker, Compose sets it to the `uploads` named volume. |
| `UPLOAD_TIMEOUT_SECONDS` | `300` | Time limit for one upload request, which carries one chunk. |

### File downloads

One file downloads as it is. A folder or many items download as one zip.
A background job builds the zip, and the app keeps it for 24 hours.

| Variable | Default | Description |
|----------|---------|-------------|
| `DOWNLOAD_TIMEOUT_SECONDS` | `3600` | Time limit for one download request. |
| `DOWNLOAD_MAX_ZIP_MB` | `10240` | Largest zip the app builds. |
| `DOWNLOAD_ZIP_CHECK_SECONDS` | `1` | How often a zip job checks that its download still exists. |
