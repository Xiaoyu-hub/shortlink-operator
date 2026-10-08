package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 这一组用例是"HTTP 层的 envtest 预演"：不起真端口、不用 curl，
// 直接把被测的路由组装出来打请求、断言响应。
//
// Day 14 之后我们会用同样的思路对付 CRD：
//
//	httptest.NewServer / envtest.Environment  —— 起真实的"服务器"
//	req := httptest.NewRequest(...)            —— 构造真实输入
//	断言响应                                    —— 断言 .status / 子资源
//
// 先把"可测试性">写进结构里（newMux 就是为此抽出来的），后面的事都顺。

// do 是测试辅助函数：发一个请求，返回状态码和响应体。
func do(t *testing.T, target string) (int, string) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()

	newMux().ServeHTTP(rec, req)

	return rec.Code, rec.Body.String()
}

func TestHealthz(t *testing.T) {
	code, body := do(t, "/healthz")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !strings.Contains(body, "ok") {
		t.Fatalf("body = %q, want 包含 ok", body)
	}
}

func TestIndex(t *testing.T) {
	code, body := do(t, "/")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !strings.Contains(body, "Day 1 warmup") {
		t.Fatalf("body 里没有 Day 1 warmup：%q", body)
	}

	// "/" 是最宽的通配路由，其它未知路径必须落到 404，
	// 不能因为挂了 "/" 就把不存在的路径也当成首页 200。
	if code, _ := do(t, "/nope"); code != http.StatusNotFound {
		t.Fatalf("未知路径 status = %d, want 404", code)
	}
}

func TestSum(t *testing.T) {
	cases := []struct {
		name   string
		target string
		want   string
	}{
		{name: "n=10", target: "/sum?n=10", want: "= 385"},
		{name: "n=1", target: "/sum?n=1", want: "= 1"},
		{name: "非法参数回落默认值", target: "/sum?n=abc", want: "= 385"},
		{name: "越界参数被钳制到上限", target: "/sum?n=999999999", want: "sum(i*i, i=1..10000)"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := do(t, tc.target)
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200", code)
			}
			if !strings.Contains(body, tc.want) {
				t.Fatalf("body = %q, want 包含 %q", body, tc.want)
			}
		})
	}
}

// TestIncAllKinds 是三种计数器实现的端到端一致性验证。
func TestIncAllKinds(t *testing.T) {
	for _, kind := range []string{"mutex", "atomic", "sharded"} {
		t.Run(kind, func(t *testing.T) {
			code, body := do(t, "/inc?kind="+kind+"&workers=16&perWorker=250")
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200", code)
			}
			if !strings.Contains(body, "expected   = 4000") {
				t.Fatalf("期望增量不对：%q", body)
			}
			if !strings.Contains(body, "✅ 一致") {
				t.Fatalf("inc 结果不一致：%q", body)
			}
		})
	}
}

func TestIncDefaultKindIsMutex(t *testing.T) {
	// 不传 kind 必须能工作，且默认实现就是 mutex。
	code, body := do(t, "/inc?workers=4&perWorker=10")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !strings.Contains(body, "kind       = mutex") {
		t.Fatalf("默认 kind 不是 mutex：%q", body)
	}
}

func TestUnknownKindIsRejected(t *testing.T) {
	for _, target := range []string{"/inc?kind=nope", "/count?kind=nope"} {
		t.Run(target, func(t *testing.T) {
			code, _ := do(t, target)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", code)
			}
		})
	}
}

func TestCountReadsRequestedKind(t *testing.T) {
	// 先给 sharded 打一点量，确认 /count 读的确实是它。
	if code, body := do(t, "/inc?kind=sharded&workers=2&perWorker=5"); code != http.StatusOK || !strings.Contains(body, "✅") {
		t.Fatalf("预置增量失败：status=%d body=%q", code, body)
	}

	code, body := do(t, "/count?kind=sharded")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !strings.Contains(body, "kind = sharded") {
		t.Fatalf("count 读错了实现：%q", body)
	}
}

// TestIntParamClamp 覆盖参数钳制：上限 1000 个 goroutine，避免一条 curl 打爆测试机。
func TestIntParamClamp(t *testing.T) {
	code, body := do(t, "/inc?kind=atomic&workers=99999&perWorker=1")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !strings.Contains(body, "goroutines = 1000") {
		t.Fatalf("workers 没有被钳制到 1000：%q", body)
	}
}
