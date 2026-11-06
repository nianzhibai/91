# gotd/log

A minimal, dependency-free logging port for gotd libraries.

A library writes structured records to a `log.Logger`; it never logs through a
global and never imports a concrete logging framework. The consumer picks the
backend by passing an adapter.

```go
import (
    "github.com/gotd/log"
    "github.com/gotd/log/logslog" // stdlib log/slog backend, no extra deps
)

l := logslog.New(slog.Default())

if l.Enabled(ctx, log.LevelDebug) {
    l.Log(ctx, log.LevelDebug, "applying update",
        log.Int64("chat_id", chatID),
        log.Int("pts", pts),
    )
}
```

For ergonomic call sites, wrap a `Logger` in a `Helper`:

```go
h := log.For(l) // log.For(nil) is a no-op Helper
h.Info(ctx, "connected", log.String("dc", dc))
```

## Design

- **`Logger` is two methods**: `Enabled(ctx, level)` and `Log(ctx, level, msg, ...Attr)`.
  The `Enabled` gate lets callers skip building attributes on hot paths.
- **Attributes don't box scalars.** `log.Int64`, `log.Bool`, `log.Duration`, …
  store into a compact `Value`; only `Any`, `Error` and `Time` carry an
  `interface{}`.
- **Levels match `log/slog`** (Debug=-4 … Error=8) so adapters convert by casting.
- **No global state.** Nothing here registers or reads a process-wide logger.

## Modules

| Module                            | Depends on                  |
|-----------------------------------|-----------------------------|
| `github.com/gotd/log`             | nothing (stdlib)            |
| `github.com/gotd/log/logslog`     | stdlib `log/slog`           |
| `github.com/gotd/log/logzap`      | `go.uber.org/zap`           |
| `github.com/gotd/log/logzerolog`  | `github.com/rs/zerolog`     |
| `github.com/gotd/log/loglogrus`   | `github.com/sirupsen/logrus`|

Each backend adapter is a **separate module** so that its dependency never
enters the core graph. Import only the one you use.

## Implementing an adapter

An adapter implements `log.Logger` by switching on `Attr.Value.Kind()` and
mapping each kind to the backend's typed field; fall back to `Value.Any()` for
`KindAny`. See `logslog`, `logzap`, `logzerolog` and `loglogrus` for references.
