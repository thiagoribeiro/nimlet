# Contributing

Issues and pull requests are welcome.

## Development

```sh
make docker-up          # Postgres + Redis for local development
make run                # start the server with dev settings
make test-unit          # unit tests
TEST_POSTGRES_DSN=... TEST_REDIS_ADDR=... make test-integration
make lint               # golangci-lint
```

See [README.md](README.md) for configuration and architecture, and
[docs/operations.md](docs/operations.md) for database roles and deployment.

## Guidelines

- Keep the hexagonal layout: `domain` → `port` → `usecase` → `adapter`/`transport`.
- Add tests next to the code you change; storage changes need integration tests.
- Schema changes are new goose migrations in `migrations/postgres/`; never edit an applied migration's SQL.
- Commits follow [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`…).
- By contributing you agree that your contributions are licensed under the Apache License 2.0.

Security issues: see [SECURITY.md](SECURITY.md).
