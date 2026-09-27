package cordis

import (
	"fmt"
	"testing"
)

// indexTotal 统计倒排索引里登记的总条数（一个 fiber 声明 n 个依赖即计 n 条）。
func indexTotal(r *Reflect) int {
	n := 0
	for _, list := range r.index {
		n += len(list)
	}
	return n
}

// 倒排索引的生命周期不变量（M-3 修复引入的内部状态）：
// track 与 untrack 必须严格配对——实例注销、未注册失败路径与
// Close 级联回收之后，索引都必须回到空，否则服务通知会随
// 运行时长逐步退化，并可能触达已注销的 fiber。
//
// 索引的键是 isolateKey（服务名 + 隔离域），默认域用空 realm 表示，
// 因此裸字面量 "db" 不再是合法键——测试里一律写成 isolateKey{name: "db"}。
func TestReflectIndexLifecycle(t *testing.T) {
	app := New()
	root := app.Root()

	consumer := &Plugin{
		Name:   "consumer",
		Inject: map[string]any{"db": nil},
		Apply:  func(*Context, any) error { return nil },
	}
	var f *Fiber
	app.DoSync(func(ctx *Context) {
		f, _ = ctx.Plugin(consumer, nil)
	})
	app.Wait()

	dbKey := isolateKey{name: "db"}
	if n := len(root.reflect.index[dbKey]); n != 1 {
		t.Fatalf("index should hold the consumer: %d", n)
	}

	// 注销 → 索引回退。
	app.DoSync(func(*Context) { f.Dispose() })
	app.Wait()
	if n := len(root.reflect.index); n != 0 {
		t.Fatalf("index must be empty after dispose: %v", root.reflect.index)
	}

	// 未声明依赖的 fiber 不进入索引。
	plain := &Plugin{Name: "plain", Apply: func(*Context, any) error { return nil }}
	app.DoSync(func(ctx *Context) {
		if _, err := ctx.Plugin(plain, nil); err != nil {
			t.Errorf("plugin: %v", err)
		}
	})
	app.Wait()
	if n := len(root.reflect.index); n != 0 {
		t.Fatalf("plugin without deps must not be indexed: %v", root.reflect.index)
	}

	// Close 级联回收后索引同样清空。
	app.DoSync(func(ctx *Context) {
		if _, err := ctx.Plugin(consumer, nil); err != nil {
			t.Errorf("plugin: %v", err)
		}
	})
	app.Wait()
	if n := len(root.reflect.index[dbKey]); n != 1 {
		t.Fatalf("index should hold the consumer again: %d", n)
	}
	app.Close()
	if n := len(root.reflect.index); n != 0 {
		t.Fatalf("index must be empty after close: %v", root.reflect.index)
	}
}

// 索引必须按「服务名 + 隔离域」分桶，而不是只按服务名：
// 同名服务在三个域中的三个订阅者落在三个互不相干的桶里，
// 且某个域回收后只有它自己的桶消失（空桶必须删除，不能留壳）。
//
// 这条断言直接钉住索引的键——键一旦退回服务名，本测试立刻变红。
func TestReflectIndexSeparatesRealmsAndEmptiesIndependently(t *testing.T) {
	app := New()
	root := app.Root()

	newConsumer := func() *Plugin {
		return &Plugin{
			Name:   "db-consumer",
			Inject: map[string]any{"db": nil},
			Apply:  func(*Context, any) error { return nil },
		}
	}
	defaultKey := isolateKey{name: "db"}
	realm1Key := isolateKey{name: "db", realm: "realm-1"}
	realm2Key := isolateKey{name: "db", realm: "realm-2"}

	var inRealm1, inRealm2 *Fiber
	app.DoSync(func(ctx *Context) {
		if _, err := ctx.Plugin(newConsumer(), nil); err != nil {
			t.Fatalf("plugin: %v", err)
		}
		inRealm1, _ = ctx.Isolate("db", "realm-1").Plugin(newConsumer(), nil)
		inRealm2, _ = ctx.Isolate("db", "realm-2").Plugin(newConsumer(), nil)
	})
	app.Wait()

	for _, tc := range []struct {
		key  isolateKey
		want int
	}{
		{defaultKey, 1},
		{realm1Key, 1},
		{realm2Key, 1},
	} {
		if got := len(root.reflect.index[tc.key]); got != tc.want {
			t.Fatalf("bucket %v: got %d want %d (index=%v)", tc.key, got, tc.want, root.reflect.index)
		}
	}
	if got := len(root.reflect.index); got != 3 {
		t.Fatalf("three realms must mean three buckets: %d", got)
	}
	if got := indexTotal(root.reflect); got != 3 {
		t.Fatalf("every consumer must be registered exactly once: %d", got)
	}

	// 回收 realm-1：只有它的桶消失，且空桶不留壳。
	app.DoSync(func(*Context) { inRealm1.Dispose() })
	app.Wait()
	if got := len(root.reflect.index[realm1Key]); got != 0 {
		t.Fatalf("realm-1 bucket must be reclaimed: %d", got)
	}
	if _, ok := root.reflect.index[realm1Key]; ok {
		t.Fatalf("empty bucket must be deleted, not left behind: %v", root.reflect.index)
	}
	if got := len(root.reflect.index); got != 2 {
		t.Fatalf("only realm-1 should disappear: %v", root.reflect.index)
	}
	if got := len(root.reflect.index[realm2Key]); got != 1 {
		t.Fatalf("realm-2 must be untouched: %v", root.reflect.index)
	}
	if inRealm2.State() != StatePending {
		t.Fatalf("realm-2 consumer must stay parked: %v", inRealm2.State())
	}
	app.Close()
}

// 规模回归（reflect.go 里那句「索引的键必须与 store 同构」的全部依据）：
// 单次通知的候选集大小只由「本次涉及的域」决定，与共存域的总数无关。
//
// 断言刻意落在**候选集长度**而不是耗时上：长度是确定值，不受机器负载
// 影响；耗时断言在共享 CI 上只会变成 flaky。域数从 1 走到 256，期望值
// 恒为 1——这就是「不随域数增长」的可判定形式。
//
// 反面（回归的签名）：索引若按服务名分桶，realms 个域各挂一个订阅者后
// 同名桶里会堆着 realms 个 fiber，单次通知得把它们整份复制一遍再逐个
// 比对域。候选集变成 O(域数)，正是 insula 侧开通退化成 O(N²) 的来源
// （10 000 租户 82 秒、4.5 GB 分配，其中 90% 花在那次复制上）。
//
// 反自证守卫见两个 indexTotal/len(index) 断言：必须确认索引里真的登记了
// realms 条、realms 个桶，否则「单桶只有 1 条」可能只是因为压根没登记成功。
func TestReflectCandidatesDoNotScaleWithUnrelatedRealms(t *testing.T) {
	for _, realms := range []int{1, 4, 256} {
		t.Run(fmt.Sprintf("realms=%d", realms), func(t *testing.T) {
			app := New()
			root := app.Root()

			var activated int
			newConsumer := func() *Plugin {
				return &Plugin{
					Name:   "foo-consumer",
					Inject: map[string]any{"foo": nil},
					Apply:  func(*Context, any) error { activated++; return nil },
				}
			}

			const target = "realm-0"
			fibers := make([]*Fiber, realms)
			app.DoSync(func(ctx *Context) {
				for i := 0; i < realms; i++ {
					f, _ := ctx.Isolate("foo", fmt.Sprintf("realm-%d", i)).Plugin(newConsumer(), nil)
					fibers[i] = f
				}
			})
			app.Wait()

			if got := indexTotal(root.reflect); got != realms {
				t.Fatalf("index must hold every consumer: got %d want %d", got, realms)
			}
			if got := len(root.reflect.index); got != realms {
				t.Fatalf("each realm must own a bucket: got %d want %d", got, realms)
			}

			targetKey := isolateKey{name: "foo", realm: target}
			cands := root.reflect.candidates([]isolateKey{targetKey})
			if len(cands) != 1 {
				t.Fatalf("candidate set must not grow with unrelated realms: got %d want 1", len(cands))
			}
			if cands[0] != fibers[0] {
				t.Fatalf("candidate must be the realm-0 consumer")
			}

			// 默认域没有任何订阅者：域不同，桶就必须不同。
			if got := root.reflect.candidates([]isolateKey{{name: "foo"}}); len(got) != 0 {
				t.Fatalf("default realm must stay empty: %d", len(got))
			}

			// 端到端：只在 realm-0 提供，只有 realm-0 的依赖者被唤醒。
			app.DoSync(func(ctx *Context) {
				if _, err := ctx.Isolate("foo", target).Provide("foo", 100, nil); err != nil {
					t.Errorf("provide: %v", err)
				}
			})
			app.Wait()
			if activated != 1 || fibers[0].State() != StateActive {
				t.Fatalf("only the target realm's consumer should activate: activated=%d state=%v",
					activated, fibers[0].State())
			}
			for i := 1; i < realms; i++ {
				if fibers[i].State() == StateActive {
					t.Fatalf("realm-%d consumer must stay parked", i)
				}
			}
			app.Close()
		})
	}
}

// 通知用的域键必须取自注册记录（impl.key），而不是提供者自身上下文的
// isolateKey 现推值：组件可以在派生上下文里 Isolate 之后再 Provide，
// 此时「注册到哪个域」与「它自己在哪个域」是两个不同的域。
//
// 这里提供者把 foo 注册进 other 域，它自己停在默认域。若通知按
// 自身上下文的键推（f.ctx.isolateKey("foo") = 默认域），other 域的依赖者
// 永远等不到通知——它已经声明了依赖、索引里也在，就是没人叫它。
//
// 同一条路径上的第二个断言：internal/service 事件的载荷也必须查
// **注册记录**（r.store[k]）而不是按提供者的上下文现解析——后者在
// 本场景里解析不到任何东西（提供者不在 other 域），监听器会收到 nil。
func TestReflectNotifyUsesTheRegistrationKeyNotTheProviderContext(t *testing.T) {
	app := New()

	// watcher 无依赖，先单独铺好并结算，保证监听器在通知发生前已挂上。
	type event struct {
		got bool
		val any
	}
	var inOtherEvent, inDefaultEvent event
	newWatcher := func(sink *event) *Plugin {
		return &Plugin{
			Name: "watcher",
			Apply: func(ctx *Context, _ any) error {
				_, err := ctx.On("internal/service", func(_ *Context, args ...any) any {
					// 只记首个事件：Close 级联回收会再发一次
					// (name, nil)，别让它覆盖掉要断言的那次。
					if sink.got {
						return nil
					}
					sink.got = true
					if len(args) >= 2 {
						sink.val = args[1]
					}
					return nil
				})
				return err
			},
		}
	}

	var inOther, inDefault int
	newConsumer := func(sink *int) *Plugin {
		return &Plugin{
			Name:   "foo-consumer",
			Inject: map[string]any{"foo": nil},
			Apply:  func(*Context, any) error { *sink++; return nil },
		}
	}
	provider := &Plugin{
		Name: "foo-provider",
		Apply: func(ctx *Context, _ any) error {
			_, err := ctx.Isolate("foo", "other").Provide("foo", 7, nil)
			return err
		},
	}

	app.DoSync(func(ctx *Context) {
		if _, err := ctx.Isolate("foo", "other").Plugin(newWatcher(&inOtherEvent), nil); err != nil {
			t.Fatalf("plugin: %v", err)
		}
		if _, err := ctx.Plugin(newWatcher(&inDefaultEvent), nil); err != nil {
			t.Fatalf("plugin: %v", err)
		}
		if _, err := ctx.Isolate("foo", "other").Plugin(newConsumer(&inOther), nil); err != nil {
			t.Fatalf("plugin: %v", err)
		}
		if _, err := ctx.Plugin(newConsumer(&inDefault), nil); err != nil {
			t.Fatalf("plugin: %v", err)
		}
	})
	app.Wait()
	if inOtherEvent.got || inOther != 0 {
		t.Fatalf("nothing provided yet: watcher=%v activated=%d", inOtherEvent.got, inOther)
	}

	app.DoSync(func(ctx *Context) {
		if _, err := ctx.Plugin(provider, nil); err != nil {
			t.Fatalf("plugin: %v", err)
		}
	})
	app.Wait()

	// 断言都放在 Close 之前：级联回收会再发一轮服务事件，
	// 把观测窗口关掉比在事后猜哪条是"提供时那条"可靠得多。
	if inOther != 1 {
		t.Fatalf("the realm the service was registered in must be notified: %d", inOther)
	}
	if inDefault != 0 {
		t.Fatalf("the provider's own realm must not be disturbed: %d", inDefault)
	}
	if !inOtherEvent.got {
		t.Fatal("other realm's listener must see the service event")
	}
	if inOtherEvent.val != 7 {
		t.Fatalf("payload must come from the registration record: %v", inOtherEvent.val)
	}
	if inDefaultEvent.got {
		t.Fatalf("default realm's listener must not see another realm's event: %v", inDefaultEvent.val)
	}
	app.Close()
}

// notifyOwn 一次覆盖 fiber 提供的全部服务：一个 fiber 提供两个服务、
// 两个依赖者各依赖其中一个时，两个依赖者都要被唤醒（这也是 candidates
// 多键去重分支的真实入口）。
func TestReflectNotifyOwnCoversEveryProvidedService(t *testing.T) {
	app := New()

	seen := map[string]bool{}
	newConsumer := func(name string) *Plugin {
		return &Plugin{
			Name:   name + "-consumer",
			Inject: map[string]any{name: nil},
			Apply:  func(*Context, any) error { seen[name] = true; return nil },
		}
	}
	provider := &Plugin{
		Name: "provider",
		Apply: func(ctx *Context, _ any) error {
			if _, err := ctx.Provide("a", 1, nil); err != nil {
				return err
			}
			_, err := ctx.Provide("b", 2, nil)
			return err
		},
	}

	app.DoSync(func(ctx *Context) {
		if _, err := ctx.Plugin(newConsumer("a"), nil); err != nil {
			t.Fatalf("plugin: %v", err)
		}
		if _, err := ctx.Plugin(newConsumer("b"), nil); err != nil {
			t.Fatalf("plugin: %v", err)
		}
		if _, err := ctx.Plugin(provider, nil); err != nil {
			t.Fatalf("plugin: %v", err)
		}
	})
	app.Wait()
	app.Close()

	if !seen["a"] || !seen["b"] {
		t.Fatalf("both dependants must be woken: %v", seen)
	}
}

// 一个 fiber 声明两个依赖 → track 逐名登记进两个桶；
// 一次通知覆盖两个键时，同一个 fiber 只能作为候选出现一次，
// 否则 notify 会对它重复 refresh、并把它重复计入受影响集合。
func TestReflectCandidatesDeduplicateAcrossKeys(t *testing.T) {
	app := New()
	root := app.Root()

	var f *Fiber
	app.DoSync(func(ctx *Context) {
		f, _ = ctx.Plugin(&Plugin{
			Name:   "ab-consumer",
			Inject: map[string]any{"a": nil, "b": nil},
			Apply:  func(*Context, any) error { return nil },
		}, nil)
	})
	app.Wait()

	aKey := isolateKey{name: "a"}
	bKey := isolateKey{name: "b"}
	if got := indexTotal(root.reflect); got != 2 {
		t.Fatalf("one fiber with two deps means two entries: %d", got)
	}
	if got := len(root.reflect.index); got != 2 {
		t.Fatalf("two deps mean two buckets: %d", got)
	}

	got := root.reflect.candidates([]isolateKey{aKey, bKey})
	if len(got) != 1 || got[0] != f {
		t.Fatalf("candidates must dedupe a fiber declaring both keys: %v", got)
	}
	if got := root.reflect.candidates([]isolateKey{bKey, aKey}); len(got) != 1 {
		t.Fatalf("dedupe must be order-independent: %v", got)
	}
	app.Close()
}
