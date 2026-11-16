# Self-hosting with Docker

This guide helps you set up and run Gost with Docker Compose.

## Setup

### Install Docker

- Install Docker Engine by following the official guide
- Start the Docker service on your machine

### Configure your environment

Docker Compose reads a `.env` file next to `docker-compose.yaml`.
Traefik gets a TLS certificate from Let's Encrypt, so your domain must point to the server, and ports 80 and 443 must be open.

| Variable | Default | Description |
|----------|---------|-------------|
| `HOST` | `localhost` | Your domain. |
| `TRAEFIK_EMAIL` | `support@example.com` | Email for Let's Encrypt. |
| `POSTGRES_USER` | `postgres` | Database user. |
| `POSTGRES_PASSWORD` | `postgres` | Database password. |
| `POSTGRES_DB` | `gost` | Database name. |
| `GRAFANA_PASSWORD` | `admin` | Grafana admin password. |

> ⚠️ **Warning:** The defaults are not safe. Set your own passwords before you deploy publicly.

### Run the app

For the first time setup, you must run the migrations and seed the root user.

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

To stop all services, use:

```sh
make docker-down
```

## Configuration

### File uploads

Uploads are stored on local disk. Docker keeps them in the `uploads` named volume.
The database keeps the owner, name, size and type of each file.

Files go up in 32 MB chunks over the [tus](https://tus.io) protocol at `/api/uploads/`, so a file of any size works, and a broken upload goes on from where it stopped.
Uploads in progress wait in `UPLOAD_DIR/.partial/`. A job removes the ones that get no new bytes for 24 hours.

| Variable | Default | Description |
|----------|---------|-------------|
| `UPLOAD_DIR` | `./storage/uploads` | Folder for uploaded files. Docker mounts the `uploads` named volume here. |
| `UPLOAD_TIMEOUT_SECONDS` | `300` | Time limit for one upload request, which carries one chunk. |

### File downloads

One file downloads as it is. A folder or many items download as one zip.
A background job builds the zip, and the app keeps it for 24 hours.

| Variable | Default | Description |
|----------|---------|-------------|
| `DOWNLOAD_TIMEOUT_SECONDS` | `3600` | Time limit for one download request. |
| `DOWNLOAD_MAX_ZIP_MB` | `10240` | Largest zip the app builds. |
| `DOWNLOAD_ZIP_CHECK_SECONDS` | `1` | How often a zip job checks that its download still exists. |
