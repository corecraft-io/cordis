package cordis

import (
	"fmt"
	"sort"
)

// impl 一个服务实现：由某个 fiber 在某个隔离域中注册。
// key 记录注册时的隔离域键——域校验的依据：
// 依赖解析（checkImpl）与直接访问（Context.Get）都以
// 调用方上下文的域键比对，同名服务跨域互不可见。
type impl struct {
	name  string
	key   isolateKey
	fiber *Fiber
	value any
	check func() bool
}

// Reflect 协效应存储（对应官方实现的 ReflectService）：
// 全局唯一的 isolate 域键 → 服务实现映射，配合依赖声明
// （inject）与依赖满足检查（checkImpl）实现空间维的可组合性。
//
// 关键语义：所有键解析都以「调用方上下文」为基准——
// provider 注册时用它自己的域键写入，dependant 检查满足度
// 时用它自己的域键读取。同名服务在不同域中互不可见。
//
// index 是依赖声明的倒排索引（**隔离域键 → 声明依赖该域中该服务的
// Fiber 列表**，按创建序）：服务上下线只需通知真正声明了该服务的依赖者，
// 而非全量扫描 Runtime × Fiber。
//
// # 索引的键必须与 store 同构（这是本层性能的全部依据）
//
// 「谁需要被通知」与「解析会命中谁」是同一个问题的两种问法，因此必须
// 用同一把钥匙。索引若只按服务名分桶、域另在通知时过滤（本层早先的
// 写法），一次通知就要先复制整张全名桶再逐个比对域——代价变成
// O(全部域的订阅者)，一个域里的事件要为所有域付钱，且随域数增长：
// 批量开通退化成 O(N²)（insula 侧实测 10 000 租户 82 秒、4.5 GB 分配，
// 其中 90% 花在那个复制上）。
//
// 代价的另一面：域键必须能从 Fiber 重算出来，untrack 才找得回自己的桶。
// 这条不变量的成立条件写在 untrack 的注释里，不要随手破坏。
type Reflect struct {
	ctx   *Context // 根上下文
	store map[isolateKey]*impl
	index map[isolateKey][]*Fiber
}

func newReflect(ctx *Context) *Reflect {
	return &Reflect{
		ctx:   ctx,
		store: make(map[isolateKey]*impl),
		index: make(map[isolateKey][]*Fiber),
	}
}

// track 按 f 的依赖声明登记倒排索引（Fiber 创建时调用一次）。
//
// 每个依赖名各算一次域键：它就是「该 fiber 会去哪个域解析这个服务」，
// 与 checkImpl 用的表达式完全相同。两者必须同源——索引登记在哪个桶，
// 通知就只会去那个桶，而解析却按 f.ctx 的域键取值。
func (r *Reflect) track(f *Fiber) {
	for name := range f.inject {
		k := f.ctx.isolateKey(name)
		r.index[k] = append(r.index[k], f)
	}
}

// untrack 注销倒排索引（Fiber 注销时调用，与其创建一一对应）。
//
// 这里**重算**域键而不是把键存在 Fiber 上，靠的是一条不变量：
//
//   - Context 构造后不可变（isolates 只读，见 context.go 的 extend）；
//   - Fiber.ctx 只在 newFiber 里赋值一次，此后不再改（视图替换走的是
//     新 Fiber，不是就地改旧 Fiber）；
//   - Fiber.inject 同样只读。
//
// 三者合起来使 f.ctx.isolateKey(name) 成为构造期算定的纯函数值，
// 重算必得同一个键。**若哪天让 ctx 或 inject 变成可变**（例如 Update
// 就地改依赖表、或允许重绑上下文），这条不变量立刻断掉，表现为
// 索引里出现永远清不掉的陈旧条目——届时必须改成「track 时把键记在
// Fiber 上」。索引一致性另有内部测试守着（见 index_internal_test.go）。
//
// 桶空即删：域键随租户注销而消失，留下空桶是"从不清理"的形态，
// 正是无界增长的典型样子。
func (r *Reflect) untrack(f *Fiber) {
	for name := range f.inject {
		k := f.ctx.isolateKey(name)
		list := r.index[k]
		for i, cur := range list {
			if cur != f {
				continue
			}
			list = append(list[:i], list[i+1:]...)
			break
		}
		if len(list) == 0 {
			delete(r.index, k)
			continue
		}
		r.index[k] = list
	}
}

// candidates 返回在 keys 这些隔离域键上声明依赖的 Fiber（按创建序去重）。
//
// 返回的是**副本**：通知过程中被通知者可能新建 Fiber（track 进同一个桶）
// 或注销（untrack 会原地压缩切片），边遍历边改会让遍历读到错位元素。
// 因为键已经是域级的，复制的规模是「本次涉及域的订阅者数」。
func (r *Reflect) candidates(keys []isolateKey) []*Fiber {
	if len(keys) == 1 {
		return append([]*Fiber(nil), r.index[keys[0]]...)
	}
	var out []*Fiber
	seen := make(map[*Fiber]bool)
	for _, k := range keys {
		for _, f := range r.index[k] {
			if seen[f] {
				continue
			}
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// getImpl 以 ctx 的隔离域解析 name。strict 为 true 时
// 仅接受处于 ACTIVE 状态的实现——这保证了「提供者正在
// 卸载」时依赖者立即视为未满足。
func (r *Reflect) getImpl(ctx *Context, name string, strict bool) *impl {
	im, ok := r.store[ctx.isolateKey(name)]
	if !ok {
		return nil
	}
	if strict && im.fiber.state != StateActive {
		return nil
	}
	return im
}

// Get 以调用方上下文解析服务值（严格模式）。
func (r *Reflect) Get(ctx *Context, name string) (any, bool) {
	im := r.getImpl(ctx, name, true)
	if im == nil {
		return nil, false
	}
	return im.value, true
}

// Set 更新当前 fiber 注册的服务值。
func (r *Reflect) Set(ctx *Context, name string, value any) error {
	im, ok := r.store[ctx.isolateKey(name)]
	if !ok {
		return fmt.Errorf("cannot set service %q without provide", name)
	}
	if im.fiber != ctx.fiber {
		return fmt.Errorf("cannot set service %q registered by another fiber", name)
	}
	im.value = value
	return nil
}

// Provide 注册服务：以调用方上下文的隔离域键写入全局存储，
// 并登记到调用方 fiber 的服务表中。返回的 Dispose 撤销注册。
//
// 撤销顺序保证（论文 §3.2 的 dependant-first 原则）：
// 先从全局存储删除并通知所有依赖者，等待它们完全下线后，
// 才清理提供者自身的服务表——依赖者绝不会访问到已销毁的服务。
func (r *Reflect) Provide(ctx *Context, name string, value any, check func() bool) (Dispose, error) {
	f := ctx.fiber
	return f.effectStep(fmt.Sprintf("ctx.provide(%q)", name), func() (disposeStep, error) {
		key := ctx.isolateKey(name)
		if old, dup := r.store[key]; dup {
			return disposeStep{}, fmt.Errorf("%w: %q at <%s>", ErrServiceDuplicate, name, old.fiber.name())
		}
		if f.store == nil {
			return disposeStep{}, fmt.Errorf("cannot provide %q on unloaded fiber", name)
		}
		im := &impl{name: name, key: key, fiber: f, value: value, check: check}
		r.store[key] = im
		f.store[name] = im
		if f.state == StateActive {
			// 加载过程中 provide 由随后的 ACTIVE 状态发布统一通知。
			// 传 key 而不是 name：key 是注册时真正使用的域键，
			// 也是索引的桶键（见 notify 注释）。
			r.notify(ctx, []isolateKey{key})
		}
		var affected []*Fiber
		return disposeStep{
			run: func() {
				delete(r.store, key)
				affected = r.notify(ctx, []isolateKey{key})
			},
			wait: func(then func()) {
				cleanup := func() { delete(f.store, name) }
				if len(affected) == 0 {
					cleanup()
					then()
					return
				}
				n := len(affected)
				for _, dep := range affected {
					dep.whenStable(func() {
						n--
						if n == 0 {
							cleanup()
							then()
						}
					})
				}
			},
		}, nil
	})
}

// notify 通知所有在 keys 这些隔离域键上声明依赖的 fiber 重新解析依赖
// 并刷新目标视图。返回受影响的 fiber（供撤销流程等待它们下线）。
//
// keys 是**提供者注册时使用的域键**（等价于 impl.key），不是从某个
// 上下文现推出来的值。在 Provide 路径上两者是同一个表达式，无所谓；
// 在 notifyOwn 路径上则有实质区别：组件若在派生上下文里 Isolate 之后
// 再 Provide，注册键与它自身上下文的域键并不相同——按自身上下文推
// 会通知到错误的域（该通知的域没被通知，不该通知的域被打扰）。
// impl.key 是唯一权威来源。
//
// 域匹配在这里是**结构性**的：候选集直接取自 provider 的桶，不再逐个
// 比对域键。早先的写法要先遍历全部域的订阅者再过滤，代价随域数增长。
//
// 曾经的 filter 参数已删除：它的三个调用点全部传 nil（等价于上面这条
// 默认规则），保留它等于给"自定义过滤"留一个口子，而任何自定义过滤
// 都只能靠全量扫描实现——正是本函数刚摆脱的东西。将来若真需要不同的
// 路由规则，做法是**另建一张索引**，而不是在这里加一个 filter。
func (r *Reflect) notify(provider *Context, keys []isolateKey) []*Fiber {
	var fibers []*Fiber
	for _, f := range r.candidates(keys) {
		hasUpdate := false
		for _, k := range keys {
			if _, ok := f.inject[k.name]; !ok {
				// 索引正确时这里恒为真。留着是让索引一旦被破坏时
				// 退化成"少通知"，而不是"解析了不该解析的服务"。
				continue
			}
			hasUpdate = true
			f.checkImpl(k.name)
		}
		if !hasUpdate {
			continue
		}
		f.refresh()
		fibers = append(fibers, f)
	}
	// internal/service：按域过滤分发服务变更事件。
	// 事件这条通路没有倒排索引（监听器以 Context 注册、不带域键），
	// 因此过滤器只能逐个比对——代价是 O(注册的监听器数)，与依赖通知
	// 不是同一条路径，也不受本次改动影响。
	for _, k := range keys {
		var value any
		if im, ok := r.store[k]; ok {
			value = im.value
		}
		nameFilter := func(hookCtx *Context) bool { return hookCtx.isolateKey(k.name) == k }
		r.ctx.events.EmitFiltered(provider, "internal/service", nameFilter, k.name, value)
	}
	return fibers
}

// notifyOwn 通知 fiber 提供的全部服务（跨越 ACTIVE 边界时调用）。
//
// 域键取自 f.store 里的 impl.key，而不是 f.ctx.isolateKey(name)：
// 见 notify 的注释（组件可以在派生上下文里注册）。
func (r *Reflect) notifyOwn(f *Fiber) {
	if f.store == nil {
		return
	}
	var keys []isolateKey
	for _, im := range f.store {
		if im.fiber == f {
			keys = append(keys, im.key)
		}
	}
	if len(keys) == 0 {
		return
	}
	// map 遍历无序，排序只为让通知顺序确定——顺序会影响被通知者的
	// 加载顺序，进而影响别人观测到的启动序列，测试与排障都需要它可复现。
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].name != keys[j].name {
			return keys[i].name < keys[j].name
		}
		return keys[i].realm < keys[j].realm
	})
	r.notify(f.ctx, keys)
}
