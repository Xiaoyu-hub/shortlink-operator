// Command warmup —— Day 1 语法热身。
//
// 目标：把 controller-runtime 的 reconciler 真正会用到的那三样东西，串成一个能跑的服务。
//
//	goroutine —— 并发执行单元。Reconcile 本身就是被 controller-runtime 用多个
//	             worker goroutine 并发调用的，所以你的调和逻辑必须并发安全。
//	channel   —— goroutine 之间的通信/信号。controller-runtime 内部的工作队列
//	             （workqueue）本质就是带限流语义的 channel。
//	interface —— 隐式实现的抽象。你以后要实现的 reconcile.Reconciler 就是一个
//	             interface，只要实现 Reconcile(ctx, req) (Result, error) 一个方法；
//	             这里的 http.Handler 是同一个套路。
//
// 跑起来：
//
//	go run ./warmup/cmd
//
// 然后另开一个终端（PowerShell 里用 curl.exe，别用 curl 别名）：
//
//	curl.exe "http://localhost:8080/"
//	curl.exe "http://localhost:8080/count"
//	curl.exe "http://localhost:8080/inc?workers=50&perWorker=1000"
//	curl.exe "http://localhost:8080/sum?n=10"
//
// 想把并发 bug 也抓出来，加 -race：
//
//	go run -race ./warmup/cmd
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/yourname/shortlink-operator/warmup"
)

// counter 是全局共享状态。它会被多个 HTTP handler goroutine 同时访问 ——
// 这正是 warmup.Counter 必须并发安全的原因。
var counter warmup.Counter

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/count", handleCount)
	mux.HandleFunc("/inc", handleInc)
	mux.HandleFunc("/sum", handleSum)

	srv := &http.Server{
		Addr:    ":8080",
		Handler: logRequests(mux), // 用中间件包一层，顺手演示 interface
	}

	// channel 用法之一：接操作系统信号。
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// 主 goroutine 去跑 server；<-stop 这行后面阻塞等待信号。
	// 这就是 Go 最典型的服务骨架：一个 goroutine 干活，主 goroutine 等退出信号。
	go func() {
		log.Println("listening on http://localhost:8080  (Ctrl+C 退出)")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-stop
	log.Println("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
}

// logRequests 是一个中间件：吃一个 http.Handler，吐一个 http.Handler。
//
// 这就是 Go 接口的威力 —— 只要一个类型实现了 ServeHTTP(ResponseWriter, *Request)，
// 它自动就是 http.Handler，不需要任何 implements 声明。
// 你以后写的 Reconciler 也一样：实现 Reconcile 方法，就自动是 reconcile.Reconciler。
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start))
	})
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, `shortlink-operator · Day 1 warmup

GET /healthz                        存活探针
GET /count                          读当前计数
GET /inc?workers=50&perWorker=1000  起 N 个 goroutine 并发打计数器，比对期望/实际
GET /sum?n=10                       纯 channel 演练：n 个 goroutine 算 i*i，主 goroutine 汇总
`)
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

func handleCount(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "count = %d\n", counter.Value())
}

// handleInc 是 goroutine + 共享状态演练。
//
// 它起 workers 个 goroutine，每个跑 perWorker 次 counter.Inc()，
// 用 sync.WaitGroup 等它们全部结束，然后把「期望值 vs 实际值」摆出来。
//
// Counter 还没改成并发安全之前，这里的 actual 会小于 expected —— 你亲眼看到丢更新。
func handleInc(w http.ResponseWriter, r *http.Request) {
	workers := clamp(intParam(r, "workers", 50), 1, 1000)
	perWorker := clamp(intParam(r, "perWorker", 1000), 1, 100000)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				counter.Inc()
			}
		}()
	}
	wg.Wait()

	expected := int64(workers) * int64(perWorker)
	got := counter.Value()

	fmt.Fprintf(w, "goroutines = %d, perWorker = %d\n", workers, perWorker)
	fmt.Fprintf(w, "expected   = %d\n", expected)
	fmt.Fprintf(w, "actual     = %d\n", got)
	if got != expected {
		fmt.Fprintf(w, "❌ 丢了 %d 次更新 —— Counter 还不并发安全\n", expected-got)
		return
	}
	fmt.Fprintln(w, "✅ 一致")
}

// handleSum 是纯 channel 演练：
// n 个 goroutine 各算一个平方数，通过 channel 把结果发回主 goroutine 汇总。
//
// 注意这里【没有共享内存】—— 数据是"传"过去的，不是"抢"着读写的。
// Go 的谚语：不要通过共享内存来通信，而要通过通信来共享内存。
func handleSum(w http.ResponseWriter, r *http.Request) {
	n := clamp(intParam(r, "n", 10), 1, 10000)

	results := make(chan int, n) // 带缓冲区，发送方不必等接收方就绪
	for i := 1; i <= n; i++ {
		go func(i int) {
			results <- i * i
		}(i)
	}

	total := 0
	for i := 0; i < n; i++ {
		total += <-results
	}

	fmt.Fprintf(w, "sum(i*i, i=1..%d) = %d\n", n, total)
}

func intParam(r *http.Request, name string, def int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return v
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
