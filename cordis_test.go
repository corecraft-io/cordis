package cordis_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cordis "github.com/corecraft-io/cordis"
)

// harness 测试脚手架：所有场景在调度器上下文中执行，
// app.Wait 保证状态机收敛后再断言。
type harness struct {
	t   *testing.T
	app *cordis.App
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return &harness{t: t, app: cordis.New()}
}

func (h *harness) run(f func(ctx *cordis.Context)) {
	h.app.DoSync(f)
	h.app.Wait()
}

// doSync 在调度器上下文中执行 f 并返回其结果，供需要在调度器内
// recover（如断言 panic）或取值的用例使用。
func (h *harness) doSync(f func(ctx *cordis.Context) any) any {
	var out any
	h.app.DoSync(func(ctx *cordis.Context) { out = f(ctx) })
	h.app.Wait()
	return out
}

// states 记录 fiber 的状态迁移序列（internal/status 事件）。
type states struct {
	mu    sync.Mutex
	trace []string
}

func (s *states) record(f *cordis.Fiber, from cordis.FiberState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trace = append(s.trace, fmt.Sprintf("%s>%s", from, f.State()))
}

func TestPluginLifecycle(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	var order []string
	p := &cordis.Plugin{
		Name: "demo",
		Apply: func(ctx *cordis.Context, config any) error {
			order = append(order, "apply")
			if _, err := ctx.Effect("e1", func() (cordis.Dispose, error) {
				return func() { order = append(order, "dispose1") }, nil
			}); err != nil {
				return err
			}
			if _, err := ctx.Effect("e2", func() (cordis.Dispose, error) {
				return func() { order = append(order, "dispose2") }, nil
			}); err != nil {
				return err
			}
			return nil
		},
	}

	var f *cordis.Fiber
	h.run(func(ctx *cordis.Context) {
		var err error
		f, err = ctx.Plugin(p, nil)
		if err != nil {
			t.Fatal(err)
		}
		if f.State() != cordis.StatePending && f.State() != cordis.StateLoading {
			// 注册在同步段内完成，转换任务入队但未执行。
			t.Fatalf("expected pre-transition state, got %s", f.State())
		}
	})
	if f.State() != cordis.StateActive {
		t.Fatalf("expected active, got %s", f.State())
	}

	h.run(func(ctx *cordis.Context) {
		f.Dispose()
	})
	if f.State() != cordis.StateDisposed {
		t.Fatalf("expected disposed, got %s", f.State())
	}
	want := []string{"apply", "dispose2", "dispose1"} // LIFO 逆序回收
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("unexpected order: %v", order)
	}
}

func TestReactiveCoeffects(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	var events []string
	provider := &cordis.Plugin{
		Name: "database",
		Apply: func(ctx *cordis.Context, _ any) error {
			events = append(events, "db+")
			_, err := ctx.Provide("database", map[string]string{"kind": "sqlite"}, nil)
			return err
		},
	}
	consumer := &cordis.Plugin{
		Name:   "cache",
		Inject: map[string]any{"database": nil},
		Apply: func(ctx *cordis.Context, _ any) error {
			db, ok := ctx.Get("database")
			if !ok {
				return fmt.Errorf("database not visible")
			}
			if db.(map[string]string)["kind"] != "sqlite" {
				return fmt.Errorf("wrong service value")
			}
			events = append(events, "cache+")
			_, err := ctx.Effect("conn", func() (cordis.Dispose, error) {
				return func() { events = append(events, "cache-") }, nil
			})
			return err
		},
	}

	var db, cache *cordis.Fiber
	h.run(func(ctx *cordis.Context) {
		db, _ = ctx.Plugin(provider, nil)
		cache, _ = ctx.Plugin(consumer, nil)
	})
	if cache.State() != cordis.StateActive {
		t.Fatalf("consumer should be active, got %s", cache.State())
	}

	// 提供者下线 → 依赖者自动卸载（效果回收），回到 PENDING。
	h.run(func(ctx *cordis.Context) {
		db.Dispose()
	})
	if cache.State() != cordis.StatePending {
		t.Fatalf("consumer should fall back to pending, got %s", cache.State())
	}
	if fmt.Sprint(events) != fmt.Sprint([]string{"db+", "cache+", "cache-"}) {
		t.Fatalf("unexpected events: %v", events)
	}

	// 提供者回归 → 依赖者自动恢复。
	events = nil
	h.run(func(ctx *cordis.Context) {
		db2, _ := ctx.Plugin(provider, nil)
		db = db2
	})
	if cache.State() != cordis.StateActive {
		t.Fatalf("consumer should reactivate, got %s", cache.State())
	}
	if fmt.Sprint(events) != fmt.Sprint([]string{"db+", "cache+"}) {
		t.Fatalf("unexpected events: %v", events)
	}
}

func TestDependantFirstDispose(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	var log []string
	provider := &cordis.Plugin{
		Name: "database",
		Apply: func(ctx *cordis.Context, _ any) error {
			// 先注册服务，再注册尾部效果：LIFO 下尾部先回收，
			// 服务撤销（含等待依赖者下线）最后完成。
			_, err := ctx.Provide("database", "db", nil)
			if err != nil {
				return err
			}
			_, err = ctx.Effect("tail", func() (cordis.Dispose, error) {
				return func() { log = append(log, "provider-tail-") }, nil
			})
			return err
		},
	}
	consumer := &cordis.Plugin{
		Name:   "consumer",
		Inject: map[string]any{"database": nil},
		Apply: func(ctx *cordis.Context, _ any) error {
			_, err := ctx.Effect("work", func() (cordis.Dispose, error) {
				return func() { log = append(log, "consumer-") }, nil
			})
			return err
		},
	}

	var db, app *cordis.Fiber
	h.run(func(ctx *cordis.Context) {
		db, _ = ctx.Plugin(provider, nil)
		app, _ = ctx.Plugin(consumer, nil)
	})
	_ = app
	h.run(func(ctx *cordis.Context) {
		db.Dispose()
	})
	want := []string{"provider-tail-", "consumer-"}
	if fmt.Sprint(log) != fmt.Sprint(want) {
		t.Fatalf("dependant must unload before provider service cleanup: %v", log)
	}
}

func TestIsolation(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	var activated int
	var seenFoo any
	newConsumer := func() *cordis.Plugin {
		return &cordis.Plugin{
			Inject: map[string]any{"foo": nil},
			Apply: func(ctx *cordis.Context, _ any) error {
				activated++
				if v, ok := ctx.Get("foo"); ok {
					seenFoo = v
				}
				return nil
			},
		}
	}

	var rootFiber, realm1, realm2 *cordis.Fiber
	h.run(func(ctx *cordis.Context) {
		rootFiber, _ = ctx.Plugin(newConsumer(), nil)
		r1, _ := ctx.Isolate("foo", "realm-1").Plugin(newConsumer(), nil)
		realm1 = r1
		r2, _ := ctx.Isolate("foo", "realm-2").Plugin(newConsumer(), nil)
		realm2 = r2
	})
	if activated != 0 {
		t.Fatalf("no service yet, no consumer should be active: %d", activated)
	}

	// 默认域提供 → 仅默认域依赖者激活，且取到本域值。
	h.run(func(ctx *cordis.Context) {
		ctx.Provide("foo", 100, nil)
	})
	if activated != 1 || rootFiber.State() != cordis.StateActive || seenFoo != 100 {
		t.Fatalf("only default-realm consumer should activate: %d seen=%v", activated, seenFoo)
	}

	// realm-1 域内的提供者插件注册服务 → 仅 realm-1 依赖者激活。
	provider := &cordis.Plugin{
		Name: "foo-provider",
		Apply: func(ctx *cordis.Context, _ any) error {
			_, err := ctx.Provide("foo", 200, nil)
			return err
		},
	}
	seenFoo = nil
	h.run(func(ctx *cordis.Context) {
		ctx.Isolate("foo", "realm-1").Plugin(provider, nil)
	})
	if activated != 2 || realm1.State() != cordis.StateActive || realm2.State() != cordis.StatePending {
		t.Fatalf("realm isolation broken: activated=%d", activated)
	}
	if seenFoo != 200 {
		t.Fatalf("realm-1 consumer should see realm-1 value, got %v", seenFoo)
	}

	// 跨域不可见：realm-2 上下文解析不到任何域的服务；
	// 默认域上下文只看到默认域的服务。
	h.run(func(ctx *cordis.Context) {
		if _, ok := ctx.Isolate("foo", "realm-2").Get("foo"); ok {
			t.Fatal("realm-2 must not see services of other realms")
		}
		if v, ok := ctx.Get("foo"); !ok || v != 100 {
			t.Fatalf("default realm should see its own service, got %v %v", v, ok)
		}
	})
}

func TestSharedRealm(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	var activated int
	newConsumer := func() *cordis.Plugin {
		return &cordis.Plugin{
			Inject: map[string]any{"foo": nil},
			Apply:  func(*cordis.Context, any) error { activated++; return nil },
		}
	}
	var a, b *cordis.Fiber
	h.run(func(ctx *cordis.Context) {
		ctx1 := ctx.Isolate("foo", "shared")
		a, _ = ctx1.Plugin(newConsumer(), nil)
		ctx2 := ctx.Isolate("foo", "shared")
		b, _ = ctx2.Plugin(newConsumer(), nil)
	})
	h.run(func(ctx *cordis.Context) {
		ctx.Isolate("foo", "shared").Provide("foo", 1, nil)
	})
	if activated != 2 || a.State() != cordis.StateActive || b.State() != cordis.StateActive {
		t.Fatalf("shared realm should satisfy both consumers: %d", activated)
	}
}

func TestHotReload(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	var applied, disposed int
	var seenConfig string
	p := &cordis.Plugin{
		Name: "server",
		Apply: func(ctx *cordis.Context, config any) error {
			applied++
			seenConfig = config.(string)
			_, err := ctx.Effect("listener", func() (cordis.Dispose, error) {
				return func() { disposed++ }, nil
			})
			return err
		},
	}
	var f *cordis.Fiber
	h.run(func(ctx *cordis.Context) {
		f, _ = ctx.Plugin(p, "v1")
	})
	if seenConfig != "v1" || applied != 1 {
		t.Fatalf("bad initial load: %v %v", seenConfig, applied)
	}

	h.run(func(ctx *cordis.Context) {
		if err := f.Update("v2"); err != nil {
			t.Fatal(err)
		}
	})
	if applied != 2 || disposed != 1 || seenConfig != "v2" {
		t.Fatalf("hot reload failed: applied=%d disposed=%d config=%s", applied, disposed, seenConfig)
	}
	if f.State() != cordis.StateActive {
		t.Fatalf("expected active after reload, got %s", f.State())
	}
}

func TestFailureAndRecovery(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	var applied, disposed int
	fail := true
	p := &cordis.Plugin{
		Name: "flaky",
		Apply: func(ctx *cordis.Context, _ any) error {
			applied++
			_, err := ctx.Effect("e", func() (cordis.Dispose, error) {
				return func() { disposed++ }, nil
			})
			if err != nil {
				return err
			}
			if fail {
				return fmt.Errorf("boom")
			}
			return nil
		},
	}
	var f *cordis.Fiber
	h.run(func(ctx *cordis.Context) {
		f, _ = ctx.Plugin(p, nil)
	})
	if f.State() != cordis.StateFailed || f.Err() == nil {
		t.Fatalf("expected failed fiber, got %s err=%v", f.State(), f.Err())
	}
	// 失败实例的效果同样被回收。
	if disposed != 1 {
		t.Fatalf("effects of failed load must be reverted, disposed=%d", disposed)
	}

	// Update 清除错误并重启。
	fail = false
	h.run(func(ctx *cordis.Context) {
		if err := f.Update(nil); err != nil {
			t.Fatal(err)
		}
	})
	if f.State() != cordis.StateActive || f.Err() != nil {
		t.Fatalf("expected recovery, got %s err=%v", f.State(), f.Err())
	}
	// 失败实例的效果已在失败路径回收（disposed=1），
	// 重启是一次干净的加载：不产生新的撤销。
	if applied != 2 || disposed != 1 {
		t.Fatalf("unexpected counts: applied=%d disposed=%d", applied, disposed)
	}
}

func TestConfigValidation(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	p := &cordis.Plugin{
		Name: "strict",
		Validate: func(config any) (any, error) {
			s, ok := config.(string)
			if !ok {
				return nil, fmt.Errorf("config must be string")
			}
			return s, nil
		},
		Apply: func(*cordis.Context, any) error { return nil },
	}
	var (
		f   *cordis.Fiber
		err error
	)
	h.run(func(ctx *cordis.Context) {
		f, err = ctx.Plugin(p, 42)
	})
	if err == nil {
		t.Fatal("invalid config must return an error")
	}
	if f == nil || f.State() != cordis.StateFailed {
		t.Fatalf("invalid config should register a FAILED fiber, got %v", f)
	}
	h.run(func(ctx *cordis.Context) {
		if err := f.Update("ok"); err != nil {
			t.Fatal(err)
		}
	})
	if f.State() != cordis.StateActive {
		t.Fatalf("valid config should activate, got %s", f.State())
	}
}

// 回归（M-2）：PluginInject 的错误契约——
// 结构性失败返回 (nil, err)；配置校验失败返回 (f, err)，
// fiber 处于 FAILED 且注销句柄已接线（Dispose 后注册表清空）。
func TestPluginErrorContract(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	h.run(func(ctx *cordis.Context) {
		if f, err := ctx.Plugin(&cordis.Plugin{Name: "bad"}, nil); err == nil || f != nil {
			t.Fatalf("invalid plugin must return (nil, err), got (%v, %v)", f, err)
		}
	})

	p := &cordis.Plugin{
		Name: "strict",
		Validate: func(config any) (any, error) {
			if s, ok := config.(string); ok {
				return s, nil
			}
			return nil, fmt.Errorf("config must be string")
		},
		Apply: func(*cordis.Context, any) error { return nil },
	}
	var f *cordis.Fiber
	h.run(func(ctx *cordis.Context) {
		var err error
		if f, err = ctx.Plugin(p, 42); err == nil {
			t.Fatal("validation failure must return an error")
		}
	})
	if f == nil || f.State() != cordis.StateFailed {
		t.Fatalf("validation failure should register a FAILED fiber, got %v", f)
	}

	h.run(func(ctx *cordis.Context) {
		f.Dispose()
		if ctx.Registry().Has(p) {
			t.Fatal("disposed failed fiber should unregister its runtime")
		}
	})
}

func TestServiceEvents(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	var seen []any
	h.run(func(ctx *cordis.Context) {
		ctx.On("internal/service", func(c *cordis.Context, args ...any) any {
			seen = append(seen, args...)
			return nil
		})
	})
	var dispose cordis.Dispose
	h.run(func(ctx *cordis.Context) {
		d, err := ctx.Provide("foo", "value", nil)
		if err != nil {
			t.Fatal(err)
		}
		dispose = d
	})
	h.run(func(ctx *cordis.Context) {
		dispose()
	})
	if len(seen) != 4 { // (name, value) × (provide, dispose)
		t.Fatalf("expected 2 service events, got %v", seen)
	}
	if seen[0] != "foo" || seen[1] != "value" {
		t.Fatalf("bad provide event: %v", seen)
	}
	if seen[2] != "foo" || seen[3] != nil {
		t.Fatalf("bad dispose event: %v", seen)
	}
}

func TestDuplicateProvide(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	var err error
	h.run(func(ctx *cordis.Context) {
		_, err = ctx.Provide("foo", 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ctx.Provide("foo", 2, nil)
	})
	if err == nil {
		t.Fatal("expected duplicate error")
	}
}

func TestEpochChase(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	// 依赖者在卸载过程中目标视图再次改变：最终应收敛到最新目标。
	var events []string
	provider := &cordis.Plugin{
		Name: "db",
		Apply: func(ctx *cordis.Context, _ any) error {
			events = append(events, "db+")
			_, err := ctx.Provide("db", "v", nil)
			return err
		},
	}
	consumer := &cordis.Plugin{
		Name:   "app",
		Inject: map[string]any{"db": nil},
		Apply: func(ctx *cordis.Context, _ any) error {
			events = append(events, "app+")
			_, err := ctx.Effect("x", func() (cordis.Dispose, error) {
				return func() { events = append(events, "app-") }, nil
			})
			return err
		},
	}
	var db *cordis.Fiber
	h.run(func(ctx *cordis.Context) {
		db, _ = ctx.Plugin(provider, nil)
		ctx.Plugin(consumer, nil)
	})
	h.run(func(ctx *cordis.Context) {
		// 同步段内先注销旧提供者再注册新提供者：
		// 消费者的目标视图经历 ":1" → inactive → ":2"，
		// pump 执行时直接看到最终目标，一次收敛。
		db.Dispose()
		db2, _ := ctx.Plugin(provider, nil)
		db = db2
	})
	// 最终：新提供者 active，消费者 active（无论中间路径）。
	if db.State() != cordis.StateActive {
		t.Fatalf("provider should be active: %s", db.State())
	}
	found := false
	for _, e := range events {
		if e == "app+" {
			found = true
		}
	}
	if !found {
		t.Fatalf("consumer should have reactivated: %v", events)
	}
}

func TestEventListenerCleanup(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	var ticks int
	p := &cordis.Plugin{
		Name: "ticker",
		Apply: func(ctx *cordis.Context, _ any) error {
			_, err := ctx.On("tick", func(*cordis.Context, ...any) any {
				ticks++
				return nil
			})
			return err
		},
	}
	var f *cordis.Fiber
	// 注册与 Emit 必须分批：Plugin 只入队 pump 任务，
	// Apply（含监听器注册）要到本同步段结束后才执行。
	h.run(func(ctx *cordis.Context) {
		f, _ = ctx.Plugin(p, nil)
	})
	h.run(func(ctx *cordis.Context) {
		ctx.Emit("tick")
	})
	if ticks != 1 {
		t.Fatalf("listener should fire, ticks=%d", ticks)
	}
	// Dispose 同理：注销也是入队任务，效果回收在下一段完成。
	h.run(func(ctx *cordis.Context) {
		f.Dispose()
	})
	h.run(func(ctx *cordis.Context) {
		ctx.Emit("tick")
	})
	if ticks != 1 {
		t.Fatalf("listener must be auto-removed with fiber, ticks=%d", ticks)
	}
}

func TestRootCloseCascades(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	var log []string
	provider := &cordis.Plugin{
		Name: "db",
		Apply: func(ctx *cordis.Context, _ any) error {
			_, err := ctx.Provide("db", 1, nil)
			return err
		},
	}
	consumer := &cordis.Plugin{
		Name:   "app",
		Inject: map[string]any{"db": nil},
		Apply: func(ctx *cordis.Context, _ any) error {
			_, err := ctx.Effect("x", func() (cordis.Dispose, error) {
				return func() { log = append(log, "app-") }, nil
			})
			return err
		},
	}
	h.run(func(ctx *cordis.Context) {
		db, _ := ctx.Plugin(provider, nil)
		_ = db
		ctx.Plugin(consumer, nil)
	})
	h.app.Close()
	if fmt.Sprint(log) != fmt.Sprint([]string{"app-"}) {
		t.Fatalf("close should cascade disposal: %v", log)
	}
}

func TestCheckFunction(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	// 提供者以 check 声明健康度：check 失败时服务虽已注册，
	// 依赖仍视为未满足（checkImpl 在 notify 时重新评估）。
	healthy := false
	provider := &cordis.Plugin{
		Name: "db",
		Apply: func(ctx *cordis.Context, _ any) error {
			_, err := ctx.Provide("db", "conn", func() bool { return healthy })
			return err
		},
	}
	var activated int
	consumer := &cordis.Plugin{
		Inject: map[string]any{"db": nil},
		Apply:  func(*cordis.Context, any) error { activated++; return nil },
	}
	var db *cordis.Fiber
	h.run(func(ctx *cordis.Context) {
		db, _ = ctx.Plugin(provider, nil)
		ctx.Plugin(consumer, nil)
	})
	if activated != 0 {
		t.Fatalf("check=false must block activation: %d", activated)
	}

	// 健康度翻转 + 提供者热重载（notify 触发依赖重新评估）→ 依赖者激活。
	healthy = true
	h.run(func(ctx *cordis.Context) {
		if err := db.Update(nil); err != nil {
			t.Fatal(err)
		}
	})
	if activated != 1 {
		t.Fatalf("check=true should activate dependant: %d", activated)
	}
}

// 回归：调度队列必须无界。单个调度任务内部触发的投递
// （每次 fiber 注册排入一次 pump）超过旧实现的固定容量
// 1024 时，唯一消费者正忙于执行当前任务，投递方会永久
// 阻塞并死锁。此处在单个任务内注册 1100 个 fiber 复现。
func TestSchedulerUnboundedQueue(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.app.DoSync(func(ctx *cordis.Context) {
			for i := 0; i < 1100; i++ {
				p := &cordis.Plugin{
					Name:  fmt.Sprintf("p%d", i),
					Apply: func(*cordis.Context, any) error { return nil },
				}
				if _, err := ctx.Plugin(p, nil); err != nil {
					t.Errorf("plugin %d: %v", i, err)
					return
				}
			}
		})
		h.app.Wait()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock: >1024 posts from a single scheduler task")
	}
}

// 回归（L-1）：撤销回调 panic 必须被记录，且不阻断其余撤销步骤。
func TestDisposePanicLogged(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	var logged []string
	h.app.Logger().Error = func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	var order []string
	p := &cordis.Plugin{
		Name: "panicky",
		Apply: func(ctx *cordis.Context, _ any) error {
			if _, err := ctx.Effect("boom", func() (cordis.Dispose, error) {
				return func() { panic("dispose exploded") }, nil
			}); err != nil {
				return err
			}
			_, err := ctx.Effect("after", func() (cordis.Dispose, error) {
				return func() { order = append(order, "after-") }, nil
			})
			return err
		},
	}
	var f *cordis.Fiber
	h.run(func(ctx *cordis.Context) {
		f, _ = ctx.Plugin(p, nil)
	})
	h.run(func(ctx *cordis.Context) {
		f.Dispose()
	})

	// LIFO：after 先撤销、boom 后撤销并 panic——panic 不推翻已完成的步骤。
	if fmt.Sprint(order) != fmt.Sprint([]string{"after-"}) {
		t.Fatalf("panic must not break remaining disposals: %v", order)
	}
	found := false
	for _, line := range logged {
		if strings.Contains(line, "dispose panic") {
			found = true
		}
	}
	if !found {
		t.Fatalf("dispose panic must be logged, got %v", logged)
	}
}

// 回归（L-2）：服务撤销窗口（提供者 state=unloading、自身服务表
// 尚未清理）内，未声明依赖的旁观 fiber 不得经 Get 读到该实现；
// 提供者自身的撤销路径仍可访问自己的服务。
func TestServiceHiddenWhileProviderUnloading(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	var bystanderCtx *cordis.Context
	var (
		active struct {
			v  any
			ok bool
		}
		duringUnload struct {
			v  any
			ok bool
		}
		selfView struct {
			v  any
			ok bool
		}
	)

	bystander := &cordis.Plugin{
		Name: "bystander",
		Apply: func(ctx *cordis.Context, _ any) error {
			bystanderCtx = ctx // 未声明对 db 的依赖
			return nil
		},
	}
	provider := &cordis.Plugin{
		Name: "db",
		Apply: func(ctx *cordis.Context, _ any) error {
			if _, err := ctx.Provide("db", "conn", nil); err != nil {
				return err
			}
			if _, err := ctx.Plugin(bystander, nil); err != nil {
				return err
			}
			// 卸载期探测：此刻提供者尚未完成效果回收。
			_, err := ctx.Effect("probe", func() (cordis.Dispose, error) {
				return func() {
					duringUnload.v, duringUnload.ok = bystanderCtx.Get("db")
					selfView.v, selfView.ok = ctx.Get("db")
				}, nil
			})
			return err
		},
	}

	var db *cordis.Fiber
	h.run(func(ctx *cordis.Context) {
		db, _ = ctx.Plugin(provider, nil)
	})
	h.run(func(ctx *cordis.Context) {
		active.v, active.ok = bystanderCtx.Get("db")
	})
	if !active.ok || active.v != "conn" {
		t.Fatalf("active provider should be visible: %v %v", active.v, active.ok)
	}

	h.run(func(ctx *cordis.Context) {
		db.Dispose()
	})
	if duringUnload.ok {
		t.Fatalf("half-reverted service must be invisible to bystanders, got %v", duringUnload.v)
	}
	if !selfView.ok || selfView.v != "conn" {
		t.Fatalf("provider must keep access to its own service while unloading: %v %v", selfView.v, selfView.ok)
	}
}

// 回归（L-3）：Wait / DoSync 报告收敛与执行结果，不再静默吞掉。
func TestWaitReportsConvergence(t *testing.T) {
	app := cordis.New()
	if !app.Wait() {
		t.Fatal("fresh app should be settled")
	}

	var (
		ran bool
		err error
	)
	if !app.DoSync(func(ctx *cordis.Context) {
		ran = true
		_, err = ctx.Provide("s", 1, nil)
	}) {
		t.Fatal("DoSync should report the task ran")
	}
	if !ran || err != nil {
		t.Fatalf("provide failed: ran=%v err=%v", ran, err)
	}
	if !app.Wait() {
		t.Fatal("app should settle after a provide")
	}

	app.Close()
	if app.Wait() {
		t.Fatal("Wait must report false once the scheduler is stopped")
	}
	if app.DoSync(func(*cordis.Context) {}) {
		t.Fatal("DoSync must report false once the scheduler is stopped")
	}
}

// 压力：多个外部 goroutine 并发混合调用 Do（异步）与 DoSync（阻塞），
// 校验三条不变量：
//  1. 任务不丢失、不重复——执行数恰等于投递数；
//  2. 调度器严格串行——任一时刻在飞任务数为 1（免锁模型的前提）；
//  3. DoSync 在调度器存活期间一律报告已执行。
//
// 该用例是 -race 的主要用武之地：调度器内部队列由互斥量保护，
// 任何遗漏的同步都会在此暴露。
func TestConcurrentExternalCalls(t *testing.T) {
	const (
		goroutines   = 8
		perGoroutine = 50
		total        = goroutines * perGoroutine
	)

	h := newHarness(t)
	defer h.app.Close()

	var executed, syncOK, inFlight, maxDepth atomic.Int64
	task := func(*cordis.Context) {
		cur := inFlight.Add(1)
		for {
			old := maxDepth.Load()
			if cur <= old || maxDepth.CompareAndSwap(old, cur) {
				break
			}
		}
		executed.Add(1)
		inFlight.Add(-1)
	}

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				if i%2 == 0 {
					h.app.Do(task)
					continue
				}
				if h.app.DoSync(task) {
					syncOK.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if !h.app.Wait() {
		t.Fatal("system did not converge")
	}
	if got := executed.Load(); got != total {
		t.Fatalf("executed %d tasks, want %d (tasks lost or duplicated)", got, total)
	}
	if got, want := syncOK.Load(), int64(total/2); got != want {
		t.Fatalf("DoSync reported success %d times, want %d", got, want)
	}
	if got := maxDepth.Load(); got != 1 {
		t.Fatalf("scheduler must run tasks serially, observed %d concurrent tasks", got)
	}
}

// 压力：外部 goroutine 并发注册组件。注册表写入与 fiber 装载
// 全部经调度器串行化，最终每个实例都必须完成加载且无遗漏。
func TestConcurrentPluginRegistration(t *testing.T) {
	const (
		goroutines   = 8
		perGoroutine = 20
	)

	h := newHarness(t)
	defer h.app.Close()

	registered := make([][]*cordis.Fiber, goroutines)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			fs := make([]*cordis.Fiber, 0, perGoroutine)
			for i := 0; i < perGoroutine; i++ {
				p := &cordis.Plugin{
					Name:  fmt.Sprintf("g%d-p%d", g, i),
					Apply: func(*cordis.Context, any) error { return nil },
				}
				var f *cordis.Fiber
				var err error
				if !h.app.DoSync(func(ctx *cordis.Context) {
					f, err = ctx.Plugin(p, nil)
				}) {
					t.Errorf("scheduler stopped during registration")
					return
				}
				if err != nil {
					t.Errorf("plugin g%d-p%d: %v", g, i, err)
					return
				}
				fs = append(fs, f)
			}
			registered[g] = fs
		}(g)
	}
	wg.Wait()

	if !h.app.Wait() {
		t.Fatal("system did not converge")
	}
	count := 0
	for _, fs := range registered {
		for _, f := range fs {
			if f == nil {
				t.Fatal("registration returned a nil fiber")
			}
			if s := f.State(); s != cordis.StateActive {
				t.Fatalf("concurrently registered fiber should be active, got %s", s)
			}
			count++
		}
	}
	if want := goroutines * perGoroutine; count != want {
		t.Fatalf("registered %d fibers, want %d", count, want)
	}
}

// 压力：Close 与外部投递并发。封闭投递口后到达的任务被丢弃
// （设计如此，见 scheduler.post），但整个过程不得 panic 或死锁；
// Close 必须保持幂等。
func TestConcurrentCloseWithPosts(t *testing.T) {
	app := cordis.New()

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				app.Do(func(*cordis.Context) {})
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		app.Close()
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock: Close racing with external posts")
	}

	// 幂等：调度器已停止，重复 Close 不得 panic 或阻塞。
	app.Close()
	if app.DoSync(func(*cordis.Context) {}) {
		t.Fatal("DoSync must report false after Close")
	}
}

// ---------------------------------------------------------------------------
// 洋葱式分发（waterfall）与事件注册选项
// ---------------------------------------------------------------------------

// nextOf 取出 waterfall 链上追加在参数末尾的 next。
// （链装配保证它必然存在且类型正确，断言失败即实现与测试脱节。）
func nextOf(args []any) func() any {
	if len(args) == 0 {
		panic("waterfall: listener received no next argument")
	}
	next, ok := args[len(args)-1].(func() any)
	if !ok {
		panic(fmt.Sprintf("waterfall: last argument is %T, want func() any", args[len(args)-1]))
	}
	return next
}

func TestWaterfallChain(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	var order []string
	h.run(func(ctx *cordis.Context) {
		ctx.On("test/waterfall", func(_ *cordis.Context, args ...any) any {
			order = append(order, "a")
			return args[0].(int) + nextOf(args)().(int)
		})
		ctx.On("test/waterfall", func(_ *cordis.Context, args ...any) any {
			order = append(order, "b")
			return args[0].(int) + nextOf(args)().(int)
		})
		// terminal 是链尾的默认行为：两个监听器各自 +2 之和。
		got := ctx.Waterfall("test/waterfall", func() any { return 2 }, 1)
		if got != 4 {
			t.Fatalf("chain result: %v, want 4", got)
		}
		if fmt.Sprint(order) != fmt.Sprint([]string{"a", "b"}) {
			t.Fatalf("chain order: %v", order)
		}
	})
}

func TestWaterfallShortCircuit(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	// 不调用 next 的监听器终止分发：链上其后的一切（含 terminal）都不执行。
	var order []string
	h.run(func(ctx *cordis.Context) {
		ctx.On("test/waterfall", func(_ *cordis.Context, args ...any) any {
			order = append(order, "a")
			return args[0].(int) + nextOf(args)().(int)
		})
		ctx.On("test/waterfall", func(_ *cordis.Context, args ...any) any {
			order = append(order, "stop")
			return args[0].(int)
		})
		ctx.On("test/waterfall", func(_ *cordis.Context, args ...any) any {
			order = append(order, "unreachable")
			return nextOf(args)()
		})
		got := ctx.Waterfall("test/waterfall", func() any {
			order = append(order, "terminal")
			return 2
		}, 1)
		// 完整链会是 1+2=3；short-circuit 后链尾不可达，只剩 1+1=2。
		if got != 2 {
			t.Fatalf("short-circuit result: %v, want 2", got)
		}
		if fmt.Sprint(order) != fmt.Sprint([]string{"a", "stop"}) {
			t.Fatalf("short-circuit order: %v", order)
		}
	})

	// terminal 可省略：链走完即返回 nil。
	h.run(func(ctx *cordis.Context) {
		ctx.On("test/empty", func(_ *cordis.Context, args ...any) any { return nextOf(args)() })
		if got := ctx.Waterfall("test/empty", nil, 1); got != nil {
			t.Fatalf("nil terminal: %v, want nil", got)
		}
	})
}

func TestWaterfallDuplicateNextPanics(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	var terminalCalls int
	err := h.doSync(func(ctx *cordis.Context) (r any) {
		ctx.On("test/waterfall", func(_ *cordis.Context, args ...any) any {
			next := nextOf(args)
			next()
			return next() // 同一帧内第二次调用 → 编程错误
		})
		defer func() {
			r = recover()
		}()
		return ctx.Waterfall("test/waterfall", func() any { terminalCalls++; return 1 }, 1)
	})
	if !errors.Is(asError(err), cordis.ErrDuplicateNext) {
		t.Fatalf("duplicate next: %v, want ErrDuplicateNext", err)
	}
	if terminalCalls != 1 {
		t.Fatalf("terminal ran %d times, want 1", terminalCalls)
	}
}

func TestWaterfallStaleNextPanics(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	// 把 next 存到外层帧再调用，等同于重复调用：外层帧的控制权
	// 早已交给下一个监听器，续用会让链被走两遍。
	var calls []string
	err := h.doSync(func(ctx *cordis.Context) (r any) {
		var outerNext func() any
		ctx.On("test/waterfall", func(_ *cordis.Context, args ...any) any {
			outerNext = nextOf(args)
			calls = append(calls, "first")
			return outerNext()
		})
		ctx.On("test/waterfall", func(_ *cordis.Context, args ...any) any {
			calls = append(calls, "second")
			return nextOf(args)()
		})
		defer func() {
			r = recover()
		}()
		return ctx.Waterfall("test/waterfall", func() any {
			calls = append(calls, "terminal")
			return outerNext()
		}, 1)
	})
	if !errors.Is(asError(err), cordis.ErrDuplicateNext) {
		t.Fatalf("stale next: %v, want ErrDuplicateNext", err)
	}
	if fmt.Sprint(calls) != fmt.Sprint([]string{"first", "second", "terminal"}) {
		t.Fatalf("stale next calls: %v", calls)
	}
}

// asError 把 recover 得到的值还原为 error（非 error 的 panic 值返回 nil）。
func asError(v any) error {
	err, _ := v.(error)
	return err
}

// ---------------------------------------------------------------------------
// 事件注册选项：prepend / global
// ---------------------------------------------------------------------------

func TestEventPrependOrder(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	var order []string
	h.run(func(ctx *cordis.Context) {
		ctx.On("e", func(*cordis.Context, ...any) any { order = append(order, "tail"); return nil })
		ctx.On("e", func(*cordis.Context, ...any) any { order = append(order, "head1"); return nil },
			cordis.ListenOptions{Prepend: true})
		ctx.On("e", func(*cordis.Context, ...any) any { order = append(order, "head2"); return nil },
			cordis.ListenOptions{Prepend: true})
		ctx.Emit("e")
	})
	if fmt.Sprint(order) != fmt.Sprint([]string{"head2", "head1", "tail"}) {
		t.Fatalf("prepend order: %v", order)
	}
}

// TestEventDispatchSnapshot 分发在监听器列表的**快照**上进行：
// 分发期间的自注销不得跳过相邻监听器，新注册的监听器本次不参与。
func TestEventDispatchSnapshot(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	var (
		order []string
		late  int
	)
	h.run(func(ctx *cordis.Context) {
		var disposeA cordis.Dispose
		d, err := ctx.On("s", func(c *cordis.Context, _ ...any) any {
			order = append(order, "A")
			disposeA() // 自注销：原地压缩监听器桶
			_, _ = c.On("s", func(*cordis.Context, ...any) any { late++; return nil })
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		disposeA = d
		if _, err := ctx.On("s", func(*cordis.Context, ...any) any {
			order = append(order, "B")
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		ctx.Emit("s")
		if fmt.Sprint(order) != fmt.Sprint([]string{"A", "B"}) {
			t.Fatalf("dispatch snapshot order: %v", order)
		}
		if late != 0 {
			t.Fatalf("listener registered during dispatch ran %d times, want 0", late)
		}

		// 第二次分发：A 已注销，分发中登记的监听器此时才生效。
		order = nil
		ctx.Emit("s")
		if fmt.Sprint(order) != fmt.Sprint([]string{"B"}) || late != 1 {
			t.Fatalf("second dispatch: %v late=%d", order, late)
		}
	})
}

// TestEventGlobalBypassesRealmFilter 域内事件（internal/service）默认
// 只送达同域监听器；Global 监听器绕过域过滤，观察全部域。
func TestEventGlobalBypassesRealmFilter(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	var scoped, global int
	h.run(func(ctx *cordis.Context) {
		if _, err := ctx.On("internal/service", func(*cordis.Context, ...any) any {
			scoped++
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := ctx.On("internal/service", func(*cordis.Context, ...any) any {
			global++
			return nil
		}, cordis.ListenOptions{Global: true}); err != nil {
			t.Fatal(err)
		}
	})

	// 默认域的服务：两种监听器都应收到。
	h.run(func(ctx *cordis.Context) {
		if _, err := ctx.Provide("db", "root-dsn", nil); err != nil {
			t.Fatal(err)
		}
	})
	if scoped != 1 || global != 1 {
		t.Fatalf("default realm: scoped=%d global=%d, want 1/1", scoped, global)
	}

	// 隔离域的服务：根域监听器看不见，Global 监听器看得见。
	h.run(func(ctx *cordis.Context) {
		if _, err := ctx.Isolate("db", "tenant-a").Provide("db", "tenant-dsn", nil); err != nil {
			t.Fatal(err)
		}
	})
	if scoped != 1 {
		t.Fatalf("scoped listener saw %d events from another realm, want 1 (unchanged)", scoped)
	}
	if global != 2 {
		t.Fatalf("global listener saw %d events, want 2", global)
	}
}

// ---------------------------------------------------------------------------
// internal/update：fiber 局部更新钩子
// ---------------------------------------------------------------------------

// TestUpdateEventHookScope 非 global 的 internal/update 监听器只拦得住
// **注册它的那个 fiber**；global 的则对所有 fiber 生效。
//
// 判别顺序很重要：先更新**另一个** fiber——被路由到局部链的监听器
// 此时必须毫无反应。若它落进全局桶，这一次就会误触发（而它随本
// fiber 卸载而回收后，再更新本 fiber 反而看不出差别）。
func TestUpdateEventHookScope(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	var (
		applied  []int
		scoped   int
		global   int
		firstCfg = 1
	)
	// ownerCtx 是首个实例自己的上下文（在 Apply 内捕获）。
	var ownerCtx *cordis.Context
	p := &cordis.Plugin{
		Name: "p",
		Apply: func(ctx *cordis.Context, config any) error {
			applied = append(applied, config.(int))
			if config.(int) != firstCfg {
				return nil
			}
			ownerCtx = ctx
			// 非 global：按官方语义应被路由到本 fiber 的局部钩子链。
			_, err := ctx.On("internal/update", func(_ *cordis.Context, args ...any) any {
				scoped++
				return nextOf(args)()
			})
			return err
		},
	}

	var first, second *cordis.Fiber
	h.run(func(ctx *cordis.Context) {
		if _, err := ctx.On("internal/update", func(_ *cordis.Context, args ...any) any {
			global++
			return nextOf(args)()
		}, cordis.ListenOptions{Global: true}); err != nil {
			t.Fatal(err)
		}
		var err error
		first, err = ctx.Plugin(p, firstCfg)
		if err != nil {
			t.Fatal(err)
		}
		second, err = ctx.Plugin(p, 2)
		if err != nil {
			t.Fatal(err)
		}
	})
	if ownerCtx == nil {
		t.Fatal("first instance never captured its context")
	}

	// 另一个 fiber 的更新：局部钩子不得介入，global 钩子应当到场。
	h.run(func(_ *cordis.Context) { _ = second.Update(22) })
	if applied[len(applied)-1] != 22 {
		t.Fatalf("second fiber did not reload: %v", applied)
	}
	if scoped != 0 {
		t.Fatalf("scoped hook fired for a foreign fiber: %d", scoped)
	}
	if global != 1 {
		t.Fatalf("global hook count after foreign update: %d, want 1", global)
	}

	// 本 fiber 的更新：局部钩子与 global 钩子各一次，重启照常发生。
	h.run(func(_ *cordis.Context) { _ = first.Update(11) })
	if applied[len(applied)-1] != 11 {
		t.Fatalf("first fiber did not reload: %v", applied)
	}
	if scoped != 1 {
		t.Fatalf("scoped hook count: %d, want 1", scoped)
	}
	if global != 2 {
		t.Fatalf("global hook count: %d, want 2", global)
	}
}

// TestUpdateHookNoSaveAndVeto noSave 标志随链传递；钩子返回 false 即
// 接管本次更新：既不重启组件，也不替换配置。
func TestUpdateHookNoSaveAndVeto(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	var (
		applied []string
		flags   []bool
	)
	p := &cordis.Plugin{
		Name: "p",
		Apply: func(_ *cordis.Context, config any) error {
			applied = append(applied, config.(string))
			return nil
		},
	}

	var f *cordis.Fiber
	h.run(func(ctx *cordis.Context) {
		var err error
		f, err = ctx.Plugin(p, "a")
		if err != nil {
			t.Fatal(err)
		}
		f.OnUpdate(func(_ any, noSave bool) bool {
			flags = append(flags, noSave)
			return true
		})
	})
	// 每次 Update 都要等它收敛到下一次 Apply，否则同一批任务里多次
	// Update 只会以最后一次配置加载一次。
	h.run(func(_ *cordis.Context) { _ = f.Update("b") })       // 组件自更新
	h.run(func(_ *cordis.Context) { _ = f.Update("c", true) }) // 宿主发起
	if fmt.Sprint(flags) != fmt.Sprint([]bool{false, true}) {
		t.Fatalf("noSave flags: %v", flags)
	}
	if fmt.Sprint(applied) != fmt.Sprint([]string{"a", "b", "c"}) {
		t.Fatalf("reload sequence: %v", applied)
	}

	// 接管：钩子返回 false，重启与配置替换都不发生。
	h.run(func(_ *cordis.Context) {
		f.OnUpdate(func(any, bool) bool { return false })
		if err := f.Update("d"); err != nil {
			t.Fatal(err)
		}
	})
	if fmt.Sprint(applied) != fmt.Sprint([]string{"a", "b", "c"}) {
		t.Fatalf("vetoed update still reloaded: %v", applied)
	}
	if f.Config() != "c" {
		t.Fatalf("vetoed update replaced config: %v", f.Config())
	}
	if f.State() != cordis.StateActive {
		t.Fatalf("vetoed update changed state: %v", f.State())
	}
}

// TestUpdateHookDuplicateNextIsReported 钩子链中的 next 误用不得击穿
// 调度器：Update 把它收敛为错误返回，应用继续可用。
func TestUpdateHookDuplicateNextIsReported(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	p := &cordis.Plugin{
		Name:  "p",
		Apply: func(*cordis.Context, any) error { return nil },
	}

	var f *cordis.Fiber
	h.run(func(ctx *cordis.Context) {
		if _, err := ctx.On("internal/update", func(_ *cordis.Context, args ...any) any {
			next := nextOf(args)
			next()
			return next()
		}, cordis.ListenOptions{Global: true}); err != nil {
			t.Fatal(err)
		}
		var err error
		f, err = ctx.Plugin(p, "a")
		if err != nil {
			t.Fatal(err)
		}
	})

	var err error
	h.run(func(_ *cordis.Context) { err = f.Update("b") })
	if !errors.Is(err, cordis.ErrDuplicateNext) {
		t.Fatalf("update error: %v, want ErrDuplicateNext", err)
	}
	if f.State() != cordis.StateActive {
		t.Fatalf("fiber state after bad hook: %v", f.State())
	}
}

// ---------------------------------------------------------------------------
// internal/listener：注册期扩展点
// ---------------------------------------------------------------------------

// TestInternalListenerTakesOver 监听器可在注册期接管注册：返回自定义
// Dispose 后，被注册的监听器不再进入事件桶，注销由接管方负责。
func TestInternalListenerTakesOver(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	var (
		handled int
		fired   int
		revoked int
	)
	h.run(func(ctx *cordis.Context) {
		if _, err := ctx.On("internal/listener", func(_ *cordis.Context, args ...any) any {
			name, _ := args[0].(string)
			if name != "custom/event" {
				return nil
			}
			handled++
			return cordis.Dispose(func() { revoked++ })
		}); err != nil {
			t.Fatal(err)
		}

		d, err := ctx.On("custom/event", func(*cordis.Context, ...any) any { fired++; return nil })
		if err != nil {
			t.Fatal(err)
		}
		ctx.Emit("custom/event")
		if handled != 1 {
			t.Fatalf("internal/listener not dispatched: %d", handled)
		}
		if fired != 0 {
			t.Fatalf("taken-over listener still registered into the bucket: %d", fired)
		}
		d() // 接管方返回的注销函数原样转交调用方
		if revoked != 1 {
			t.Fatalf("custom dispose: %d", revoked)
		}
	})
}

// BenchmarkServiceNotify 度量服务上下线通知的代价：1000 个未声明
// 该依赖的 Fiber 在场时，单次 provide/dispose 的耗时。
// 引入倒排索引前该路径逐 fiber 全量扫描（O(全部 Fiber)）。
func BenchmarkServiceNotify(b *testing.B) {
	app := cordis.New()
	defer app.Close()

	unrelated := &cordis.Plugin{
		Name:   "unrelated",
		Inject: map[string]any{"never": nil},
		Apply:  func(*cordis.Context, any) error { return nil },
	}
	var setupErr error
	app.DoSync(func(ctx *cordis.Context) {
		for i := 0; i < 1000; i++ {
			if _, err := ctx.Plugin(unrelated, nil); err != nil {
				setupErr = err
				return
			}
		}
	})
	if setupErr != nil {
		b.Fatal(setupErr)
	}
	app.Wait()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var perr error
		app.DoSync(func(ctx *cordis.Context) {
			d, err := ctx.Provide("other", i, nil)
			if err != nil {
				perr = err
				return
			}
			d()
		})
		if perr != nil {
			b.Fatal(perr)
		}
		app.Wait()
	}
}
