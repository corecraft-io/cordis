package cordis

import (
	"fmt"
	"strings"
)

// Logger 见 logger.go：日志服务由根上下文持有（`App.Logger()`），
// 组件内用 `ctx.Logger()` 取带插件名的日志器。

// Listener 事件监听器。回调收到的 ctx 是分发方上下文（Emit/Serial/…
// 的调用者）；waterfall 模式还会在参数末尾追加 next。
type Listener func(ctx *Context, args ...any) any

// ListenOptions 监听器注册选项。
type ListenOptions struct {
	// Prepend 把监听器插入注册序头部（默认追加到尾部）。内置分发器
	// （internal/update 的局部钩子装配器）依赖它取得优先位置。
	Prepend bool
	// Global 让监听器绕过 isolate 域过滤——对全部域的同类事件可见。
	// 内置的 internal/service 事件按域过滤，只有 Global 监听器能观察
	// 到其它域的服务上下线；internal/* 系列事件本身不带过滤。
	Global bool
}

func (o ListenOptions) isZero() bool { return !o.Prepend && !o.Global }

// hook 事件监听器，随注册它的 Fiber 生命周期自动回收。
type hook struct {
	ctx      *Context
	callback Listener
	global   bool
}

// Events 事件总线。监听器通过 Fiber.Effect 注册，
// Fiber 卸载时自动注销。
//
// 单线程模型下所有分发模式均为串行执行；Parallel 与官方实现的
// 差别仅在于回调不并发，错误聚合语义保持一致。
type Events struct {
	ctx   *Context // 根上下文
	hooks map[string][]*hook
}

func newEvents(ctx *Context) *Events {
	e := &Events{ctx: ctx, hooks: make(map[string][]*hook)}
	// 内置的两枚钩子构成 fiber 局部更新钩子的装配器，注册序有意义，
	// 不可调换（见 routeInternalUpdate / runLocalUpdateHooks）：
	//   1) internal/listener —— 注册期扩展点，把非 global 的
	//      internal/update 监听器改挂到注册方 fiber 的局部链上；
	//   2) internal/update（global + prepend）—— 分发时先跑该 fiber
	//      的局部链，再落回真正的 next。
	if _, err := e.On(ctx, "internal/listener", e.routeInternalUpdate); err != nil {
		panic(err) // 根上下文必然可用，失败即编码错误
	}
	if _, err := e.On(ctx, "internal/update", e.runLocalUpdateHooks,
		ListenOptions{Prepend: true, Global: true}); err != nil {
		panic(err)
	}
	return e
}

// On 注册监听器（默认追加到注册序末尾），返回可手动注销
// （可重复调用）的 Dispose；Fiber 卸载时监听器自动注销。
//
// 注册前先分发 internal/listener：任一监听器返回非 nil 的 Dispose
// 即接管本次注册（返回值原样转交调用方），内置装配器正是借此把
// 非 global 的 internal/update 监听器改挂到 fiber 局部链上。
//
// 失活校验由 Effect 内部统一执行（assertActive），此处不重复。
func (e *Events) On(ctx *Context, name string, listener Listener, opts ...ListenOptions) (Dispose, error) {
	var o ListenOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	if err := ctx.fiber.assertActive(); err != nil {
		return nil, err
	}
	if result := e.Bail(ctx, "internal/listener", name, listener, o); result != nil {
		if d, ok := result.(Dispose); ok && d != nil {
			return d, nil
		}
	}
	return ctx.fiber.Effect("ctx.on("+name+")", func() (Dispose, error) {
		h := &hook{ctx: ctx, callback: listener, global: o.Global}
		e.insert(name, h, o.Prepend)
		return func() { e.unregister(name, h) }, nil
	})
}

// Once 注册一次性监听器，首次触发后自动注销。
func (e *Events) Once(ctx *Context, name string, listener Listener, opts ...ListenOptions) (Dispose, error) {
	var outer Dispose
	inner, err := e.On(ctx, name, func(c *Context, args ...any) any {
		if outer != nil {
			outer()
		}
		return listener(c, args...)
	}, opts...)
	if err != nil {
		return nil, err
	}
	outer = inner
	return inner, nil
}

// insert 按选项把监听器放入桶：Prepend 走头部，默认走尾部。
func (e *Events) insert(name string, h *hook, prepend bool) {
	if !prepend {
		e.hooks[name] = append(e.hooks[name], h)
		return
	}
	e.hooks[name] = append([]*hook{h}, e.hooks[name]...)
}

func (e *Events) unregister(name string, h *hook) {
	hooks := e.hooks[name]
	for i, cur := range hooks {
		if cur == h {
			e.hooks[name] = append(hooks[:i], hooks[i+1:]...)
			if len(e.hooks[name]) == 0 {
				delete(e.hooks, name)
			}
			return
		}
	}
}

// hooksOf 取按注册序排列的监听器**快照**。filter 非 nil 时，
// 仅保留 global 或通过过滤的监听器（用于 isolate 域感知分发）。
//
// 一律返回副本：分发期间监听器可以注册或注销自己（注销会原地压缩
// 切片），在活切片上直接遍历会漏掉被顶替的相邻元素——waterfall 还要
// 在这份快照上逐级 shift，复用活切片更不可能正确。
func (e *Events) hooksOf(name string, filter func(hookCtx *Context) bool) []*hook {
	hooks := e.hooks[name]
	if filter == nil {
		return append([]*hook(nil), hooks...)
	}
	var out []*hook
	for _, h := range hooks {
		if h.global || filter(h.ctx) {
			out = append(out, h)
		}
	}
	return out
}

// observe 观测一次分发：mode / 事件名 / 载荷 / 分发方上下文。
//
// 两个刻意的设计（与官方实现的 _resolve 同款）：一是**无人监听时
// 直接返回**（否则每次分发都要多一次开销）；二是 internal/* 事件
// 自身不再触发它，避免观测者把自己卷进无限递归。
func (e *Events) observe(ctx *Context, mode, name string, args []any) {
	if len(e.hooks["internal/dispatch"]) == 0 {
		return
	}
	if strings.HasPrefix(name, "internal/") {
		return
	}
	for _, h := range e.hooksOf("internal/dispatch", nil) {
		e.invoke(ctx, "internal/dispatch", h, []any{mode, name, args, ctx})
	}
}

// Emit 同步分发事件，回调返回值与错误均被忽略（错误记日志）。
func (e *Events) Emit(ctx *Context, name string, args ...any) {
	e.observe(ctx, "emit", name, args)
	for _, h := range e.hooksOf(name, ctx.eventFilter) {
		e.invoke(ctx, name, h, args)
	}
}

// EmitFiltered 以显式过滤规则分发事件。
func (e *Events) EmitFiltered(ctx *Context, name string, filter func(hookCtx *Context) bool, args ...any) {
	for _, h := range e.hooksOf(name, filter) {
		e.invoke(ctx, name, h, args)
	}
}

func (e *Events) invoke(ctx *Context, name string, h *hook, args []any) {
	defer func() {
		if r := recover(); r != nil {
			e.ctx.app.logger.Error("event %q listener panic: %v", name, r)
		}
	}()
	_ = h.callback(ctx, args...)
}

// Serial 串行分发；首个返回非 nil 结果的回调终止分发并返回该结果。
func (e *Events) Serial(ctx *Context, name string, args ...any) any {
	e.observe(ctx, "serial", name, args)
	for _, h := range e.hooksOf(name, ctx.eventFilter) {
		if result, err := call(h, ctx, args); err != nil {
			e.ctx.app.logger.Error("event %q listener error: %v", name, err)
		} else if result != nil {
			return result
		}
	}
	return nil
}

// Bail 同 Serial，但同步执行且不吞 panic。
func (e *Events) Bail(ctx *Context, name string, args ...any) any {
	e.observe(ctx, "bail", name, args)
	for _, h := range e.hooksOf(name, ctx.eventFilter) {
		if result := h.callback(ctx, args...); result != nil {
			return result
		}
	}
	return nil
}

// Parallel 分发并聚合全部错误（单线程下等价于串行）。
func (e *Events) Parallel(ctx *Context, name string, args ...any) error {
	e.observe(ctx, "parallel", name, args)
	var errs []error
	for _, h := range e.hooksOf(name, ctx.eventFilter) {
		if _, err := call(h, ctx, args); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 1 {
		return errs[0]
	}
	if len(errs) > 1 {
		msgs := ""
		for i, err := range errs {
			if i > 0 {
				msgs += "; "
			}
			msgs += err.Error()
		}
		return fmt.Errorf("%d errors: %s", len(errs), msgs)
	}
	return nil
}

func call(h *hook, ctx *Context, args []any) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("listener panic: %v", r)
		}
	}()
	return h.callback(ctx, args...), nil
}

// Waterfall 洋葱式分发：每个监听器收到 (args..., next)，调用 next 把
// 控制权交给链上的下一个监听器，最后一个 next 落到 terminal（nil
// 表示返回 nil）。监听器不调用 next 即终止分发，其返回值原样传出——
// internal/update 的「否决本次重载」正是靠这一点表达。
//
// next 只在**当前监听器帧**内可用且只能用一次：重复调用，或把 next
// 保存到外层帧之后再调用，都会 panic ErrDuplicateNext。这是监听器的
// 编程错误（next 代表"把这次调用继续下去"），必须立刻暴露而不是
// 静默吞掉。调度器内的调用点（Fiber.Update）会把该 panic 收敛为错误
// 返回，不会击穿调度器 goroutine。
func (e *Events) Waterfall(ctx *Context, name string, terminal func() any, args ...any) any {
	e.observe(ctx, "waterfall", name, args)
	hooks := e.hooksOf(name, ctx.eventFilter)
	steps := make([]chainStep, len(hooks))
	for i, h := range hooks {
		h := h // 显式固化循环变量：闭包捕获不依赖语言版本的循环变量语义
		steps[i] = func(call []any) any { return h.callback(ctx, call...) }
	}
	return chain(steps, terminal, args)
}

// chainStep 一次链式调用的执行单元：call 以 (args..., next) 调用。
type chainStep func(call []any) any

// chain 把 steps 串成洋葱链：next 落到下一个 step，走完则落到 terminal。
func chain(steps []chainStep, terminal func() any, args []any) any {
	idx := 0
	var dispatch func() any
	dispatch = func() any {
		if idx >= len(steps) {
			if terminal == nil {
				return nil
			}
			return terminal()
		}
		step := steps[idx]
		idx++
		called := false
		call := append(append(make([]any, 0, len(args)+1), args...), func() any {
			if called {
				panic(ErrDuplicateNext)
			}
			called = true
			return dispatch()
		})
		return step(call)
	}
	return dispatch()
}

// routeInternalUpdate 内置 internal/listener 钩子：把**非 global** 的
// internal/update 监听器改挂到注册方 fiber 的局部钩子链上。于是
// 「谁注册的钩子」与「谁的更新该被它拦住」自动一一对应——同一插件
// 的多个实例互不打扰，这正是官方实现的 internal/listener 路由。
// 返回非 nil 的 Dispose 即接管本次注册（监听器不再进全局桶）。
func (e *Events) routeInternalUpdate(ctx *Context, args ...any) any {
	if len(args) < 3 {
		return nil
	}
	name, _ := args[0].(string)
	opts, _ := args[2].(ListenOptions)
	if name != "internal/update" || opts.Global {
		return nil
	}
	listener, ok := args[1].(Listener)
	if !ok || listener == nil {
		return nil
	}
	return ctx.fiber.addLocalUpdate(listener)
}

// runLocalUpdateHooks 内置 internal/update 钩子（global + prepend）：
// 把注册在**本 fiber** 上的局部钩子按注册序串成链，链尾接真正的
// next（即"替换配置并重启"）。任一局部钩子不调用 next 即否决本次
// 更新——等价于官方实现的 fiber._hooks 局部链。
func (e *Events) runLocalUpdateHooks(ctx *Context, args ...any) any {
	f := ctx.Fiber()
	if f == nil || len(args) == 0 {
		return nil
	}
	next, _ := args[len(args)-1].(func() any)
	payload := args[:len(args)-1]
	hooks := append([]*localUpdateHook(nil), f.localUpdates...)
	steps := make([]chainStep, len(hooks))
	for i, h := range hooks {
		h := h
		steps[i] = func(call []any) any { return h.fn(ctx, call...) }
	}
	return chain(steps, next, payload)
}
