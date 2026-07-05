# Contributing to Tollgate

Thanks for your interest! Tollgate is early and moving fast — small, focused
contributions land easiest.

## Development setup

Go 1.25+ is the only requirement (the SQLite driver is pure Go — no CGO, no
C toolchain).

```sh
git clone https://github.com/opslync/tollgate
cd tollgate
make build          # -> bin/tollgate
make test           # go test ./...
make lint           # go vet + gofmt (+ golangci-lint if installed)
```

Run it locally against a provider:

```sh
cp config.example.yaml config.yaml   # edit: providers, agents, budgets
./bin/tollgate --config config.yaml
```

See [ARCHITECTURE.md](ARCHITECTURE.md) for how requests flow through the
codebase and what each package owns.

## Pull requests

- **Keep PRs small and single-purpose.** One feature or fix per PR.
- **Tests land with the change.** Every package has table-driven tests to
  mirror; enforcement and parsing changes need coverage for the failure
  paths, not just the happy path.
- **CI must be green**: build, `go test -race`, golangci-lint, and
  `helm lint` all run on every PR.
- Match the existing code style — standard Go, comments only where the code
  can't say it.

## Reporting bugs / proposing features

Open an issue using the templates. For anything security-sensitive, see
[SECURITY.md](SECURITY.md) — please do not open public issues for
vulnerabilities.

## Pricing table updates

`pricing/pricing.yaml` is versioned and maintained by hand. PRs updating
rates are very welcome — bump the `version` date and link the provider's
published pricing page in the PR description.
