# Day 1 · 语法热身（warmup）

这个目录是 Day 1 的练习场。Day 3 用 `kubebuilder init` 生成正式骨架后，它和正式代码没有耦合，
学完可以直接删掉（或留着当并发玩具）。

## 你现在要做的事

### 1. 看坏代码怎么坏（5 分钟）

```powershell
go test -race ./warmup/
```

`warmup/counter.go` 里的 `Counter` 是**故意**写成并发不安全的。
上面的命令应该会让你看到 `WARNING: DATA RACE`，同时用例失败、报"丢了多少次更新"。

### 2. 亲手修好它（30–60 分钟）

不看文档，把 `warmup/counter.go` 改成并发安全。

两个方案都写一遍、都跑一遍测试：

| 方案 | 用什么 | 提示 |
|---|---|---|
| A | `sync.Mutex` | 给「读改写」这段临界区整体加锁；注意 `Value()` 也要加锁 |
| B | `sync/atomic` | `atomic.AddInt64` / `atomic.LoadInt64`；注意对齐（字段放 struct 第一个） |

验收：

```powershell
go test -race -count=3 ./warmup/    # 连跑 3 次全绿
```

### 3. 跑通 HTTP 服务（Day 1 的产出）

```powershell
go run ./warmup/cmd
```

（目录结构：`warmup/counter.go` 是 `package warmup`，`warmup/cmd/main.go` 是 `package main`。
将来 kubebuilder 生成的 `cmd/main.go` 也长这样——一个 main 包去 import 别的包。
根部不能放 `cmd/`，那是 Day 3 `kubebuilder init` 要占的地盘。）

另开一个终端：

```powershell
curl.exe "http://localhost:8080/"
curl.exe "http://localhost:8080/inc?workers=50&perWorker=1000"
curl.exe "http://localhost:8080/sum?n=10"
```

`/inc` 会告诉你期望值 vs 实际值。改成并发安全之前它是 ❌，改完之后是 ✅ ——
这就是你 Day 1 的「行为证据」。

### 4. 加分（可选）

```powershell
go run -race ./warmup/cmd
curl.exe "http://localhost:8080/inc?workers=200&perWorker=5000"
```

用 `-race` 跑那个**坏**版本：race detector 会在请求处理过程中直接把竞争打印出来。
记住这个感觉 —— Day 14 之后你写 envtest，`-race` 就是你的默认开关。

## 自检（ROADMAP Day 1）

- [ ] 不查文档能写出并发安全的计数器（Mutex + goroutine）
- [ ] `go run ./warmup/cmd` 能起来，`/inc` 返回 ✅
- [ ] 能用自己的话解释：为什么 `count++` 会丢更新
