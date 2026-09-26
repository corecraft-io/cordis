package cordis

import (
	"fmt"
	"testing"
)

// TestDisposableCompactionPreservesOrder 确认墓碑压缩确实触发，
// 且重建后仍保持插入序（clear 按 LIFO 逆序返回）。
//
// 触发条件：order > 8 且 order > 2×存活数。压入 16 条后删除 9 条，
// order 仍为 16、存活降为 7，越过阈值 ⇒ 压缩把 order 收缩到 7。
// 重建依赖「id 单调递增 ⇒ 排序即插入序」这一不变量，故必须实测。
func TestDisposableCompactionPreservesOrder(t *testing.T) {
	l := newDisposableList()
	var disposed []int
	removes := make([]func(), 0, 16)
	for i := 0; i < 16; i++ {
		i := i
		removes = append(removes, l.push(disposeStep{run: func() { disposed = append(disposed, i) }}))
	}
	for i := 0; i < 9; i++ {
		removes[i]()
	}
	if len(l.order) != 7 {
		t.Fatalf("compaction should shrink order to live count: order=%d steps=%d", len(l.order), l.len())
	}
	for _, s := range l.clear() {
		s.run()
	}
	want := []int{15, 14, 13, 12, 11, 10, 9}
	if fmt.Sprint(disposed) != fmt.Sprint(want) {
		t.Fatalf("LIFO order broken after compaction: got %v, want %v", disposed, want)
	}
}

// BenchmarkDisposableSteadyChurn 度量一次「更替」的摊还代价：
// push 一条新效果 + delete 同槽位旧效果，含摊入的墓碑重建成本。
//
// 存活数 live 恒定，order 中的墓碑持续累积，每约 live 次操作触发
// 一次重建（O(live·log live)）。若重建被正确摊还，单次更替的成本
// 应随 live 增长缓慢（≈log live）而非线性恶化——下方 live 从 4 增至
// 256（64×）时 ns/op 仅小幅上升即为此结论的实测依据。
func BenchmarkDisposableSteadyChurn(b *testing.B) {
	for _, live := range []int{4, 32, 256} {
		b.Run(fmt.Sprintf("live-%d", live), func(b *testing.B) {
			l := newDisposableList()
			removes := make([]func(), live)
			for i := range removes {
				removes[i] = l.push(disposeStep{run: func() {}})
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				slot := i % live
				removes[slot]()
				removes[slot] = l.push(disposeStep{run: func() {}})
			}
		})
	}
}

// BenchmarkDisposableCompaction 隔离度量单次重建的代价。
//
// 计时区外预置 order=2×live、存活=live 的列表——此时 order > 2×存活数
// 恰好不成立，压缩尚未触发；计时区内仅删除一条即越过阈值，触发一次
// order 重建（map 遍历 + 排序）。计时区因此只含单次 O(1) 的 delete 与
// 单次重建，ns/op 直接反映重建本身的开销。
func BenchmarkDisposableCompaction(b *testing.B) {
	for _, live := range []int{32, 512} {
		b.Run(fmt.Sprintf("live-%d", live), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				l := newDisposableList()
				removes := make([]func(), 0, 2*live)
				for j := 0; j < 2*live; j++ {
					removes = append(removes, l.push(disposeStep{run: func() {}}))
				}
				// 删除 live 条：order 保持 2×live，存活降至 live，
				// 阈值 order > 2×存活数 恰未成立（相等）。
				for j := 0; j < live; j++ {
					removes[j]()
				}
				b.StartTimer()
				removes[live]() // 再删一条 ⇒ 触发一次重建
				// 自校验：若阈值条件被改动导致重建未触发，
				// 本基准将立刻失败而非静默退化为纯 delete 测量。
				if len(l.order) != live-1 {
					b.Fatalf("compaction did not fire: order=%d, want %d", len(l.order), live-1)
				}
			}
		})
	}
}

// BenchmarkDisposableClear 度量 clear 的代价与「历史操作量」的关系。
//
// 存活数固定为 64，churn 倍数递增：每轮更替为 push+delete 各一次，
// 存活数不变而历史 push 次数按倍数增长。压缩使 order ≤ 2×存活数，
// 故 clear 的 ns/op 应在各倍数下基本持平；若压缩失效，order 将随
// 历史 push 次数线性增长，clear 成本随之线性上升（64× 时约 32 倍）。
func BenchmarkDisposableClear(b *testing.B) {
	const live = 64
	for _, churn := range []int{1, 4, 16, 64} {
		b.Run(fmt.Sprintf("churn-%dx", churn), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				l := newDisposableList()
				removes := make([]func(), live)
				for j := range removes {
					removes[j] = l.push(disposeStep{run: func() {}})
				}
				for j := 0; j < churn*live; j++ {
					slot := j % live
					removes[slot]()
					removes[slot] = l.push(disposeStep{run: func() {}})
				}
				b.StartTimer()
				if n := len(l.clear()); n != live {
					b.Fatalf("clear returned %d steps, want %d", n, live)
				}
			}
		})
	}
}
