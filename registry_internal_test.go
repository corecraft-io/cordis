package cordis

import "testing"

// rtConsistent 断言 fibers 切片与 index 索引相互自洽：
// 长度相等、每个切片的下标与 index 记录一致、index 的键都还在切片里。
// 这是 swap-with-last + 位置索引删除的不变式——任一处漏更新都会立刻失衡。
func rtConsistent(t *testing.T, rt *Runtime) {
	t.Helper()
	if len(rt.fibers) != len(rt.index) {
		t.Fatalf("fibers(%d) 与 index(%d) 长度不一致", len(rt.fibers), len(rt.index))
	}
	for i, f := range rt.fibers {
		if f == nil {
			t.Fatalf("fibers[%d] 悬空（nil），删除后未正确收缩", i)
		}
		if got, ok := rt.index[f]; !ok || got != i {
			t.Fatalf("index[%p]=%d 但 fibers 中位于 %d（不一致）", f, got, i)
		}
	}
	for f := range rt.index {
		found := false
		for _, cur := range rt.fibers {
			if cur == f {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("index 含切片里不存在的 fiber %p", f)
		}
	}
}

// TestRuntimeRemoveConsistency 覆盖删除的三种位置（首/中/尾）与乱序，
// 验证删除后 index 与 fibers 始终自洽、且无悬空/泄漏。
func TestRuntimeRemoveConsistency(t *testing.T) {
	rt := &Runtime{plugin: &Plugin{}}
	fs := make([]*Fiber, 5)
	removers := make([]func(), 5)
	for i := range fs {
		fs[i] = &Fiber{}
		removers[i] = rt.add(fs[i])
	}
	rtConsistent(t, rt)
	if len(rt.fibers) != 5 {
		t.Fatalf("add 后应有 5 个 fiber，实际 %d", len(rt.fibers))
	}

	// 删中间（触发 swap-with-last：把末尾元素搬进空洞并修正其 index）。
	removers[2]()
	rtConsistent(t, rt)
	if len(rt.fibers) != 4 {
		t.Fatalf("删中间后应为 4，实际 %d", len(rt.fibers))
	}

	// 重复删除同一 fiber 必须是 no-op，不再改动状态。
	removers[2]()
	rtConsistent(t, rt)
	if len(rt.fibers) != 4 {
		t.Fatalf("重复删除不应改变长度，实际 %d", len(rt.fibers))
	}

	// 删首、删尾，再删光。
	removers[0]()
	removers[4]()
	rtConsistent(t, rt)
	removers[1]()
	removers[3]()
	rtConsistent(t, rt)

	if len(rt.fibers) != 0 || len(rt.index) != 0 {
		t.Fatalf("删光后应为空，fibers=%d index=%d", len(rt.fibers), len(rt.index))
	}
}

// TestRuntimeRemoveThroughDispose 走公开注销路径：实例逐个经 f.Dispose()
// 注销后，Runtime 的 fibers / index 必须正确收缩、始终自洽，最终 Close
// 把 Runtime 整体清空。这是 ADR-0007 第 2 处退化的反向保护——删除逻辑
// 一旦错配（漏更新 moved 的 index、漏删键），这里就会失衡或残留死键。
func TestRuntimeRemoveThroughDispose(t *testing.T) {
	app := New()
	root := app.Root()
	p := &Plugin{
		Name:   "nop",
		Inject: map[string]any{},
		Apply:  func(*Context, any) error { return nil },
	}
	var fs []*Fiber
	app.DoSync(func(ctx *Context) {
		for i := 0; i < 50; i++ {
			f, err := ctx.Plugin(p, nil)
			if err != nil {
				t.Fatalf("Plugin: %v", err)
			}
			fs = append(fs, f)
		}
	})
	rt := root.registry.Get(p)
	if rt == nil {
		t.Fatal("plugin 未注册出 Runtime")
	}
	rtConsistent(t, rt)
	if len(rt.fibers) != 50 {
		t.Fatalf("应有 50 个实例，实际 %d", len(rt.fibers))
	}

	// 乱序注销一半，每步都校验自洽。（Dispose 是 Fiber API，
	// 必须在调度器上下文中调用——测试也不得绕过这条约束。）
	app.DoSync(func(ctx *Context) {
		for _, i := range []int{0, 49, 25, 12, 37, 7, 43, 19} {
			fs[i].Dispose()
		}
	})
	app.Wait()
	rtConsistent(t, rt)
	if len(rt.fibers) != 42 {
		t.Fatalf("注销 8 个后应剩 42，实际 %d", len(rt.fibers))
	}

	// 注销剩余。
	app.DoSync(func(ctx *Context) {
		for _, f := range fs {
			f.Dispose()
		}
	})
	app.Wait()
	rtConsistent(t, rt)
	if len(rt.fibers) != 0 || len(rt.index) != 0 {
		t.Fatalf("全注销后应为空，fibers=%d index=%d", len(rt.fibers), len(rt.index))
	}

	// 再建一批后立即 Close，Runtime 应被整体清空。
	app.DoSync(func(ctx *Context) {
		for i := 0; i < 10; i++ {
			if _, err := ctx.Plugin(p, nil); err != nil {
				t.Fatalf("Plugin: %v", err)
			}
		}
	})
	app.Close()
	if rt2 := root.registry.Get(p); rt2 != nil {
		t.Fatalf("Close 后 Runtime 应已删除，fibers=%d index=%d", len(rt2.fibers), len(rt2.index))
	}
}
