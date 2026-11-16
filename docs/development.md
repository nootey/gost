# Development

## Database

```sh
make migrate-up                          # run all migrations
make migrate-down                        # roll back the last migration
make migrate-create NAME=create_x_table  # add a new migration
make migrate-fresh                       # drop, migrate and seed
make seed                                # core data: roles, permissions, root user
```

Queries live in `internal/queries/`. Run `make sqlc` after you change them.

## Testing

```sh
make test              # all tests
make test-unit         # unit tests, with mocks
make test-integration  # integration tests, with a real database
```

Integration tests use Testcontainers to start PostgreSQL. Docker must be running.
Run `make mock` after you change an interface.

## Linting

```sh
make lint                      # server
cd client && pnpm run lint     # client
```

`make pre-push` runs the full check: mocks, build, lint and tests for the server and the client.

## CI/CD

GitHub Actions runs these workflows:

- **Lint** - client and server
- **Tests** - all server tests with coverage
- **Release** - creates Git tags and GitHub releases

### Release automation

Create these labels in GitHub (Settings → Labels):

| Name | Color | Description |
|------|-------|-------------|
| `release:major` | `#d73a4a` | Breaking changes (v1.0.0 → v2.0.0) |
| `release:minor` | `#fbca04` | New features (v1.0.0 → v1.1.0) |
| `release:patch` | `#0e8a16` | Bug fixes (v1.0.0 → v1.0.1) |

To make a release:

1. Create a PR to `main`
2. Add one of the `release:*` labels
3. Merge the PR

The workflow then creates the tag and the release.
