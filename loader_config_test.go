package cordis_test

import (
	"fmt"
	"os"
	"testing"

	cordis "github.com/corecraft-io/cordis"
)

// TestLoaderConfigInterpolation 配置模板：${env:NAME} 在入口加载时
// 展开（分组配置是子入口列表，不参与插值——与官方 _resolveConfig 同款
// 分派）。官方的 interpolate 还支持跨入口引用与模板拼接，Go 侧只覆盖
// 最常用的一类，其余交给嵌入方，本层不发明私有语法。
func TestLoaderConfigInterpolation(t *testing.T) {
	h := newLoaderHarness(t)
	defer h.close()

	t.Setenv("CORDIS_TEST_DSN", "postgres://from-env")
	t.Setenv("CORDIS_TEST_PORT", "6543")

	var got []any
	h.plugins["db"] = &cordis.Plugin{
		Name:  "db",
		Apply: func(_ *cordis.Context, config any) error { got = append(got, config); return nil },
	}

	h.loader.Load([]cordis.EntryOptions{
		{ID: "plain", Name: "db", Config: "no-template"},
		{ID: "str", Name: "db", Config: "${env:CORDIS_TEST_DSN}"},
		{ID: "missing", Name: "db", Config: "dsn=${env:CORDIS_TEST_ABSENT}"},
		{ID: "map", Name: "db", Config: map[string]any{
			"dsn":  "${env:CORDIS_TEST_DSN}",
			"port": "${env:CORDIS_TEST_PORT}",
			"keep": 1,
		}},
		{ID: "list", Name: "db", Config: []any{"${env:CORDIS_TEST_DSN}", 2}},
	})
	if fmt.Sprint(got) != fmt.Sprint([]any{
		// 注意 Load 是协调式：同名插件的多个入口按配置序各自加载。
		"no-template",
		"postgres://from-env",
		"dsn=", // 未定义的环境变量展开为空串
		map[string]any{"dsn": "postgres://from-env", "port": "6543", "keep": 1}, // 非字符串原值照旧
		[]any{"postgres://from-env", 2},
	}) {
		t.Fatalf("interpolated configs: %v", got)
	}

	// 分组配置（子入口列表）不参与插值。
	h.plugins["group"] = &cordis.Plugin{
		Name:  "group",
		Apply: func(*cordis.Context, any) error { return nil },
	}
	h.loader.Load([]cordis.EntryOptions{
		{ID: "g", Name: "group", Group: true, Config: []cordis.EntryOptions{
			{ID: "c", Name: "db", Config: "${env:CORDIS_TEST_DSN}"},
		}},
	})
	child, err := h.loader.Tree().Resolve("g:c")
	if err != nil {
		t.Fatal(err)
	}
	// 子入口自己仍会展开（插值发生在每个入口各自加载时）。
	if child.Options().Config != "${env:CORDIS_TEST_DSN}" {
		t.Fatalf("group child config untouched: %v", child.Options().Config)
	}

	// Entry.Evaluate 对应官方 entry.evaluate。
	e, _ := h.loader.Tree().Resolve("plain")
	if v := e.Evaluate("${env:CORDIS_TEST_DSN}"); v != "postgres://from-env" {
		t.Fatalf("Evaluate: %q", v)
	}
	if v := e.Evaluate("literal"); v != "literal" {
		t.Fatalf("Evaluate passthrough: %q", v)
	}
	if os.Getenv("CORDIS_TEST_DSN") == "" {
		t.Fatal("env not set (test harness issue)")
	}
}

// TestPluginSimplify 配置回写前先过 Simplify（对应官方 Config.simplify）：
// 组件自更新时落回入口的是**可持久化形态**，而不是运行期展开的形态。
func TestPluginSimplify(t *testing.T) {
	h := newLoaderHarness(t)
	defer h.close()

	h.plugins["db"] = &cordis.Plugin{
		Name: "db",
		Validate: func(config any) (any, error) {
			// 运行期把 "host:port" 展开成结构体，Simplify 折叠回去。
			s, ok := config.(string)
			if !ok {
				return config, nil
			}
			return map[string]any{"raw": s, "expanded": true}, nil
		},
		Simplify: func(config any) any {
			m, ok := config.(map[string]any)
			if !ok {
				return config
			}
			return m["raw"]
		},
		Apply: func(*cordis.Context, any) error { return nil },
	}

	h.loader.Load([]cordis.EntryOptions{{ID: "db", Name: "db", Config: "alpha"}})
	e, _ := h.loader.Tree().Resolve("db")
	if e.Fiber().State() != cordis.StateActive {
		t.Fatalf("db should be active: %v", e.Fiber().State())
	}
	// 入口配置保持原始形态（Load 路径不经过 Simplify）。
	if e.Options().Config != "alpha" {
		t.Fatalf("load-time config: %v", e.Options().Config)
	}

	// 组件自更新：Validate 展开 → Simplify 折叠回可持久化形态。
	h.app.DoSync(func(*cordis.Context) { _ = e.Fiber().Update("beta") })
	h.app.Wait()
	if e.Options().Config != "beta" {
		t.Fatalf("write-back must go through Simplify: %v", e.Options().Config)
	}
	if got := e.Fiber().Config().(map[string]any)["raw"]; got != "beta" {
		t.Fatalf("runtime config: %v", e.Fiber().Config())
	}
}
