package cordis

import "fmt"

// Runtime 同一 Plugin 的全部运行时实例集合。
// 注册表以 Plugin 指针为键（Go 中不可比较的函数无法作键，
// 以 Plugin 定义本身作为组件身份，等价于官方实现的
// 以插件回调函数为身份）。
type Runtime struct {
	plugin *Plugin
	fibers []*Fiber
	// index 把 Fiber 指针映射到其在 fibers 中的位置，使 remove 为 O(1)。
	// Runtime 以 Plugin 为键，单实例的 fibers 承载整个 App 内该插件
	// 的全部实例（insula 下单分片可达数千），线性扫描 + 整尾拼接曾
	// 是注销路径的主成本（ADR-0007 第 2 处退化）。
	index map[*Fiber]int
}

// Plugin 返回该 Runtime 对应的插件定义。
func (rt *Runtime) Plugin() *Plugin { return rt.plugin }

// Name 返回插件名（官方实现 Runtime.name 的同义访问）。
func (rt *Runtime) Name() string {
	if rt.plugin == nil {
		return ""
	}
	return rt.plugin.Name
}

// Fibers 返回当前实例的快照。顺序**不是**注册序：remove 采用与末尾
// 交换的 O(1) 删除（见 remove 注释），读者只应关心成员而非次序。
func (rt *Runtime) Fibers() []*Fiber {
	return append([]*Fiber(nil), rt.fibers...)
}

func (rt *Runtime) add(f *Fiber) func() {
	if rt.index == nil {
		rt.index = make(map[*Fiber]int, len(rt.fibers)+1)
	}
	rt.index[f] = len(rt.fibers)
	rt.fibers = append(rt.fibers, f)
	return func() { rt.remove(f) }
}

func (rt *Runtime) remove(f *Fiber) {
	i, ok := rt.index[f]
	if !ok {
		return
	}
	delete(rt.index, f)
	last := len(rt.fibers) - 1
	if i != last {
		// 与末尾交换而非整尾复制：删除由 O(N) 降为 O(1)。
		// 顺序不再是插入序，但 fibers 的读者（settled 稳定性检查、
		// Registry.Delete 全量注销）都只关心成员而非次序。
		moved := rt.fibers[last]
		rt.fibers[i] = moved
		rt.index[moved] = i
	}
	rt.fibers[last] = nil
	rt.fibers = rt.fibers[:last]
}

// Registry 插件注册表：管理 Plugin → Runtime 映射，
// 提供 Plugin/Inject 两个实例化入口。
type Registry struct {
	ctx      *Context // 根上下文
	counter  int
	runtimes map[*Plugin]*Runtime
	order    []*Runtime // 注册序，保证通知的确定性
}

func newRegistry(ctx *Context) *Registry {
	return &Registry{ctx: ctx, runtimes: make(map[*Plugin]*Runtime)}
}

func (r *Registry) nextUID() int {
	r.counter++
	return r.counter
}

// Size 返回已注册的 Runtime 数量。
func (r *Registry) Size() int { return len(r.runtimes) }

// Has 判断插件是否已注册。
func (r *Registry) Has(p *Plugin) bool {
	_, ok := r.runtimes[p]
	return ok
}

// Get 返回插件的 Runtime。
func (r *Registry) Get(p *Plugin) *Runtime { return r.runtimes[p] }

// Runtimes 按注册序返回全部 Runtime。
func (r *Registry) Runtimes() []*Runtime {
	return append([]*Runtime(nil), r.order...)
}

// Values 等价于 Runtimes()，与官方实现 `registry.values()` 同名。
func (r *Registry) Values() []*Runtime { return r.Runtimes() }

// Keys 按注册序返回全部已注册插件（官方 `registry.keys()` 的同义访问）。
func (r *Registry) Keys() []*Plugin {
	out := make([]*Plugin, 0, len(r.order))
	for _, rt := range r.order {
		out = append(out, rt.plugin)
	}
	return out
}

// ForEach 按注册序遍历全部插件及其 Runtime（官方 `registry.forEach`）。
// 回调内不得增删注册表——遍历的是快照，修改会在下一轮才可见。
func (r *Registry) ForEach(fn func(p *Plugin, rt *Runtime)) {
	for _, rt := range r.Runtimes() {
		fn(rt.plugin, rt)
	}
}

func resolveConfig(p *Plugin, config any) (any, error) {
	if p.Validate == nil {
		return config, nil
	}
	return p.Validate(config)
}

// Plugin 在 ctx 下实例化插件：
//
//  1. 创建 Fiber（PENDING），合并 inject 拦截配置到其上下文；
//  2. 以父 fiber 的效果注册——注册动作（入列、解析配置、
//     首次依赖评估）立即执行，注销动作（冻结 epoch、等待
//     效果完全回收）在父 fiber 卸载或手动 Dispose 时执行；
//  3. 依赖满足时由状态机自动驱动 LOADING → ACTIVE。
//
// 返回的 Fiber 可用于 Update 热重载与 Dispose 注销。
func (r *Registry) Plugin(ctx *Context, p *Plugin, config any) (*Fiber, error) {
	return r.PluginInject(ctx, p, config, nil)
}

// PluginInject 同 Plugin，但以 inject 覆盖插件声明的依赖表。
// loader 层据此实现入口级依赖声明（EntryOptions.Inject）。
// inject 为 nil 时沿用 Plugin.Inject。
//
// 返回值契约：
//   - (nil, err)：结构性失败（插件无效或父上下文已失活），
//     Fiber 从未创建/注册，调用方无需善后；
//   - (f, err)：配置校验失败——Fiber 已注册并处于 FAILED 状态
//     （错误同时经日志记录），可经 f.Update 修复或 f.Dispose 注销；
//   - (f, nil)：成功。
func (r *Registry) PluginInject(ctx *Context, p *Plugin, config any, inject map[string]any) (*Fiber, error) {
	if p == nil || p.Apply == nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidPlugin, "<nil>")
	}
	if err := ctx.fiber.assertActive(); err != nil {
		return nil, err
	}
	rt := r.runtimes[p]
	if rt == nil {
		rt = &Runtime{plugin: p}
		r.runtimes[p] = rt
		r.order = append(r.order, rt)
	}
	if inject == nil {
		inject = make(map[string]any, len(p.Inject))
		for k, v := range p.Inject {
			inject[k] = v
		}
	}
	var cfgErr error
	f := newFiber(ctx, config, inject, rt)
	d, err := ctx.fiber.effectStep("ctx.plugin()", func() (disposeStep, error) {
		remove := rt.add(f)
		cfg, err := resolveConfig(p, config)
		if err != nil {
			r.ctx.app.logger.Error("plugin %s config error: %v", p.Name, err)
			cfgErr = err
			f.err = err
			f.setState(StateFailed)
		} else {
			f.config = cfg
			f.refresh()
		}
		return disposeStep{
			run: func() {
				f.disposed = true
				f.uid = 0
				f.localUpdates = nil     // 局部更新钩子随实例终结
				r.ctx.reflect.untrack(f) // 依赖倒排索引随实例注销
				f.ctx.Emit("internal/plugin", f)
				if _, ok := r.runtimes[p]; ok {
					remove()
					if len(rt.fibers) == 0 {
						r.deleteRuntime(p)
					}
				}
				f.setEpoch(inactiveEpoch)
				if f.store == nil && f.pending == nil {
					f.setState(StateDisposed)
				}
			},
			wait: func(then func()) { f.whenStable(then) },
		}, nil
	})
	if err != nil {
		// 效果未注册（父上下文失活）：fiber 从未挂载，不可用。
		// 依赖倒排索引需一并回退，保证 track/untrack 配对。
		r.ctx.reflect.untrack(f)
		return nil, err
	}
	f.dispose = d
	return f, cfgErr
}

// InjectList 把服务名列表转换为必选依赖声明（对应官方 inject 的
// **数组形式** `inject: ['a', 'b']`）：值与官方一致，均为 nil，
// 即"必选依赖、无附加拦截配置"。
//
// 官方的 Inject.resolve 还会沿原型链合并类继承来的 inject 表；
// Go 没有原型链，多形态依赖用 map 直接表达即可（Plugin.Inject /
// EntryOptions.Inject 都是 map[string]any）。
func InjectList(names ...string) map[string]any {
	out := make(map[string]any, len(names))
	for _, name := range names {
		out[name] = nil
	}
	return out
}

// Inject 声明动态依赖：deps 满足时执行 apply 并追踪其全部效果，
// 任一依赖失满足时效果被逆序回收。等价于实例化一个匿名插件。
func (r *Registry) Inject(ctx *Context, deps map[string]any, apply func(ctx *Context) error) (*Fiber, error) {
	p := &Plugin{
		Name:   "inject",
		Inject: deps,
		Apply:  func(c *Context, _ any) error { return apply(c) },
	}
	return r.Plugin(ctx, p, nil)
}

// Delete 注销插件的全部实例。
func (r *Registry) Delete(p *Plugin) {
	rt := r.runtimes[p]
	if rt == nil {
		return
	}
	delete(r.runtimes, p)
	for i, cur := range r.order {
		if cur == rt {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	for _, f := range append([]*Fiber(nil), rt.fibers...) {
		f.Dispose()
	}
}

func (r *Registry) deleteRuntime(p *Plugin) {
	delete(r.runtimes, p)
	for i, cur := range r.order {
		if cur != nil && cur.plugin == p {
			r.order = append(r.order[:i], r.order[i+1:]...)
			return
		}
	}
}
