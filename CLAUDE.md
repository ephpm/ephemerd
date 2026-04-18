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

## Expectations for all agents

- **Write tests.** For every change, add unit tests for the logic you touched. If the change crosses a real boundary (containerd, WSL, GitHub API, network, filesystem at scale), add an integration or e2e test too — see `test/e2e/` for the privileged containerd suite and the existing `*_test.go` patterns in each package. If something genuinely can't be tested (e.g. requires hardware, external OS behaviour), say so explicitly in the PR rather than silently skipping.
- **Fix lint errors before committing.** Run `mage lint` (or `mage ci` which includes it) and fix every issue before creating a commit. The CI pipeline runs `golangci-lint` with `errcheck` and `errorlint` enabled — if it fails in CI, you should have caught it locally first.
- **Never discard errors with `_ =`.** Always check error return values and at minimum log a warning. Use `if err := foo(); err != nil { log.Warn(...) }` instead of `_ = foo()`. This applies to Close(), Remove(), Write(), and every other fallible call. If you truly cannot handle the error (e.g., in a defer after another error), wrap it in a log statement anyway.
- **Flag complicated features for an arch doc.** Before finishing a non-trivial feature — new subsystems, cross-platform behaviour, anything that changes how components talk to each other, anything future-you will have to re-derive from code — ask the user whether a `docs/arch/<feature>.md` is warranted. Don't write one speculatively, and don't skip asking just because the code "feels clear right now."
