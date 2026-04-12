# pkg/localtunnel

Vendored and modified copy of [github.com/localtunnel/go-localtunnel](https://github.com/localtunnel/go-localtunnel).

## Why this is vendored

The upstream library is unmaintained (last commit 2017, last npm publish ~4 years ago). The `Listen` function does not accept a `context.Context`, making it impossible to cancel or timeout tunnel establishment. When the free localtunnel server is unresponsive, `Listen` hangs indefinitely.

Rather than depend on a dead upstream or maintain a fork, we copied the source and added context support directly.

## Changes from upstream

- `Listen` now accepts a `context.Context` as its first parameter
- The HTTP setup request uses `http.NewRequestWithContext` so cancellation aborts the registration call
- The internal context (used by proxy goroutines) is derived from the caller's context via `context.WithCancel`

## License

This code is licensed under the [Mozilla Public License 2.0](LICENSE). The MPL 2.0 is file-level copyleft — these files remain under MPL 2.0, but the rest of the ephemerd project (MIT) is unaffected.
