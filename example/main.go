// 示例：时空可组合组件模型（论文《Spatiotemporal Composability》Go 实现）
//
// 场景：一个由声明式配置驱动的「数据库 + 缓存 + Web 服务」组合：
//
//   - 时间维：组件配置热重载，旧实例的副作用（连接、监听端口）按 LIFO 逆序回收；
//   - 空间维：依赖以协效应声明，提供者下线时依赖者自动降级，
//     隔离域让多套服务栈并存互不干扰（如多租户）。
package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	cordis "github.com/corecraft-io/cordis"
)

func main() {
	app := cordis.New()
	defer app.Close()

	// ---------------------------------------------------------------------------
	// 组件定义：每个副作用都返回 Dispose（显式逆操作），
	// 对外能力以服务（Provide）形式发布。
	// ---------------------------------------------------------------------------

	database := &cordis.Plugin{
		Name: "database",
		Apply: func(ctx *cordis.Context, config any) error {
			dsn := config.(string)
			fmt.Printf("[database] connect %s\n", dsn)
			_, err := ctx.Provide("database", dsn, nil)
			if err != nil {
				return err
			}
			_, err = ctx.Effect("conn", func() (cordis.Dispose, error) {
				return func() { fmt.Printf("[database] close %s\n", dsn) }, nil
			})
			return err
		},
	}

	cache := &cordis.Plugin{
		Name:   "cache",
		Inject: map[string]any{"database": nil}, // 协效应：依赖声明
		Apply: func(ctx *cordis.Context, _ any) error {
			dsn, _ := ctx.Get("database")
			fmt.Printf("[cache] warm up on %s\n", dsn)
			if _, err := ctx.Provide("cache", "cache:"+fmt.Sprint(dsn), nil); err != nil {
				return err
			}
			_, err := ctx.Effect("entries", func() (cordis.Dispose, error) {
				return func() { fmt.Println("[cache] flush entries") }, nil
			})
			return err
		},
	}

	web := &cordis.Plugin{
		Name:   "web",
		Inject: map[string]any{"database": nil, "cache": nil},
		Apply: func(ctx *cordis.Context, config any) error {
			port := config.(int)
			dsn, _ := ctx.Get("database")
			fmt.Printf("[web] listen :%d (on %s)\n", port, dsn)
			_, err := ctx.Effect("listener", func() (cordis.Dispose, error) {
				return func() { fmt.Printf("[web] shutdown :%d\n", port) }, nil
			})
			return err
		},
	}

	// ticker 演示「外部 goroutine + 显式逆操作」：周期由配置驱动，
	// 热重载时旧 goroutine 停表之后新的才启动（LIFO 逆序回收），
	// 卸载后其 goroutine 必然退出——逆操作要真的等到它退出。
	// 定时器里的每次回调都经 App.Do 回到调度器，绝不直接碰运行时状态。
	ticker := &cordis.Plugin{
		Name: "ticker",
		Apply: func(ctx *cordis.Context, config any) error {
			every := config.(time.Duration)
			ctx.Logger().Info("start ticking every %s", every)
			stop := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				t := time.NewTicker(every)
				defer t.Stop()
				for {
					select {
					case <-t.C:
						ctx.App().Do(func(c *cordis.Context) {
							c.Logger("tick").Info("tick")
						})
					case <-stop:
						return
					}
				}
			}()
			_, err := ctx.Effect("ticker", func() (cordis.Dispose, error) {
				return func() {
					ctx.Logger().Info("stop ticking")
					close(stop)
					wg.Wait()
				}, nil
			})
			return err
		},
	}

	plugins := map[string]*cordis.Plugin{
		"database": database,
		"cache":    cache,
		"web":      web,
		"ticker":   ticker,
	}

	loader := cordis.NewLoader(app, func(name string) (*cordis.Plugin, error) {
		if p, ok := plugins[name]; ok {
			return p, nil
		}
		return nil, fmt.Errorf("unknown plugin %q", name)
	})

	// ---------------------------------------------------------------------------
	// 声明式配置：组件实例由 Entry 描述，而非过程式代码。
	// ---------------------------------------------------------------------------

	config := []cordis.EntryOptions{
		{ID: "db", Name: "database", Config: "postgres://prod"},
		{ID: "cache", Name: "cache"},
		{ID: "web", Name: "web", Config: 8080},
	}

	fmt.Println("== 初始加载 ==")
	loader.Load(config)
	printStates(loader, []string{"db", "cache", "web"})

	// ---------------------------------------------------------------------------
	// 时间维演示：配置热重载（论文 §5 的 HMR 机制）
	// web 配置变更 → 卸载旧实例（逆序回收监听器）→ 加载新实例。
	// ---------------------------------------------------------------------------

	fmt.Println("\n== 热重载：web 8080 → 9090 ==")
	config[2].Config = 9090
	loader.Load(config)
	printStates(loader, []string{"db", "cache", "web"})

	// ---------------------------------------------------------------------------
	// 空间维演示：依赖者降级与恢复（reactive coeffects）
	// 数据库下线 → cache/web 自动回收效果回到 PENDING；
	// 数据库回归 → 依赖者自动恢复。
	// ---------------------------------------------------------------------------

	fmt.Println("\n== 提供者下线：db 禁用 ==")
	config[0].Disabled = true
	loader.Load(config)
	printStates(loader, []string{"db", "cache", "web"})

	fmt.Println("\n== 提供者回归：db 启用 ==")
	config[0].Disabled = false
	loader.Load(config)
	printStates(loader, []string{"db", "cache", "web"})

	// ---------------------------------------------------------------------------
	// 空间维演示：隔离域（isolation domain）
	// 两套完整的「数据库+缓存+Web」栈以共享域标签隔离：
	// tenant-a 栈与 tenant-b 栈互不可见，同名服务多实例并存。
	// ---------------------------------------------------------------------------

	fmt.Println("\n== 隔离域：多租户双栈 ==")
	realm := func(label string) map[string]any {
		return map[string]any{"database": label, "cache": label}
	}
	loader.Load([]cordis.EntryOptions{
		{ID: "db-a", Name: "database", Config: "postgres://tenant-a", Isolate: realm("tenant-a")},
		{ID: "db-b", Name: "database", Config: "postgres://tenant-b", Isolate: realm("tenant-b")},
		{ID: "cache-a", Name: "cache", Isolate: realm("tenant-a")},
		{ID: "cache-b", Name: "cache", Isolate: realm("tenant-b")},
		{ID: "web-a", Name: "web", Config: 9001, Isolate: realm("tenant-a")},
		{ID: "web-b", Name: "web", Config: 9002, Isolate: realm("tenant-b")},
	})
	printStates(loader, []string{"db-a", "db-b", "cache-a", "cache-b", "web-a", "web-b"})

	// ---------------------------------------------------------------------------
	// 声明式操作：运行期动态调整入口树（协调与级联回收）。
	// ---------------------------------------------------------------------------

	fmt.Println("\n== 动态操作：移除 tenant-b 的数据库（其栈整体降级）==")
	loader.Remove("db-b")
	printStates(loader, []string{"db-a", "cache-a", "web-a", "cache-b", "web-b"})

	// ---------------------------------------------------------------------------
	// 全树快照：Entries() 覆盖全部入口（含分组子入口），顺序按短 ID 稳定；
	// Wait() 等待协调收敛（依赖未满足的 pending 也是稳定态，不会挂起）。
	// ---------------------------------------------------------------------------

	fmt.Println("\n== 全树快照 ==")
	for _, e := range loader.Tree().Entries() {
		fmt.Printf("   %-8s %s\n", e.Options().ID, e.Options().Name)
	}
	fmt.Printf("   已收敛=%v 未稳定=%d\n", loader.Wait(), len(loader.Tree().Pending()))

	// ---------------------------------------------------------------------------
	// 日志：命名日志器 + 出口 + 有界缓冲。
	// 名字取显式参数 > logger 拦截配置 > 插件名；缓冲保留最近 N 条。
	// ---------------------------------------------------------------------------

	fmt.Println("\n== 日志：命名日志器与缓冲 ==")
	app.Logger().SetBufferSize(64)
	app.DoSync(func(ctx *cordis.Context) {
		ctx.Logger("demo").Info("来自显式命名的日志器")
		ctx.Logger().Warn("来自插件上下文的日志器（本例为 root）")
		ctx.Logger().Debug("低于默认阈值（info），缓冲不记录")
	})
	for _, m := range app.Logger().Messages() {
		fmt.Printf("   #%d [%s] %s: %s\n", m.Seq, m.Level, m.Name, m.Text)
	}

	// ---------------------------------------------------------------------------
	// 洋葱式分发（waterfall）：监听器拿到 (args..., next)，
	// 不调用 next 即终止整条链——本例用它把配置改写后再交给下一个环节。
	// ---------------------------------------------------------------------------

	fmt.Println("\n== 洋葱式分发：graphql 端口改写链 ==")
	app.DoSync(func(ctx *cordis.Context) {
		_, _ = ctx.On("demo/port", func(_ *cordis.Context, args ...any) any {
			next := args[len(args)-1].(func() any)
			return next().(int) + 1000 // 最外层：整体 +1000
		}, cordis.ListenOptions{Prepend: true})
		_, _ = ctx.On("demo/port", func(_ *cordis.Context, args ...any) any {
			next := args[len(args)-1].(func() any)
			return next().(int) * 2 // 内层：先翻倍
		})
		port := ctx.Waterfall("demo/port", func() any { return 21 }, "ignored").(int)
		fmt.Printf("   (21 * 2) + 1000 = %d\n", port)
	})

	// ---------------------------------------------------------------------------
	// 时间维的"外部定时器"：效果携带显式逆操作（停表 + 关闭计数），
	// 外部 goroutine 只经 App.Do 进入调度器，绝不直接碰运行时状态。
	// ---------------------------------------------------------------------------

	fmt.Println("\n== 定时器效果：外部 goroutine 经 App.Do 进入 ==")
	if _, err := loader.Create(cordis.EntryOptions{ID: "tick", Name: "ticker", Config: 60 * time.Millisecond}, "", -1); err != nil {
		fmt.Println("   tick create:", err)
	}
	time.Sleep(180 * time.Millisecond)
	// 热重载周期：旧 goroutine 停表之后，新 goroutine 才启动（LIFO 逆序回收）。
	var updateErr error
	app.DoSync(func(*cordis.Context) {
		e, err := loader.Tree().Resolve("tick")
		if err != nil {
			updateErr = err
			return
		}
		updateErr = e.Fiber().Update(30 * time.Millisecond)
	})
	if updateErr != nil {
		fmt.Println("   tick update:", updateErr)
	}
	time.Sleep(120 * time.Millisecond)
	if err := loader.Remove("tick"); err != nil {
		fmt.Println("   tick remove:", err)
	}
	fmt.Println("   定时器已注销：其 goroutine 随之退出")

	// ---------------------------------------------------------------------------
	// 配置插值：入口配置里的 ${env:NAME} 在交给插件前展开
	//（分组配置即子入口列表，不参与插值）。
	// ---------------------------------------------------------------------------

	fmt.Println("\n== 配置插值：${env:NAME} ==")
	os.Setenv("CORDIS_EXAMPLE_HOST", "10.0.0.7")
	if _, err := loader.Create(cordis.EntryOptions{
		ID:   "envdb",
		Name: "database",
		// map / slice 里的字符串同样会展开；这里用字符串以便直接观察。
		Config: "postgres://${env:CORDIS_EXAMPLE_HOST}:5432/app",
	}, "", -1); err != nil {
		fmt.Println("   envdb create:", err)
	}
	// 入口配置保持模板原文，交给插件的是展开后的值（见上面 [database] connect 一行）。
	if e, err := loader.Tree().Resolve("envdb"); err == nil {
		fmt.Printf("   入口配置（原文）：%v\n", e.Options().Config)
		fmt.Printf("   组件实际收到：%v\n", e.Fiber().Config())
		fmt.Printf("   Evaluate(\"${env:CORDIS_EXAMPLE_HOST}\") = %s\n", e.Evaluate("${env:CORDIS_EXAMPLE_HOST}"))
	}
	_ = loader.Remove("envdb")

	// ---------------------------------------------------------------------------
	// 效果自省 + 自定义日志出口：
	// Fiber.Effects() 列出当前仍生效的效果标签（注册序）；
	// App.Logger().Exporter 追加一个出口，与默认 stderr 出口并存。
	// ---------------------------------------------------------------------------

	fmt.Println("\n== 效果自省与自定义日志出口 ==")
	var collected []string
	stopLog := app.Logger().Exporter(&cordis.Exporter{
		Levels: map[string]cordis.LogLevel{"": cordis.LevelWarn}, // 只收 warn 及以上
		Export: func(m cordis.LogMessage) { collected = append(collected, m.Text) },
	})
	app.DoSync(func(ctx *cordis.Context) {
		if e, err := loader.Tree().Resolve("db-a"); err == nil && e.Fiber() != nil {
			fmt.Printf("   %s 持有的效果：%v\n", e.ID(), e.Fiber().Effects())
			ctx.Logger().Warn("重载 %s 的连接池", e.ID())
			e.Fiber().Update("postgres://tenant-a?pool=8")
		}
	})
	fmt.Printf("   自定义出口收到 %d 条（warn 及以上）\n", len(collected))
	stopLog() // 注销出口（幂等）
	// 卸载后自省为空。
	app.DoSync(func(ctx *cordis.Context) {
		if e, err := loader.Tree().Resolve("db-a"); err == nil && e.Fiber() != nil {
			e.Fiber().Dispose()
		}
	})
	app.Wait()

	// ---------------------------------------------------------------------------
	// 关闭：沿效果链级联回收——全部组件按依赖逆序完全还原环境。
	// ---------------------------------------------------------------------------
	fmt.Println("\n== 关闭应用 ==")
}

func printStates(loader *cordis.Loader, ids []string) {
	var parts []string
	for _, id := range ids {
		e, err := loader.Tree().Resolve(id)
		if err != nil {
			parts = append(parts, fmt.Sprintf("%s=removed", id))
			continue
		}
		if e.Fiber() == nil {
			parts = append(parts, fmt.Sprintf("%s=off", id))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%s", id, e.Fiber().State()))
	}
	fmt.Printf("   %s\n", strings.Join(parts, "  "))
}
