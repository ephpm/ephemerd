# AGENTS.md

Hard rules for any AI agent (Claude Code, Cursor, Aider, etc.) working in this repo. CLAUDE.md has the longer-form expectations; this file is the short list of things that get caught in CI and shouldn't.

## Before every `git push`

Run the CI pipeline locally and fix every error before pushing. **No exceptions.** Not "looks fine to me", not "the change is small", not "my local cgo is broken so I'll skip it" — those are the exact reasons CI gets surprised. If the local environment can't run `mage ci`, fix that environment before pushing, or run the targets individually and document what was skipped *and why* in the commit message.

```
mage ci
```

That target runs download, lint, test, and build — the same sequence CI runs. If `mage ci` is too slow for an iteration loop, run the relevant subset:

```
mage lint          # golangci-lint — errcheck and errorlint enabled
mage test          # go test ./...
mage build         # cross-compile for current OS
mage e2egithub     # GitHub provider e2e against fake server
mage e2emodproxy   # Go module proxy e2e
```

If any of those fail, fix the failure before pushing. A red CI run on a PR you opened is a process failure, not a discovery.

## Specifically: the `golangci-lint` failure modes that have bitten us

- **errcheck** — every fallible call must be checked. Closures over `http.ResponseWriter` writes, `Close()` in defers, `io.Copy` return values, `fmt.Fprintf` return values. The repo policy is to wrap them in `if err := foo(); err != nil { log.Warn(...) }`, never `_ = foo()`.
- **staticcheck SA9003** — empty `if` branches. If the branch is empty because the comment is "either is acceptable", invert the condition and `t.Errorf`/`return err` for the *un*expected case.
- **typecheck failures on Windows** — `miekg/pkcs11` cgo preprocessing fails on Windows. This is a *local* problem, not a CI problem. Running lint on Linux works. If you can't run lint locally, you push at your own risk and the user's annoyance.

## Specifically: the test failure modes that have bitten us

- **`pkg/dind/TestPushHandlerEndToEnd`** has been flaky in CI in ways that aren't obviously reproducible locally. Don't paper over a flake with a `cs.Info()` "warm-up" call or a `time.Sleep` — that's flake-masking, and the real bug will resurface in the next iteration. If a test is genuinely flaky, find the race or the missing lease/label and fix it; if you can't, mark it `t.Skip` with an issue number, not a silent diagnostic.

## Pushing workflow yaml changes

Workflow YAML doesn't go through `mage lint`, but the consequences of a broken `.github/workflows/*.yml` are worse than Go lint errors — a bad workflow doesn't tell you anything when it fails on the *next* trigger. For non-trivial workflow changes:

- Use [`actionlint`](https://github.com/rhysd/actionlint) if installed, or paste into [rhysd.github.io/actionlint](https://rhysd.github.io/actionlint/) for a quick sanity check.
- Sanity-check `runs-on` labels exist on registered self-hosted runners (ephemerd JIT-registers based on host `goruntime.GOARCH`; cross-arch runs require the matching host to be online).
- Confirm any `secrets.*` references exist in the repo/org secrets before pushing.

## Pushing release-pipeline / tag-triggered changes

`.github/workflows/release.yml` only fires on `push: tags: v*`, so you cannot test it on a branch. Sequence:

1. Push the workflow change in a regular PR. Get it merged.
2. Push a release-candidate tag like `v0.0.1-rc1` to validate the full pipeline end-to-end before pushing the real tag.
3. Only push `v0.0.1` once the rc has produced a clean draft release.

Do **not** push the real tag as the first test of a changed release workflow.
