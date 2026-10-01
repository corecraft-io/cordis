package cordis

import (
	"reflect"
	"testing"
)

// hookSnapshot 取「事件名 → 监听器数」快照（只记非空桶）。
// 对应官方测试的 getHookSnapshot：用于证明监听器随 Fiber 回收后
// 不留残桶、不漏计数。
func hookSnapshot(app *App) map[string]int {
	out := map[string]int{}
	for name, hooks := range app.root.events.hooks {
		if len(hooks) > 0 {
			out[name] = len(hooks)
		}
	}
	return out
}

// assertNoScheduleErr 断言调度器上下文中收集到的错误（t.Fatal 不能在
// 调度 goroutine 里调用——Goexit 会直接终结调度器）。
func assertNoScheduleErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestEventBucketLifecycle 空桶必须删除：监听器全部注销后桶不应留存，
// 否则长跑进程里每个用过的事件名都变成一条永久记录（无界增长的形态）。
// 内置的两枚装配器钩子（internal/listener、internal/update）除外。
func TestEventBucketLifecycle(t *testing.T) {
	app := New()
	defer app.Close()

	baseline := hookSnapshot(app)
	if len(baseline) != 2 {
		t.Fatalf("built-in hook buckets: %v, want exactly internal/listener + internal/update", baseline)
	}

	var (
		first, second Dispose
		err           error
	)
	app.DoSync(func(ctx *Context) {
		first, err = ctx.On("custom", func(*Context, ...any) any { return nil })
		if err != nil {
			return
		}
		second, err = ctx.On("custom", func(*Context, ...any) any { return nil })
	})
	assertNoScheduleErr(t, err)
	if got := hookSnapshot(app)["custom"]; got != 2 {
		t.Fatalf("custom bucket: %d, want 2", got)
	}

	app.DoSync(func(*Context) { first() })
	if got := hookSnapshot(app)["custom"]; got != 1 {
		t.Fatalf("custom bucket after one dispose: %d, want 1", got)
	}

	app.DoSync(func(*Context) { second() })
	// 断言的是**原始桶**：空桶必须被删除，而不是留一个 len == 0 的键
	// （后者在长跑进程里就是每个用过的事件名一条永久记录）。
	if _, present := app.root.events.hooks["custom"]; present {
		t.Fatal("empty bucket must be deleted, not merely emptied")
	}
	if !reflect.DeepEqual(hookSnapshot(app), baseline) {
		t.Fatalf("buckets after disposal: %v, want %v", hookSnapshot(app), baseline)
	}
}

// TestEventListenerNoLeakAcrossPluginTeardown 监听器必须随注册它的 Fiber
// 一起回收：插件的创建/注销不改变全局桶的构成（官方测试的 compare
// snapshot 断言）。
func TestEventListenerNoLeakAcrossPluginTeardown(t *testing.T) {
	app := New()
	defer app.Close()

	var (
		fibers []*Fiber
		err    error
	)
	app.DoSync(func(ctx *Context) {
		p := &Plugin{
			Name: "talker",
			Apply: func(ctx *Context, _ any) error {
				if _, err := ctx.On("custom/one", func(*Context, ...any) any { return nil }); err != nil {
					return err
				}
				_, err := ctx.On("internal/status", func(*Context, ...any) any { return nil })
				return err
			},
		}
		for i := 0; i < 3; i++ {
			f, pluginErr := ctx.Plugin(p, nil)
			if pluginErr != nil {
				err = pluginErr
				return
			}
			fibers = append(fibers, f)
		}
	})
	assertNoScheduleErr(t, err)
	app.Wait()

	baseline := hookSnapshot(app)
	if baseline["custom/one"] != 3 || baseline["internal/status"] != 3 {
		t.Fatalf("listeners not registered per instance: %v", baseline)
	}

	app.DoSync(func(*Context) {
		for _, f := range fibers {
			f.Dispose()
		}
	})
	app.Wait()

	got := hookSnapshot(app)
	if got["custom/one"] != 0 || got["internal/status"] != 0 {
		t.Fatalf("listeners leaked after teardown: %v", got)
	}
	if len(got) != 2 {
		t.Fatalf("unexpected buckets after teardown: %v", got)
	}
}
