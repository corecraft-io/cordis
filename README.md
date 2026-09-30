# Cordis (Go)

[![CI](https://github.com/corecraft-io/cordis/actions/workflows/ci.yml/badge.svg)](https://github.com/corecraft-io/cordis/actions/workflows/ci.yml)

**English** · [中文](README.zh-CN.md)

> A pure Go implementation of the **spatiotemporal composability** component model from *[A Programming Paradigm for Spatiotemporal Composability](https://arxiv.org/abs/2608.25512)*.
> Zero third-party dependencies · single-goroutine lock-free runtime · 44 tests green (incl. `-race`) · Apache-2.0

---

## 1. Overview

`cordis` is a **component runtime**: it makes components composable along two dimensions at once.

| Dimension | Claim | Mechanism |
| --- | --- | --- |
| **Temporal** | Every side effect carries an **explicit inverse** tracked by the runtime, so removing a component fully restores the environment | `ctx.Effect` / `ctx.Provide` / `ctx.On` all return a `Dispose`, reclaimed in **LIFO order** on unload |
| **Spatial** | Components declare service dependencies as **coeffects**; the runtime drives load/unload as satisfaction changes | `Plugin.Inject` feeds an `epoch`-derived target view that the state machine chases |

In one sentence: **you declare only what you need and how to create it; the runtime creates it at the right moment and tears it down completely when its dependencies disappear.**

---

## 2. Core Concepts

| Concept | Paper / official TS | This implementation | Notes |
| --- | --- | --- | --- |
| component definition | plugin | `Plugin` | `Name` + `Inject` (coeffect) + `Validate` (config validation) + `Apply` (component logic) |
| component instance | fiber / scope | `Fiber` | Holds the effect table, dependency snapshot, exposed services and lifecycle state machine |
| target view | `epoch` | `epoch` (unexported) | Derived from the current set of dependency implementations; any replacement yields a new epoch |
| coeffect store | `ReflectService` | `Reflect` | Global map from `isolateKey{name, realm}` to service implementation |
| isolation realm | isolation / realm | `Context.Isolate` | Same-named services in different realms are mutually invisible, allowing several service stacks to coexist (multi-tenancy) |
| intercept config | intercept | `Context.Intercept` | Construction-time config read by the provider, merged along the context chain |
| event bus | events | `Events` | Listeners are reclaimed with the Fiber that registered them |
| declarative config | loader | `Loader` | `Entry` / `EntryGroup` / `EntryTree` plus the reconciliation algorithm |

The table maps every concept of the paper and the official TypeScript implementation onto its counterpart here, so the two can be read side by side. The right-hand column records what the Go type actually holds, and the notes flag the semantics that matter when porting.

---

## 3. Architecture

```mermaid
flowchart TD
    App[App host + single-goroutine scheduler]
    App --> Root[Root Context]
    Root --> Registry[Registry plugin table]
    Root --> Reflect[Reflect coeffect store]
    Root --> Events[Events bus]
    Root --> Loader[Loader declarative layer]
    Loader --> Tree[EntryTree]
    Tree --> Group[EntryGroup]
    Tree --> Entry[Entry]
    Entry --> Fiber[Fiber component instance]
    Registry --> Fiber
    Fiber --> Ctx[Context chain]
    Ctx --> Store[isolates / intercepts / service lookup]
```

Three layers, matching the diagram:

1. **Host layer** — `App` owns the root context and the scheduler goroutine. `App.Do` / `App.DoSync` are the only legal entry points for outside goroutines.
2. **Runtime layer** — `Fiber` / `Context` / `Reflect` / `Registry` / `Events` provide all effect tracking and dependency resolution.
3. **Declarative layer** — `Loader` / `EntryTree` / `EntryGroup` / `Entry` turn "instantiate a component" from procedural code into a reconcilable configuration tree.

### File Responsibilities

| File | Lines | Responsibility |
| --- | --- | --- |
| `cordis.go` | 93 | Package doc, `FiberState`, error values, `Plugin` |
| `app.go` | 225 | `App` host, scheduler, `Wait` / `Close` |
| `context.go` | 203 | Unified context, `Isolate` / `Intercept` derivation, `Get` / `Provide` facade |
| `fiber.go` | 561 | State machine, epoch chasing, effects and LIFO disposal, hot config update |
| `reflect.go` | 241 | Coeffect store, realm key resolution, reverse dependency index and notifications |
| `registry.go` | 193 | `Plugin → Runtime` mapping, `Plugin` / `PluginInject` / `Inject` instantiation entry points |
| `events.go` | 189 | Event bus (`Emit` / `Serial` / `Bail` / `Parallel`) and `Logger` |
| `disposable.go` | 72 | Two-phase dispose steps and the order-preserving list |
| `loader.go` | 788 | Declarative configuration layer: `EntryOptions` / `Entry` / `EntryGroup` / `EntryTree` / `Loader` |
| `example/main.go` | 174 | End-to-end example covering hot reload, degradation and isolation |
| `cordis_test.go` | 1078 | Core runtime tests (22 + 1 benchmark) |
| `loader_test.go` | 707 | Loader tests (15 + 1 benchmark) |
| `index_internal_test.go` | 60 | Reverse index lifecycle invariants (white-box) |
| `disposable_internal_test.go` | 125 | Tombstone compaction invariants and benchmarks (1 + 3, white-box) |

---

## 4. Quick Start

```bash
git clone https://github.com/corecraft-io/cordis.git
cd cordis

go test ./...          # 44 tests
go test -race ./...    # race detector
go vet ./...
go run ./example       # end-to-end demo
```

### Using as a Dependency

```sh
go get github.com/corecraft-io/cordis
```

```go
import cordis "github.com/corecraft-io/cordis"
```

To hack on cordis alongside a project that depends on it, use a Go workspace
instead of a `replace` directive — `replace` is ignored when your module is used
as a dependency, and a local one also breaks `go install`. Put this in a `go.work`
**above both checkouts**, outside either repository:

```
// go.work
go 1.22

use (
    ./cordis
    ./your-project
)
```

The module path is a full domain path precisely so that this works: a bare module
name (e.g. `cordis`) cannot be resolved by the module proxy at all
(`malformed module path "cordis": missing dot in first path element`).

---

## 5. Usage

### 5.1 Defining and Instantiating

```go
app := cordis.New()
defer app.Close()

db := &cordis.Plugin{
    Name: "database",
    Apply: func(ctx *cordis.Context, config any) error {
        dsn := config.(string)
        if _, err := ctx.Provide("database", dsn, nil); err != nil {
            return err
        }
        _, err := ctx.Effect("conn", func() (cordis.Dispose, error) {
            return func() { log.Println("close", dsn) }, nil
        })
        return err
    },
}

app.Do(func(ctx *cordis.Context) {
    ctx.Plugin(db, "postgres://prod")
})
```

### 5.2 Effects and Disposal

Every side effect must supply an inverse, and every `Dispose` must be **idempotent**.

| API | Purpose |
| --- | --- |
| `ctx.Effect(label, fn)` | General effect: `fn` returns a `Dispose` |
| `ctx.EffectIter(label, iter)` | Incremental: yields many times, all disposed LIFO |
| `ctx.Provide(name, value, check)` | Registers a service; disposal is **dependant-first** |
| `ctx.On` / `ctx.Once` | Event listeners, auto-removed with the Fiber |

### 5.3 Services and Reactive Dependencies

```go
cache := &cordis.Plugin{
    Name:   "cache",
    Inject: map[string]any{"database": nil}, // coeffect: required dependency
    Apply: func(ctx *cordis.Context, _ any) error {
        dsn, ok := ctx.Get("database")
        if !ok {
            return errors.New("database unavailable")
        }
        _, err := ctx.Provide("cache", "cache:"+dsn.(string), nil)
        return err
    },
}
```

When the `database` provider goes away, the effects of `cache` are reclaimed automatically and it returns to `pending`; it comes back on its own once the provider returns. **No listener or retry logic has to be written by hand.**

The third argument of `Provide`, `check func() bool`, expresses "the service exists but is temporarily unusable": it is called on every dependency resolution, and returning `false` makes the dependent count as unsatisfied immediately. A panic inside `check` is swallowed and treated as unsatisfied (and logged).

### 5.4 Isolation Realms (Multi-Tenancy)

```go
ctx.Isolate("database", "tenant-a")   // shared within the realm, invisible across realms
ctx.Intercept("database", myConfig)   // intercept config read by the provider
```

Declared at the loader layer through `EntryOptions.Isolate`: `true` means a realm private to that entry (key `#entryID`), while a string means a shared realm (key `@label`).

### 5.5 Events

| Method | Semantics |
| --- | --- |
| `Emit` | Synchronous, ignores return values and errors (errors are logged) |
| `Serial` | Serial; the first non-nil result stops dispatch |
| `Bail` | Like `Serial`, but throws synchronously |
| `Parallel` | Aggregates all errors (equivalent to serial under one thread) |
| `EmitFiltered` | Dispatch with explicit realm filtering |

Built-in events: `internal/plugin` (instance created/disposed), `internal/status` (state transitions), `internal/service` (service up/down).

### 5.6 Declarative Configuration

```go
loader := cordis.NewLoader(app, func(name string) (*cordis.Plugin, error) {
    return plugins[name], nil
})

loader.Load([]cordis.EntryOptions{
    {ID: "db",    Name: "database", Config: "postgres://prod"},
    {ID: "cache", Name: "cache"},
    {ID: "web",   Name: "web",      Config: 8080},
})
```

| Operation | Behavior |
| --- | --- |
| `Load(options)` | Full reconciliation: create, remove and update, in configuration order |
| `Create` / `Remove` / `Update` | Single-entry operations; `Update` can move across groups, rebuilding the context and reloading fully |
| `EntryGroup` | Group entry; disabling cascades to all descendants |
| `EntryOptions.ID` | **Unique across the tree** (the index is keyed by short id, addressed by the `group:child` path); duplicates are rejected and logged |
| `EntryOptions.Inject` | Entry-level dependency overrides (`cordis.DepRemove` removes a declared dependency explicitly) |
| `EntryTree.OnCommit` | Synchronous callback after every structural change, for persistence |
| `NewLoader` | The same plugin may be instantiated many times, sharing one `Runtime` |

Dispatch rules for configuration changes (`Entry.update`):

- Disable (including cascading) disposes the Fiber and detaches the group subtree.
- A change to a spatial declaration (`Name` / `Group` / `Inject` / `Isolate` / `Intercept`) **detaches the old subtree synchronously**, disposes the old instance, then reloads fully under the new declaration. Effect reclamation is asynchronous, so the subtree structure must be consistent immediately or the short-id index collides during the rebuild.
- A change to `Config` alone goes through `Fiber.Update`. If it fails `Validate`, the Fiber enters `failed` and stays on the entry, recovering in place once corrected.
- A group entry reconciles its children through update hooks rather than restarting itself; a group config of the wrong type (not `[]EntryOptions`) is logged and the existing children are kept.

---

## 6. Concurrency Model

This replicates the JavaScript single-threaded event loop: `App` runs **exactly one** scheduler goroutine, and every state transition executes **serially** on it.

- **User callbacks (`Apply` / `Dispose` / event listeners) run inside the scheduler by construction**, so they may call any `Context` API directly, **with no locking**.
- Outside goroutines enter only through `App.Do` (asynchronous) or `App.DoSync` (blocking).
- ⚠️ **Never call `DoSync` / `Wait` from inside the scheduler** — that deadlocks.
- The task queue is an **unbounded slice behind a mutex**, not a fixed-capacity channel: posting from within a task cannot self-block, matching JS event-loop semantics.

`App.Wait()` blocks until the queue is drained and every Fiber is stable, and **returns whether the system truly converged** (it returns `false` and warns if the scheduler stopped or a polling limit was hit). `App.Close()` freezes the root fiber's target view, cascades reclamation down the effect chain and stops the scheduler; it is safe to call repeatedly.

---

## 7. Lifecycle State Machine

| State | Meaning |
| --- | --- |
| `pending` | Registered, dependencies unsatisfied, waiting |
| `loading` | Dependencies satisfied, running component logic |
| `active` | Logic succeeded and all dependencies still hold |
| `failed` | Failed after having been active; effects reclaimed, awaiting `Update` |
| `unloading` | Reclaiming effects in LIFO order |
| `disposed` | Unregistered from the parent; lifecycle over |

```mermaid
stateDiagram-v2
    [*] --> pending
    pending --> loading: deps satisfied
    loading --> active: apply ok
    loading --> failed: apply failed
    active --> unloading: deps lost / replaced / Update
    unloading --> pending: target inactive
    unloading --> loading: target active (reload)
    failed --> loading: Update clears error
    pending --> disposed: dispose
    active --> disposed: dispose
```

**Epoch inertia chain.** An epoch change does not transition immediately; it posts a `pump` task to the scheduler. `pump` decides against the **latest** epoch, and if the epoch changed again while it was transitioning — synchronous code moving the target view mid-transition — it keeps chasing until the current state matches the target. This is equivalent to the mutually chained reload/unload of the official implementation.

---

## 8. API Reference

### `App`

| Method | Description |
| --- | --- |
| `New()` | Creates the app and starts the scheduler |
| `Do(f)` / `DoSync(f)` | Run async / sync; `DoSync` reports whether the task actually ran |
| `Root()` | Root context, scheduler-side only |
| `Wait()` | Waits for stability, returns whether it converged |
| `Close()` | Cascades reclamation and stops the scheduler |
| `Logger()` | Logger accessor; `Error` / `Warn` / `Info` are all replaceable |

### `Context`

| Method | Description |
| --- | --- |
| `Get(name)` / `GetMust(name)` | Resolves a service up the Fiber chain, realm-aware; invisible unless the provider is `active` |
| `Provide(name, value, check)` | Registers a service, returning a Fiber-bound `Dispose` |
| `Set(name, value)` | Updates a value registered by this Fiber |
| `Effect` / `EffectIter` / `On` / `Once` / `Emit` | Effects and events |
| `Isolate(name, realm)` / `Intercept(name, cfg)` / `InterceptOf(name)` | Spatial declarations |
| `Plugin(p, config)` / `Inject(deps, apply)` | Instantiate a component / declare dynamic dependencies |
| `App()` / `Fiber()` / `Root()` / `Entry()` | Context navigation |

### `Registry` Error Contract

| Return | Meaning |
| --- | --- |
| `(nil, err)` | Structural failure (invalid plugin or an inactive parent context); the Fiber was never registered |
| `(f, err)` | Config failed `Validate`; the Fiber is registered and `failed`, fixable via `f.Update` or disposable via `f.Dispose` |
| `(f, nil)` | Success |

### `Fiber`

`State()` · `Err()` · `Config()` · `UID()` · `Update(config)` · `OnUpdate(hook)` · `Dispose()` · `Effect(label, fn)` · `EffectIter(label, iter)`

### Error Values

`ErrInactiveEffect` · `ErrServiceDuplicate` · `ErrServiceNotFound` · `ErrInvalidPlugin` · `ErrEntryNotFound`

---

## 9. Example Output

`go run ./example` covers five scenarios.

| Scenario | What to watch |
| --- | --- |
| initial load | Driven by dependency order: `database` → `cache` → `web` |
| hot reload `web: 8080 → 9090` | The old instance shuts down before the new one listens |
| `db` disable and restore | `cache` and `web` return to `pending`, then come back automatically |
| two-tenant stacks | `tenant-a` / `tenant-b` same-named services coexist without interference |
| removing `db-b` | That stack degrades while `tenant-a` is unaffected |

---

## 10. Test Coverage

`go test ./...` → **44 tests pass**; `go test -race ./...` reports no races; plus 5 benchmarks (`-bench .`).

CI (`.github/workflows/ci.yml`) runs on both **Go 1.22.x** (the minimum declared in `go.mod`) and **stable**: `gofmt -l` must be clean, then `go vet`, `go build`, `go test -race`, the benchmarks, and an example smoke run.

**Core runtime (`cordis_test.go`, 22 tests)**

| Test | Coverage |
| --- | --- |
| `TestPluginLifecycle` | lifecycle transitions |
| `TestReactiveCoeffects` | reactive deps: providers coming and going drive dependents |
| `TestDependantFirstDispose` | disposal ordering |
| `TestIsolation` / `TestSharedRealm` | private and shared realms |
| `TestHotReload` | hot config reload |
| `TestFailureAndRecovery` | `apply` failure and recovery via `Update` |
| `TestConfigValidation` / `TestPluginErrorContract` | `Validate` failure paths and the error contract |
| `TestServiceEvents` | `internal/service` events |
| `TestDuplicateProvide` | duplicate registration in one realm |
| `TestEpochChase` | epoch changing again mid-transition |
| `TestEventListenerCleanup` | listeners reclaimed with the Fiber |
| `TestRootCloseCascades` | cascading root close |
| `TestCheckFunction` | unhealthy `check` |
| `TestServiceHiddenWhileProviderUnloading` | visibility during the disposal window |
| `TestDisposePanicLogged` | panics logged and not propagated |
| `TestWaitReportsConvergence` | `Wait` / `DoSync` convergence and execution reporting |
| `TestSchedulerUnboundedQueue` | no self-deadlock on overflow within one task |
| `TestConcurrentExternalCalls` | 8 goroutines mixing `Do`/`DoSync`: no lost or duplicated tasks, strict serialization, honest execution reporting |
| `TestConcurrentPluginRegistration` | 160 concurrent registrations all converge to `active` |
| `TestConcurrentCloseWithPosts` | `Close` racing external posts: no deadlock, no panic, idempotent |

**Loader (`loader_test.go`, 15 tests)**

| Test | Coverage |
| --- | --- |
| `TestLoaderBasicLoad` / `TestLoaderReconcile` / `TestLoaderConfigReload` | load, full reconciliation, config reload |
| `TestLoaderGroup` / `TestLoaderGroupIsolateRebuild` / `TestLoaderGroupConfigTypeError` | group reconcile, rebuild on a spatial change, keeping children on a config type error |
| `TestLoaderEntryIsolate` / `TestLoaderEntryInject` | entry-level realm and dependency declarations |
| `TestLoaderTreeOperations` / `TestLoaderGroupMoveRebuild` | tree edits and cross-group moves (including synchronous subtree detach and rebuild) |
| `TestLoaderDuplicateShortID` | duplicate short-id rejection, including across groups |
| `TestLoaderConfigErrorRecovery` | recovery in place after a validation failure |
| `TestLoaderCommitHook` / `TestLoaderSelfDispose` | commit hook, self-dispose |
| `TestLoaderLargeLoad` | 1100 entries in one load without deadlock |

**Internal invariants (white-box)**

`TestReflectIndexLifecycle` (`index_internal_test.go`) — strict track/untrack pairing for the reverse dependency index: disposal, unregistered failure paths, and the index draining to empty after a cascading `Close`.

`TestDisposableCompactionPreservesOrder` (`disposable_internal_test.go`) — compaction actually fires (`order > 8` and `order > 2× live`), and the sort-based rebuild preserves LIFO order.

**Benchmarks**

| Benchmark | What it measures |
| --- | --- |
| `BenchmarkServiceNotify` | cost of service up/down notifications (before/after the reverse index, see commit history) |
| `BenchmarkLoaderLoad` | reconciliation throughput |
| `BenchmarkDisposableSteadyChurn` | amortized churn cost including tombstone rebuilds |
| `BenchmarkDisposableCompaction` | an isolated `order` rebuild (map iteration plus sort) |
| `BenchmarkDisposableClear` | `clear` cost against history length; flat means compaction is working |

Measured numbers for the three compaction benchmarks, with interpretation, are in table 3 of the code review report: growing the live set 64× costs only 2.2× per churn, and `clear` stays flat while the history grows 64×, which is the evidence that `order` does not accumulate over a long-lived list.

---

## 11. References

- **Paper**: [A Programming Paradigm for Spatiotemporal Composability](https://arxiv.org/abs/2608.25512) (arXiv:2608.25512 · [DOI](https://doi.org/10.48550/arXiv.2608.25512), by authors from Peking University and DeepSeek-AI, 2026-08-26) — the theoretical source of the spatiotemporal composability model; the paper's source files live in [cordiverse/paper](https://github.com/cordiverse/paper).
- **Official TypeScript implementation**: [cordiverse/cordis](https://github.com/cordiverse/cordis) (npm [`cordis`](https://www.npmjs.com/package/cordis), MIT) — the semantic baseline this repository mirrors module by module (concept mapping in §2).
- Package-level design notes are in the header comment of `cordis.go`; per-module tradeoffs are in the corresponding source files.

---

## 12. License

This project is licensed under the [Apache License 2.0](LICENSE); the full text is in `LICENSE` at the repository root.

```
Copyright 2026 metaRobin

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
```

> Note: the **official TypeScript implementation** is an independent project whose license is unrelated to this repository; copyright of the paper belongs to its authors.