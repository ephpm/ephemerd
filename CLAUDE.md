# ephemerd

Ephemeral GitHub Actions runner daemon. Runs on Linux, Windows (WSL2), and macOS.

## Before committing

Always run `mage ci` before committing or pushing. This downloads dependencies, lints, tests, and builds:

```
mage ci
```

This is the same pipeline that runs in GitHub Actions. Fix any errors before creating commits.

Individual targets if needed:

- `mage lint` — download golangci-lint to `./bin/` and run it
- `mage test` — download embedded deps and run all tests
- `mage build` — compile ephemerd for the current OS

## Build system

This project uses [Mage](https://magefile.org/) (not Make). All dependency versions are pinned in `mage/download/download.go`.

## Project layout

- `cmd/ephemerd/` — CLI entry point (urfave-cli/v3)
- `pkg/` — library packages
- `api/v1/` — gRPC protobuf definitions
- `mage/` — build and download targets
- `docs/arch/` — architecture decision docs
- `examples/` — deployment examples (Terraform, etc.)
