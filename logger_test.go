package cordis_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	cordis "github.com/corecraft-io/cordis"
)

// logRecorder 线程安全的日志采集器（日志可能产自调度 goroutine）。
type logRecorder struct {
	mu   sync.Mutex
	msgs []cordis.LogMessage
}

func (r *logRecorder) record(m cordis.LogMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, m)
}

func (r *logRecorder) all() []cordis.LogMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]cordis.LogMessage(nil), r.msgs...)
}

func (r *logRecorder) plain() []string {
	out := []string{}
	for _, m := range r.all() {
		out = append(out, m.Text)
	}
	return out
}

func (r *logRecorder) named() []string {
	out := []string{}
	for _, m := range r.all() {
		out = append(out, fmt.Sprintf("%s:%s", m.Name, m.Level))
	}
	return out
}

// TestLoggerNameResolution 名字解析顺序：显式参数 > 拦截配置 > 插件名。
func TestLoggerNameResolution(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	rec := &logRecorder{}
	h.app.Logger().Capture(rec.record)

	var inPlugin []string
	p := &cordis.Plugin{
		Name: "database",
		Apply: func(ctx *cordis.Context, _ any) error {
			inPlugin = append(inPlugin,
				ctx.Logger().Name(), // 插件名
				ctx.Intercept("logger", cordis.LoggerOptions{Name: "db-custom"}).Logger().Name(),
				ctx.Logger("explicit").Name(), // 显式参数压过拦截配置
			)
			ctx.Logger().Info("hello")
			return nil
		},
	}
	h.run(func(ctx *cordis.Context) {
		if _, err := ctx.Plugin(p, nil); err != nil {
			t.Fatal(err)
		}
	})
	want := []string{"database", "db-custom", "explicit"}
	if fmt.Sprint(inPlugin) != fmt.Sprint(want) {
		t.Fatalf("logger names: %v, want %v", inPlugin, want)
	}
	if got := rec.all(); len(got) != 1 || got[0].Name != "database" || got[0].FiberName != "database" {
		t.Fatalf("message routing: %+v", got)
	}
	// 根上下文上的服务级日志器名为 root。
	h.app.Logger().Info("from root")
	if got := rec.all(); got[len(got)-1].Name != "root" {
		t.Fatalf("root logger name: %q", got[len(got)-1].Name)
	}
}

// TestLoggerLevelFiltering 阈值判定：出口的 Levels 按名优先、其次默认项、
// 最后回落到日志器自身（拦截配置）的级别；级别越高越"吵"，
// 只有 level ≤ 阈值 的消息才输出。
func TestLoggerLevelFiltering(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	def := &logRecorder{}
	verbose := &logRecorder{}
	h.app.Logger().Capture(def.record)
	if d := h.app.Logger().Exporter(&cordis.Exporter{
		Levels: map[string]cordis.LogLevel{"": cordis.LevelDebug}, // 默认项：全部级别
		Export: verbose.record,
	}); d == nil {
		t.Fatal("exporter registration returned nil")
	}

	quiet := &cordis.Plugin{
		Name: "quiet",
		Apply: func(ctx *cordis.Context, _ any) error {
			ctx.Logger().Debug("d") // 低于默认阈值（info）
			ctx.Logger().Info("i")
			ctx.Logger().Warn("w")
			ctx.Logger().Error("e")
			return nil
		},
	}
	h.run(func(ctx *cordis.Context) {
		if _, err := ctx.Plugin(quiet, nil); err != nil {
			t.Fatal(err)
		}
	})
	if got := def.plain(); fmt.Sprint(got) != fmt.Sprint([]string{"i", "w", "e"}) {
		t.Fatalf("default exporter (threshold=info): %v", got)
	}
	if got := verbose.plain(); fmt.Sprint(got) != fmt.Sprint([]string{"d", "i", "w", "e"}) {
		t.Fatalf("verbose exporter (threshold=debug): %v", got)
	}

	// 拦截配置把该日志器的阈值提到 debug：出口的阈值优先级为
	// `Levels[名字] → Levels[""] → 日志器自身级别`，因此
	// 无 levels 的出口跟随日志器（follow），显式写了默认项的出口只认
	// 自己（strict），按名字覆盖的出口最优先（byName）。
	follow, strict, byName := &logRecorder{}, &logRecorder{}, &logRecorder{}
	h.app.Logger().Exporter(&cordis.Exporter{Export: follow.record})
	h.app.Logger().Exporter(&cordis.Exporter{
		Levels: map[string]cordis.LogLevel{"": cordis.LevelInfo},
		Export: strict.record,
	})
	h.app.Logger().Exporter(&cordis.Exporter{
		Levels: map[string]cordis.LogLevel{"loud": cordis.LevelError},
		Export: byName.record,
	})
	loud := &cordis.Plugin{
		Name: "loud",
		Apply: func(ctx *cordis.Context, _ any) error {
			ctx.Logger().Info("i")
			ctx.Logger().Debug("d")
			ctx.Logger().Error("e")
			return nil
		},
	}
	h.run(func(ctx *cordis.Context) {
		scoped := ctx.Intercept("logger", map[string]any{"level": cordis.LevelDebug}).
			Intercept("logger", map[string]any{"name": "loud"})
		if _, err := scoped.Plugin(loud, nil); err != nil {
			t.Fatal(err)
		}
	})
	if got := follow.plain(); fmt.Sprint(got) != fmt.Sprint([]string{"i", "d", "e"}) {
		t.Fatalf("exporter without levels must follow the logger level: %v", got)
	}
	if got := strict.plain(); fmt.Sprint(got) != fmt.Sprint([]string{"i", "e"}) {
		t.Fatalf("exporter with an explicit default must keep its own threshold: %v", got)
	}
	if got := byName.plain(); fmt.Sprint(got) != fmt.Sprint([]string{"e"}) {
		t.Fatalf("per-name override: %v", got)
	}
}

// TestLoggerExporterDispose 出口注销幂等，且不影响默认出口。
func TestLoggerExporterDispose(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	rec := &logRecorder{}
	def := &logRecorder{}
	h.app.Logger().Capture(def.record)
	dispose := h.app.Logger().Exporter(&cordis.Exporter{Export: rec.record})

	h.app.Logger().Info("before")
	dispose()
	dispose() // 幂等
	h.app.Logger().Info("after")

	if got := rec.plain(); fmt.Sprint(got) != fmt.Sprint([]string{"before"}) {
		t.Fatalf("extra exporter: %v", got)
	}
	if got := def.plain(); fmt.Sprint(got) != fmt.Sprint([]string{"before", "after"}) {
		t.Fatalf("default exporter must survive: %v", got)
	}
}

// TestLoggerBufferIsBoundedAndChronological 环形缓冲有界且保持时间序
// （对应官方 logger 测试的同名断言）。
func TestLoggerBufferIsBoundedAndChronological(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	h.app.Logger().Capture(func(cordis.LogMessage) {}) // 静音默认出口
	h.app.Logger().SetBufferSize(4)
	if got := h.app.Logger().BufferSize(); got != 4 {
		t.Fatalf("buffer size: %d", got)
	}

	for i := 0; i < 10; i++ {
		h.app.Logger().Info("msg-%d", i)
	}
	msgs := h.app.Logger().Messages()
	if len(msgs) != 4 {
		t.Fatalf("buffer length: %d, want 4", len(msgs))
	}
	want := []string{"msg-6", "msg-7", "msg-8", "msg-9"}
	got := []string{}
	for _, m := range msgs {
		got = append(got, m.Text)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("buffer content: %v, want %v", got, want)
	}
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Seq <= msgs[i-1].Seq {
			t.Fatalf("sequence not increasing: %v", msgs)
		}
		if msgs[i].Time.Before(msgs[i-1].Time) {
			t.Fatalf("timestamps not monotonic: %v", msgs)
		}
	}

	// 缩容时只保留最新的若干条。
	h.app.Logger().SetBufferSize(2)
	if got := h.app.Logger().Messages(); len(got) != 2 || got[0].Text != "msg-8" {
		t.Fatalf("shrink buffer: %v", got)
	}
	// 0 表示不缓存。
	h.app.Logger().SetBufferSize(0)
	h.app.Logger().Info("dropped")
	if got := h.app.Logger().Messages(); len(got) != 0 {
		t.Fatalf("buffer must be empty: %v", got)
	}
}

// TestLoggerSilence 静音默认出口后不再有 stderr 输出，但缓冲照旧记录。
func TestLoggerSilence(t *testing.T) {
	h := newHarness(t)
	defer h.app.Close()

	h.app.Logger().Silence()
	h.app.Logger().Error("quiet please")
	msgs := h.app.Logger().Messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "quiet please") {
		t.Fatalf("buffer must still record: %v", msgs)
	}
	if msgs[0].Level != cordis.LevelError || msgs[0].Level.String() != "error" {
		t.Fatalf("level: %v", msgs[0].Level)
	}
}
