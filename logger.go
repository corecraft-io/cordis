package cordis

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// 日志子系统（对应官方实现的 packages/core/src/logger.ts）。
//
// 三层结构：
//
//	LoggerService  根上下文上的服务：持有出口（Exporter）表与环形缓冲；
//	Logger         命名日志器：由 LoggerService 派生，绑定一个名字与级别下限；
//	Exporter       出口：决定"这条消息要不要输出"以及"输出到哪"。
//
// 与官方实现的差异：官方每个 Exporter 的默认级别来自
// `exporter.levels[name] ?? exporter.levels.default ?? logger.level ?? INFO`，
// 本实现逐条照搬；官方 Message 上挂的是 `WeakRef<Fiber>`，Go 没有弱引用，
// 这里只记名字与实例号，避免日志缓冲把已注销的 Fiber 拖住。

// LogLevel 日志级别。数值越大越"吵"：等级不大于阈值的消息才会输出
// （与官方实现的 LoggerLevel 取值一致）。
type LogLevel int

const (
	// LevelError 错误。
	LevelError LogLevel = iota
	// LevelWarn 警告。
	LevelWarn
	// LevelInfo 信息（默认阈值）。
	LevelInfo
	// LevelDebug 调试。
	LevelDebug
)

func (l LogLevel) String() string {
	switch l {
	case LevelError:
		return "error"
	case LevelWarn:
		return "warn"
	case LevelInfo:
		return "info"
	case LevelDebug:
		return "debug"
	}
	return fmt.Sprintf("level(%d)", int(l))
}

// DefaultBufferSize 环形缓冲的默认容量（与官方一致：1000 条）。
const DefaultBufferSize = 1000

// LogMessage 一条日志。
type LogMessage struct {
	Seq       int
	Time      time.Time
	Name      string // 日志器名（插件名 / 显式名）
	Level     LogLevel
	Text      string // 已按格式串渲染的正文
	FiberName string // 产出该消息的组件实例（根为 "root"）
	UID       int    // 实例号；未绑定实例为 0
}

// Exporter 日志出口。Levels 按日志器名给出该出口的级别阈值
// （键 "" 为默认项）；某名字未命中而默认项也缺失时，回落到日志器
// 自身的级别（拦截配置指定，未指定则 LevelInfo）。
// Export 为 nil 的出口被忽略。
type Exporter struct {
	Levels map[string]LogLevel
	Export func(LogMessage)
}

// threshold 按官方规则解析本出口对 name 的阈值。
func (e *Exporter) threshold(name string, base LogLevel) LogLevel {
	if e.Levels != nil {
		if lv, ok := e.Levels[name]; ok {
			return lv
		}
		if lv, ok := e.Levels[""]; ok {
			return lv
		}
	}
	return base
}

// LoggerOptions 日志拦截配置（ctx.Intercept("logger", …)）：
// Name 覆盖日志器名，Level 覆盖级别阈值。
//
// Level 的零值即 LevelError，无法用它表达"覆盖为 LevelError"——
// 需要按日志器名压制到只输出错误时，用 Exporter.Levels
// （如 map[string]LogLevel{"db": LevelError}）或 map 形式的拦截配置
// map[string]any{"name": "db", "level": LevelError}（键存在即生效）。
type LoggerOptions struct {
	Name  string
	Level LogLevel
}

// Logger 命名日志器：绑定名字、级别阈值与产出实例。
type Logger struct {
	service *LoggerService
	name    string
	level   LogLevel
	fiber   string
	uid     int
}

// Name 返回日志器名。
func (l *Logger) Name() string { return l.name }

// Level 返回解析后的级别阈值（拦截配置指定的值，缺省 LevelInfo）。
func (l *Logger) Level() LogLevel { return l.level }

// Service 返回其所属的日志服务（用于注册出口、读缓冲）。
func (l *Logger) Service() *LoggerService { return l.service }

// Exporter 等价于 l.Service().Exporter(e)：组件内可写
// `ctx.Logger().Exporter(…)`，与官方实现的 `ctx.logger.exporter(…)` 同形。
func (l *Logger) Exporter(e *Exporter) Dispose {
	if l.service == nil {
		return func() {}
	}
	return l.service.Exporter(e)
}

// Error / Warn / Info / Debug 输出一条日志。参数为格式化串与参数
// （语义同 fmt.Sprintf）；方法名不带 f 后缀是为了与官方 API 同名，
// 也便于把消息原样透传给出口。
func (l *Logger) Error(format string, args ...any) { l.log(LevelError, format, args...) }
func (l *Logger) Warn(format string, args ...any)  { l.log(LevelWarn, format, args...) }
func (l *Logger) Info(format string, args ...any)  { l.log(LevelInfo, format, args...) }
func (l *Logger) Debug(format string, args ...any) { l.log(LevelDebug, format, args...) }

func (l *Logger) log(level LogLevel, format string, args ...any) {
	if l.service == nil {
		return
	}
	l.service.emit(LogMessage{
		Name:      l.name,
		Level:     level,
		Text:      fmt.Sprintf(format, args...),
		FiberName: l.fiber,
		UID:       l.uid,
	}, l.level)
}

// LoggerService 日志服务，挂载于根上下文。
type LoggerService struct {
	mu         sync.Mutex
	exporters  map[int]*Exporter
	seq        int
	seqMessage int
	buffer     []LogMessage
	bufferSize int
}

// defaultExporterID 固定为默认出口（stderr）的槽位，
// 使 Silence / Capture 能整体替换它而不影响用户注册的额外出口。
const defaultExporterID = 1

func newLoggerService() *LoggerService {
	s := &LoggerService{
		exporters:  make(map[int]*Exporter, 2),
		seq:        defaultExporterID,
		bufferSize: DefaultBufferSize,
	}
	s.exporters[defaultExporterID] = &Exporter{
		Levels: map[string]LogLevel{"": LevelInfo},
		Export: func(m LogMessage) {
			fmt.Fprintf(os.Stderr, "[cordis:%s] %s: %s\n", m.Level, m.Name, m.Text)
		},
	}
	return s
}

// Exporter 注册一个额外出口，返回注销它的 Dispose（幂等）。
// 额外出口独立于默认出口：两者各按自己的 Levels 过滤。
// 注意它不是 fiber 效果——日志出口常需比注册它的实例活得久
// （例如整个进程共用一份落盘出口），需要随实例回收时自行登记 Effect。
func (s *LoggerService) Exporter(e *Exporter) Dispose {
	s.mu.Lock()
	s.seq++
	id := s.seq
	s.exporters[id] = e
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.exporters, id)
	}
}

// Silence 关闭默认出口（追加的 Exporter 不受影响）。
func (s *LoggerService) Silence() { s.setDefault(nil) }

// Capture 把默认出口替换为 fn（测试与嵌入方用）。
func (s *LoggerService) Capture(fn func(LogMessage)) {
	s.setDefault(&Exporter{Export: fn})
}

func (s *LoggerService) setDefault(e *Exporter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e == nil {
		delete(s.exporters, defaultExporterID)
		return
	}
	s.exporters[defaultExporterID] = e
}

// BufferSize 返回环形缓冲容量。
func (s *LoggerService) BufferSize() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bufferSize
}

// SetBufferSize 调整环形缓冲容量（≤0 表示不缓存）。
func (s *LoggerService) SetBufferSize(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n < 0 {
		n = 0
	}
	s.bufferSize = n
	if len(s.buffer) > n {
		s.buffer = append(s.buffer[:0], s.buffer[len(s.buffer)-n:]...)
	}
}

// Messages 返回缓冲内的消息快照（按时间序）。
func (s *LoggerService) Messages() []LogMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]LogMessage(nil), s.buffer...)
}

// Logger 派生日志器：name 省略时按调用方上下文解析
// （拦截配置的 name → 所属插件名）。
func (s *LoggerService) Logger(name ...string) *Logger {
	explicit := ""
	if len(name) > 0 {
		explicit = name[0]
	}
	return s.forContext(nil, explicit)
}

// forContext 以 ctx 解析名字与级别：显式名 > 拦截配置名 > 插件名。
func (s *LoggerService) forContext(ctx *Context, explicit string) *Logger {
	name, level, hasLevel, fiber, uid := explicit, LevelInfo, false, "root", 0
	if ctx != nil {
		if cfg, ok := ctx.InterceptOf("logger"); ok {
			if n, lv, ok := loggerConfig(cfg); ok {
				if name == "" {
					name = n
				}
				if lv != nil {
					level, hasLevel = *lv, true
				}
			}
		}
		if ctx.fiber != nil {
			fiber = ctx.fiber.name()
			uid = ctx.fiber.uid
		}
	}
	if ctx != nil && name == "" {
		name = fiber
	}
	if name == "" {
		name = "root"
	}
	if !hasLevel {
		level = LevelInfo
	}
	return &Logger{service: s, name: name, level: level, fiber: fiber, uid: uid}
}

// loggerConfig 解析两种形态的日志拦截配置。
// 返回 (name, 级别指针, ok)：级别指针为 nil 表示未指定。
func loggerConfig(cfg any) (string, *LogLevel, bool) {
	switch v := cfg.(type) {
	case LoggerOptions:
		name := v.Name
		if v.Level != LevelError {
			lv := v.Level
			return name, &lv, true
		}
		return name, nil, true
	case map[string]any:
		name, _ := v["name"].(string)
		var lv *LogLevel
		if raw, ok := v["level"]; ok {
			if parsed, ok := asLevel(raw); ok {
				lv = &parsed
			}
		}
		return name, lv, true
	}
	return "", nil, false
}

// asLevel 把拦截配置里的级别值规范化（LogLevel 或整型）。
func asLevel(raw any) (LogLevel, bool) {
	switch v := raw.(type) {
	case LogLevel:
		return v, true
	case int:
		return LogLevel(v), true
	case int32:
		return LogLevel(v), true
	case int64:
		return LogLevel(v), true
	}
	return LevelInfo, false
}

// Error / Warn / Info / Debug 以根日志器（名 "root"）输出。
// 组件内部请用 ctx.Logger() 以获得带插件名的日志器。
func (s *LoggerService) Error(format string, args ...any) {
	s.forContext(nil, "").log(LevelError, format, args...)
}
func (s *LoggerService) Warn(format string, args ...any) {
	s.forContext(nil, "").log(LevelWarn, format, args...)
}
func (s *LoggerService) Info(format string, args ...any) {
	s.forContext(nil, "").log(LevelInfo, format, args...)
}
func (s *LoggerService) Debug(format string, args ...any) {
	s.forContext(nil, "").log(LevelDebug, format, args...)
}

// emit 把消息派发给全部出口并记入环形缓冲。
//
// base 是日志器自身的阈值（拦截配置指定，缺省 LevelInfo）：出口的
// Levels 可按名覆盖它，缓冲则直接采用它——对应官方实现里那个
// "无 levels 的出口"（缓冲）与"出口级覆盖"的组合语义。
//
// 出口回调在锁外调用：出口自身再打日志（或注销自己）都不会死锁。
func (s *LoggerService) emit(m LogMessage, base LogLevel) {
	s.mu.Lock()
	s.seqMessage++
	m.Seq = s.seqMessage
	m.Time = time.Now()
	exporters := make([]*Exporter, 0, len(s.exporters))
	for _, e := range s.exporters {
		if e != nil && e.Export != nil {
			exporters = append(exporters, e)
		}
	}
	if m.Level <= base && s.bufferSize > 0 {
		s.buffer = append(s.buffer, m)
		if over := len(s.buffer) - s.bufferSize; over > 0 {
			s.buffer = append(s.buffer[:0], s.buffer[over:]...)
		}
	}
	s.mu.Unlock()

	for _, e := range exporters {
		if m.Level > e.threshold(m.Name, base) {
			continue
		}
		e.Export(m)
	}
}

// Logger 返回根上下文上的日志服务（= 未命名日志器的宿主）。
func (a *App) Logger() *LoggerService { return a.logger }
