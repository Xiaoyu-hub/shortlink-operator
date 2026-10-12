# controller-runtime v0.25.0（Windows 补丁副本）

本目录是 `sigs.k8s.io/controller-runtime@v0.25.0` 的完整副本，仅改动一个文件，
通过 `go.mod` 的 `replace` 指令指向本目录（为什么：见下）。

## 改动内容（1 个标识符）

`pkg/internal/testing/process/signal_windows.go`：

```diff
-func signalProcess(process *os.Process, _ syscall.Signal) error {
+func signalProcessImpl(process *os.Process, _ syscall.Signal) error {
 	return process.Kill()
 }
```

## 为什么要有这个补丁

上游 `process.go:132` 无条件声明 `var signalProcess = signalProcessImpl`，
而 `signalProcessImpl` 只在 `signal_unix.go`（linux/darwin/…）与 `signal_other.go` 中定义，
两者都排除了 windows；Windows 专用的 `signal_windows.go` 却定义了 `func signalProcess`。
结果在 Windows 上编译 envtest 包时：

- `signalProcess` 重复声明（var 与 func）
- `signalProcessImpl` 未定义

`go test ./...`（envtest 单测）与 `go vet ./...` 因此直接 build failed。
该 bug 在 v0.25.1 / v0.25.2（截至 2026-09-30 的上游最新发布版）中仍然存在，
本机网络无法访问 github.com（DNS 被劫持到 127.0.0.1），无法核对上游 main 分支是否已修，
于是采用「本地补丁模块」方案：把一个标识符改名对齐，其余代码逐字节未动。

验证方式：本目录内 `git diff` 仅应有这一个文件、一行变化。

## 什么时候可以删掉

一旦上游发布修复版（例如 v0.25.3+ 或后续版本线），在 `go.mod` 中升级
`require sigs.k8s.io/controller-runtime` 并删除 `replace` 与本目录即可。
判断标准：`go test ./internal/controller/...` 在 Windows 上能编译通过。