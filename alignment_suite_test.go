package cordis_test

import (
	"fmt"
	"testing"

	cordis "github.com/corecraft-io/cordis"
)

// ---------------------------------------------------------------------------
// 官方 fiber.spec：更新与依赖重载并发时的收敛
// ---------------------------------------------------------------------------

// TestUpdateWhileDependencyReloads 依赖者与提供者同时 Update（同一批
// 任务里发出两次更新）：两者都只以**最终值**各加载一次，不得出现
// 中间的陈旧组合。
func TestUpdateWhileDependencyReloads(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	var applied [][2]any
	provider := &cordis.Plugin{
		Name: "provider",
		Apply: func(ctx *cordis.Context, config any) error {
			cfg := config.(map[string]any)
			_, err := ctx.Provide("provider", cfg["value"], nil)
			return err
		},
	}
	consumer := &cordis.Plugin{
		Name:   "consumer",
		Inject: map[string]any{"provider": nil},
		Apply: func(ctx *cordis.Context, config any) error {
			cfg := config.(map[string]any)
			v, _ := ctx.Get("provider")
			applied = append(applied, [2]any{v, cfg["mode"]})
			return nil
		},
	}

	var p, c *cordis.Fiber
	h.run(func(ctx *cordis.Context) {
		p, _ = ctx.Plugin(provider, map[string]any{"value": 1})
		c, _ = ctx.Plugin(consumer, map[string]any{"mode": "old"})
	})
	want := [][2]any{{1, "old"}}
	if fmt.Sprint(applied) != fmt.Sprint(want) {
		t.Fatalf("baseline: %v", applied)
	}

	// 同一批任务里两次 Update：最终组合只能是 (2, "new")。
	h.run(func(*cordis.Context) {
		_ = p.Update(map[string]any{"value": 2})
		_ = c.Update(map[string]any{"mode": "new"})
	})
	want = append(want, [2]any{2, "new"})
	if fmt.Sprint(applied) != fmt.Sprint(want) {
		t.Fatalf("concurrent updates: %v, want %v", applied, want)
	}
	if c.State() != cordis.StateActive || p.State() != cordis.StateActive {
		t.Fatalf("states: provider=%v consumer=%v", p.State(), c.State())
	}
	// 两份配置都以最终值生效（不是中间态）。
	if got := c.Config().(map[string]any)["mode"]; got != "new" {
		t.Fatalf("consumer config: %v", got)
	}
	if got := p.Config().(map[string]any)["value"]; got != 2 {
		t.Fatalf("provider config: %v", got)
	}
}

// ---------------------------------------------------------------------------
// 官方 plugin.spec：嵌套插件的注册表与效果快照
// ---------------------------------------------------------------------------

// TestNestedPluginSnapshot 父插件在 Apply 里实例化子插件：注册表会
// 出现两个 Runtime（父子各一），注销父实例后子实例随之消失、
// 注册表收缩——对应官方 nested plugins 的快照对比断言。
func TestNestedPluginSnapshot(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	child := &cordis.Plugin{
		Name: "child",
		Apply: func(ctx *cordis.Context, _ any) error {
			_, err := ctx.Effect("child-effect", func() (cordis.Dispose, error) {
				return func() {}, nil
			})
			return err
		},
	}
	var nested *cordis.Fiber
	parent := &cordis.Plugin{
		Name: "parent",
		Apply: func(ctx *cordis.Context, _ any) error {
			if _, err := ctx.Effect("parent-effect", func() (cordis.Dispose, error) {
				return func() {}, nil
			}); err != nil {
				return err
			}
			f, err := ctx.Plugin(child, nil)
			nested = f
			return err
		},
	}

	var f *cordis.Fiber
	h.run(func(ctx *cordis.Context) { f, _ = ctx.Plugin(parent, nil) })

	names := []string{}
	h.run(func(ctx *cordis.Context) {
		for _, rt := range ctx.Registry().Values() {
			names = append(names, rt.Name())
		}
		if ctx.Registry().Size() != 2 {
			t.Fatalf("registry size: %d, want 2", ctx.Registry().Size())
		}
	})
	if fmt.Sprint(names) != fmt.Sprint([]string{"parent", "child"}) {
		t.Fatalf("registry snapshot: %v", names)
	}
	// 父实例的效果里除了自己的，还有一条 ctx.plugin()——子插件的注册
	// 本身就是父实例的一个效果，这正是"父卸载则子级联注销"的机制。
	want := []string{"parent-effect", "ctx.plugin()"}
	if fmt.Sprint(f.Effects()) != fmt.Sprint(want) {
		t.Fatalf("parent effects: %v, want %v", f.Effects(), want)
	}
	if fmt.Sprint(nested.Effects()) != fmt.Sprint([]string{"child-effect"}) {
		t.Fatalf("nested effects: %v", nested.Effects())
	}

	// 注销父实例：子实例级联注销，注册表收缩为一个（父插件的 Runtime
	// 也随实例清空而被删除）。
	h.run(func(*cordis.Context) { f.Dispose() })
	h.run(func(ctx *cordis.Context) {
		if ctx.Registry().Size() != 0 {
			t.Fatalf("registry after teardown: %d, want 0", ctx.Registry().Size())
		}
		if got := f.Effects(); len(got) != 0 {
			t.Fatalf("parent effects after dispose: %v", got)
		}
		if got := nested.Effects(); len(got) != 0 {
			t.Fatalf("nested effects after dispose: %v", got)
		}
	})
}

// ---------------------------------------------------------------------------
// 官方 commit.spec：只读不报告；跨组移动报告目标组与来源组
// ---------------------------------------------------------------------------

// TestCommitReportsOnlyWrites 只有结构变更才提交：Resolve / Entries
// / Pending 等只读操作不得产生 EntryChange；跨组移动时 From 指向
// 来源组、Group 指向目标组。
func TestCommitReportsOnlyWrites(t *testing.T) {
	h := newLoaderHarness(t)
	defer h.close()

	h.plugins["leaf"] = &cordis.Plugin{
		Name:  "leaf",
		Apply: func(*cordis.Context, any) error { return nil },
	}
	h.loader.Load([]cordis.EntryOptions{
		{ID: "outer", Name: "group", Group: true, Config: []cordis.EntryOptions{
			{ID: "c", Name: "leaf"},
		}},
		{ID: "inner", Name: "group", Group: true, Config: []cordis.EntryOptions{}},
	})

	before := len(h.changes)

	// 只读：解析、全树遍历、收敛查询都不应提交。
	_, _ = h.loader.Tree().Resolve("outer:c")
	_ = h.loader.Tree().Entries()
	_ = h.loader.Tree().Pending()
	_ = h.loader.Wait()
	if got := len(h.changes); got != before {
		t.Fatalf("reads must not commit: %d -> %d", before, got)
	}

	// 跨组移动：From = 源组，Group = 目标组，且 ID 用短 ID。
	if err := h.loader.Update("outer:c", cordis.EntryOptions{Name: "leaf"}, "inner", -1); err != nil {
		t.Fatal(err)
	}
	if len(h.changes) != before+1 {
		t.Fatalf("move must commit once: %d", len(h.changes))
	}
	change := h.changes[len(h.changes)-1]
	if change.ID != "c" {
		t.Fatalf("change id: %q, want the short id", change.ID)
	}
	if change.From == nil || change.From != h.loader.Tree().Root().Children()[0].Subgroup() {
		t.Fatal("From must be the source group")
	}
	if change.Group == nil || change.Group != h.loader.Tree().Root().Children()[1].Subgroup() {
		t.Fatal("Group must be the target group")
	}
	if change.Options == nil || change.Legacy == nil {
		t.Fatalf("move change payload: options=%v legacy=%v", change.Options, change.Legacy)
	}
}
