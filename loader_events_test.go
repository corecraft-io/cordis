package cordis_test

import (
	"fmt"
	"strings"
	"testing"

	cordis "github.com/corecraft-io/cordis"
)

// TestLoaderEventSurface loader 的四个扩展点事件：entry-init（构造期）、
// patch-context（洋葱链，环绕上下文重建）、partial-dispose（入口存活但
// 实例被替换/热重载，以及被分组协调摘除两种 active 取值）、
// config-update（供配置写入方通知）。
func TestLoaderEventSurface(t *testing.T) {
	h := newLoaderHarness(t)
	defer h.close()

	var (
		inits    []string
		patched  []string
		partials []string
		updates  int
	)
	h.app.DoSync(func(ctx *cordis.Context) {
		mustOn(t, ctx, "loader/entry-init", func(_ *cordis.Context, args ...any) any {
			e := args[0].(*cordis.Entry)
			inits = append(inits, e.ID())
			return nil
		})
		// patch-context 是洋葱链：监听器可在 next 前后各做一步。
		mustOn(t, ctx, "loader/patch-context", func(_ *cordis.Context, args ...any) any {
			e := args[0].(*cordis.Entry)
			patched = append(patched, "before:"+e.Options().Name)
			result := args[len(args)-1].(func() any)()
			patched = append(patched, "after:"+e.Options().Name)
			return result
		})
		mustOn(t, ctx, "loader/partial-dispose", func(_ *cordis.Context, args ...any) any {
			e := args[0].(*cordis.Entry)
			legacy := args[1].(cordis.EntryOptions)
			active := args[2].(bool)
			partials = append(partials, fmt.Sprintf("%s:%v:%v", e.Options().Name, legacy.Config, active))
			return nil
		})
		mustOn(t, ctx, "loader/config-update", func(*cordis.Context, ...any) any {
			updates++
			return nil
		})
	})

	h.plugins["leaf"] = &cordis.Plugin{
		Name:  "leaf",
		Apply: func(*cordis.Context, any) error { return nil },
	}

	h.loader.Load([]cordis.EntryOptions{{ID: "a", Name: "leaf", Config: 1}})
	if fmt.Sprint(inits) != fmt.Sprint([]string{"a"}) {
		t.Fatalf("entry-init: %v", inits)
	}
	if fmt.Sprint(patched) != fmt.Sprint([]string{"before:leaf", "after:leaf"}) {
		t.Fatalf("patch-context chain: %v", patched)
	}

	// 仅配置变化 → 入口存活，发 active=true 的 partial-dispose。
	h.loader.Load([]cordis.EntryOptions{{ID: "a", Name: "leaf", Config: 2}})
	if fmt.Sprint(partials) != fmt.Sprint([]string{"leaf:1:true"}) {
		t.Fatalf("partial-dispose(active=true): %v", partials)
	}

	// 分组协调摘掉子入口 → active=false。
	h.loader.Load([]cordis.EntryOptions{
		{ID: "g", Name: "group", Group: true, Config: []cordis.EntryOptions{
			{ID: "c1", Name: "leaf"},
		}},
	})
	h.loader.Load([]cordis.EntryOptions{
		{ID: "g", Name: "group", Group: true, Config: []cordis.EntryOptions{}},
	})
	last := partials[len(partials)-1]
	if last != "leaf:<nil>:false" {
		t.Fatalf("partial-dispose(active=false): %v", partials)
	}

	h.loader.NotifyConfigUpdate()
	if updates != 1 {
		t.Fatalf("config-update: %d", updates)
	}
}

// TestLoaderEntriesAndWait Entries() 覆盖全树（含分组子入口）且顺序稳定；
// 收敛后 Pending() 为空，Wait() 报告收敛。
func TestLoaderEntriesAndWait(t *testing.T) {
	h := newLoaderHarness(t)
	defer h.close()

	h.plugins["leaf"] = &cordis.Plugin{
		Name:  "leaf",
		Apply: func(*cordis.Context, any) error { return nil },
	}
	h.plugins["waiter"] = &cordis.Plugin{
		Name:   "waiter",
		Inject: map[string]any{"missing": nil},
		Apply:  func(*cordis.Context, any) error { return nil },
	}

	h.loader.Load([]cordis.EntryOptions{
		{ID: "b", Name: "leaf"},
		{ID: "g", Name: "group", Group: true, Config: []cordis.EntryOptions{
			{ID: "c", Name: "leaf"},
		}},
		// 依赖永远不满足：这是**稳定态**，不算 pending。
		{ID: "a", Name: "waiter"},
	})

	ids := []string{}
	for _, e := range h.loader.Tree().Entries() {
		ids = append(ids, e.Options().ID)
	}
	if fmt.Sprint(ids) != fmt.Sprint([]string{"a", "b", "c", "g"}) {
		t.Fatalf("Entries(): %v (want sorted, group children included)", ids)
	}
	if p := h.loader.Tree().Pending(); len(p) != 0 {
		t.Fatalf("Pending() after convergence: %v", p)
	}
	if !h.loader.Wait() {
		t.Fatal("Wait() must report convergence")
	}
}

// TestLoaderLocate 由实例反查入口：分组内的实例应解析到自己的入口
// （全路径 ID），而不是外层分组。
func TestLoaderLocate(t *testing.T) {
	h := newLoaderHarness(t)
	defer h.close()

	h.plugins["leaf"] = &cordis.Plugin{
		Name:  "leaf",
		Apply: func(*cordis.Context, any) error { return nil },
	}
	h.loader.Load([]cordis.EntryOptions{
		{ID: "g", Name: "group", Group: true, Config: []cordis.EntryOptions{
			{ID: "c", Name: "leaf"},
		}},
	})

	child, err := h.loader.Tree().Resolve("g:c")
	if err != nil {
		t.Fatal(err)
	}
	if got := h.loader.Locate(child.Fiber()); got != child {
		t.Fatalf("Locate(child fiber): %v", got)
	}
	if got := h.loader.Locate(child.Fiber()).ID(); got != "g:c" {
		t.Fatalf("Locate id: %q, want g:c", got)
	}

	// 入口内的子插件（非入口根实例）也应定位回该入口。
	var nested *cordis.Fiber
	nestedPlugin := &cordis.Plugin{
		Name:  "nested",
		Apply: func(*cordis.Context, any) error { return nil },
	}
	h.plugins["holder"] = &cordis.Plugin{
		Name: "holder",
		Apply: func(ctx *cordis.Context, _ any) error {
			f, err := ctx.Plugin(nestedPlugin, nil)
			nested = f
			return err
		},
	}
	h.loader.Load([]cordis.EntryOptions{
		{ID: "g", Name: "group", Group: true, Config: []cordis.EntryOptions{
			{ID: "c", Name: "leaf"},
		}},
		{ID: "holder", Name: "holder"},
	})
	if nested == nil {
		t.Fatal("nested plugin not instantiated")
	}
	if got := h.loader.Locate(nested); got == nil || got.Options().ID != "holder" {
		t.Fatalf("Locate(nested fiber): %v", got)
	}
}

// TestLoaderBuiltins "cordis:" 前缀命中内置插件表；未知名字报错。
func TestLoaderBuiltins(t *testing.T) {
	h := newLoaderHarness(t)
	defer h.close()

	rec := &logRecorder{}
	h.app.Logger().Capture(rec.record)

	leaf := &cordis.Plugin{
		Name:  "leaf",
		Apply: func(*cordis.Context, any) error { return nil },
	}
	h.loader.Builtins(map[string]*cordis.Plugin{"leaf": leaf})

	h.loader.Load([]cordis.EntryOptions{
		{ID: "g", Name: "cordis:group", Group: true, Config: []cordis.EntryOptions{}},
		{ID: "l", Name: "cordis:leaf"},
	})
	e, err := h.loader.Tree().Resolve("l")
	if err != nil {
		t.Fatal(err)
	}
	if e.Fiber() == nil || e.Fiber().State() != cordis.StateActive {
		t.Fatalf("builtin entry: %v", e.Fiber())
	}

	// 未知内置插件：入口保留但无实例，并记一条错误（前缀走不通时不
	// 应回落给外部解析器）。
	h.loader.Load([]cordis.EntryOptions{
		{ID: "x", Name: "cordis:nope"},
	})
	xe, _ := h.loader.Tree().Resolve("x")
	if xe.Fiber() != nil {
		t.Fatalf("unknown builtin must not load: %v", xe.Fiber())
	}
	found := false
	for _, m := range rec.all() {
		if m.Level == cordis.LevelError && strings.Contains(m.Text, `unknown builtin plugin "cordis:nope"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("unknown builtin must be reported: %v", rec.plain())
	}
}

// TestLoaderLogs SetLogs 打开后按 apply / reload / unload 记录结构变更，
// 走名为 "loader" 的日志器；分组入口自身不记录。
func TestLoaderLogs(t *testing.T) {
	h := newLoaderHarness(t)
	defer h.close()

	rec := &logRecorder{}
	h.app.Logger().Capture(rec.record)

	h.plugins["leaf"] = &cordis.Plugin{
		Name:  "leaf",
		Apply: func(*cordis.Context, any) error { return nil },
	}
	h.loader.Load([]cordis.EntryOptions{
		{ID: "g", Name: "group", Group: true, Config: []cordis.EntryOptions{
			{ID: "c", Name: "leaf", Config: 1},
		}},
	})
	if len(rec.all()) != 0 {
		t.Fatalf("logs must be off by default: %v", rec.plain())
	}

	h.loader.SetLogs(true)
	h.loader.Load([]cordis.EntryOptions{
		{ID: "g", Name: "group", Group: true, Config: []cordis.EntryOptions{
			{ID: "c", Name: "leaf", Config: 2},
		}},
	})
	got := rec.plain()
	if fmt.Sprint(got) != fmt.Sprint([]string{"reload plugin leaf"}) {
		t.Fatalf("reload log: %v", got)
	}
	for _, m := range rec.all() {
		if m.Name != "loader" {
			t.Fatalf("logs must use the loader logger: %+v", m)
		}
	}

	// 移除子入口 → unload；分组入口自身不记录 (apply 时已跳过 group)。
	rec.msgs = nil
	h.loader.Load([]cordis.EntryOptions{})
	if got := rec.plain(); fmt.Sprint(got) != fmt.Sprint([]string{"unload plugin leaf"}) {
		t.Fatalf("unload log: %v", got)
	}
}

func mustOn(t *testing.T, ctx *cordis.Context, name string, l cordis.Listener) {
	t.Helper()
	if _, err := ctx.On(name, l); err != nil {
		t.Fatal(err)
	}
}

// TestLoaderNestedRealms 嵌套域：分组入口带 isolate 时，组内子入口
// 默认继承该域；子入口自己声明的域（共享标签或私有域）覆盖继承值。
// （对应官方 isolate.spec 的 realm reference / nested realms。）
func TestLoaderNestedRealms(t *testing.T) {
	h := newLoaderHarness(t)
	defer h.close()

	var seen []any
	h.plugins["bar"] = &cordis.Plugin{
		Name: "bar",
		Apply: func(ctx *cordis.Context, config any) error {
			_, err := ctx.Provide("bar", config, nil)
			return err
		},
	}
	h.plugins["foo"] = &cordis.Plugin{
		Name:   "foo",
		Inject: map[string]any{"bar": nil},
		Apply: func(ctx *cordis.Context, _ any) error {
			v, _ := ctx.Get("bar")
			seen = append(seen, v)
			return nil
		},
	}

	alpha := map[string]any{"bar": "alpha"}
	h.loader.Load([]cordis.EntryOptions{
		{ID: "pa", Name: "bar", Config: "A", Isolate: alpha},
		{ID: "pb", Name: "bar", Config: "B", Isolate: map[string]any{"bar": "beta"}},
		{ID: "g", Name: "group", Group: true, Isolate: alpha, Config: []cordis.EntryOptions{
			{ID: "inherit", Name: "foo"}, // 继承分组域 alpha
			{ID: "shared", Name: "foo", Isolate: map[string]any{"bar": "beta"}},
			{ID: "private", Name: "foo", Isolate: map[string]any{"bar": true}},
		}},
	})

	// 继承 alpha → A；共享标签 beta → B；私有域无人提供 → pending。
	if fmt.Sprint(seen) != fmt.Sprint([]any{"A", "B"}) {
		t.Fatalf("nested realms: %v", seen)
	}
	priv, err := h.loader.Tree().Resolve("g:private")
	if err != nil {
		t.Fatal(err)
	}
	if priv.Fiber().State() != cordis.StatePending {
		t.Fatalf("private realm inside a group: %v", priv.Fiber().State())
	}
}

// TestLoaderChangeProviderOrInjector 官方 isolate.spec 的两个特例：
// 换**提供者**所在域、换**依赖者**所在域，都会让依赖者重建并按新域
// 重新解析（旧实例先完全下线）。
func TestLoaderChangeProviderOrInjector(t *testing.T) {
	h := newLoaderHarness(t)
	defer h.close()

	var (
		applied int
		removed int
		seen    []any
	)
	h.plugins["bar"] = &cordis.Plugin{
		Name: "bar",
		Apply: func(ctx *cordis.Context, config any) error {
			_, err := ctx.Provide("bar", config, nil)
			return err
		},
	}
	h.plugins["foo"] = &cordis.Plugin{
		Name:   "foo",
		Inject: map[string]any{"bar": nil},
		Apply: func(ctx *cordis.Context, _ any) error {
			applied++
			v, _ := ctx.Get("bar")
			seen = append(seen, v)
			_, err := ctx.Effect("foo", func() (cordis.Dispose, error) {
				return func() { removed++ }, nil
			})
			return err
		},
	}

	// 提供者先占 alpha 域，依赖者继承分组的 alpha 域。
	h.loader.Load([]cordis.EntryOptions{
		{ID: "pa", Name: "bar", Config: "A", Isolate: map[string]any{"bar": "alpha"}},
		{ID: "pb", Name: "bar", Config: "B", Isolate: map[string]any{"bar": "beta"}},
		{ID: "g", Name: "group", Group: true, Isolate: map[string]any{"bar": "alpha"}, Config: []cordis.EntryOptions{
			{ID: "c", Name: "foo"},
		}},
	})
	if fmt.Sprint(seen) != fmt.Sprint([]any{"A"}) || applied != 1 {
		t.Fatalf("baseline: seen=%v applied=%d", seen, applied)
	}

	// 换提供者所在域（依赖者继承分组域，故随分组一起重建）：
	// 旧实例先下线，再按 beta 重新解析。
	seen = nil
	if err := h.loader.Update("g", cordis.EntryOptions{
		Name:    "group",
		Group:   true,
		Isolate: map[string]any{"bar": "beta"},
		Config: []cordis.EntryOptions{
			{ID: "c", Name: "foo"},
		},
	}, "", -1); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(seen) != fmt.Sprint([]any{"B"}) {
		t.Fatalf("change provider realm: seen=%v, want [B]", seen)
	}
	if applied != 2 || removed != 1 {
		t.Fatalf("change provider realm: applied=%d removed=%d, want 2/1", applied, removed)
	}

	// 换依赖者自己的域：改用共享标签 alpha（提供者 pa 在该域）。
	seen = nil
	if err := h.loader.Update("g:c", cordis.EntryOptions{
		Name:    "foo",
		Isolate: map[string]any{"bar": "alpha"},
	}, "", -1); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(seen) != fmt.Sprint([]any{"A"}) {
		t.Fatalf("change injector realm: seen=%v, want [A]", seen)
	}
	if applied != 3 || removed != 2 {
		t.Fatalf("change injector realm: applied=%d removed=%d, want 3/2", applied, removed)
	}
}
