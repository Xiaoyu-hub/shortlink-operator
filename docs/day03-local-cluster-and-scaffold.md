# Day 3 — 本地集群 + kubebuilder 脚手架跑通

> 本文件是 Day 3 的产出：装齐工具 → `kubebuilder init` 出骨架 → 通读 Makefile →
> 建一个隔离的本地集群 → 让 manager「什么都没有也先跑起来」。
> 本机是 Windows，所以这一天的真正成本不在 K8s 概念，而在**把 POSIX 味的脚手架搬进 Windows**，
> 下面第 4、5 节是实测踩出来的坑，每条都带复现命令和修法。

---

## 一、环境事实（实测）

| 组件 | 版本 / 路径 | 备注 |
|---|---|---|
| Go | `go1.27.1 windows/amd64` | `D:\software\go\bin\go.exe` |
| kubebuilder | `C:\Users\10235\go\bin\kubebuilder.exe` | CLI `cliVersion: 4.16.0`（写进 `PROJECT`） |
| k3d | `C:\Users\10235\AppData\Local\Microsoft\WinGet\Links\k3d.exe` | 用它当本地集群（kind 本机未装） |
| kubectl | `C:\Users\10235\AppData\Local\Programs\DockerDesktop\resources\bin\kubectl.exe` | Docker Desktop 自带 |
| Docker | 29.7.2（Docker Desktop 运行中） | 数据在 D 盘 |
| MSYS2（提供 `sh`/`mkdir`/管道） | `C:\msys64\usr\bin` | **make 必须在它或 Git Bash 里跑**，见第 4 节 |
| make | `C:\ProgramData\chocolatey\bin\make.exe` | GNU make（choco 装） |
| GCC/`cgo` | 可用（Day 1 已验 `-race`） | |

**记住这一条**：以后每天开工，先把 make 跑在 bash 里，命令固定成下面这个样子（后面会脚本化）：

```bash
export PATH="/c/ProgramData/chocolatey/bin:/d/software/go/bin:/c/Users/10235/AppData/Local/Microsoft/WinGet/Links:/c/Users/10235/AppData/Local/Programs/DockerDesktop/resources/bin:$PATH"
cd /d/program/open/shortlink-operator && make build
```

---

## 二、脚手架：一条命令生成的骨架

```bash
kubebuilder init --domain shortlink.dev --repo github.com/Xiaoyu-hub/shortlink-operator
```

`PROJECT` 里落下的元数据（这是 CLI 的「账本」，之后 `create api`/`create webhook` 都读它）：

```yaml
cliVersion: 4.16.0
domain: shortlink.dev
layout:
- go.kubebuilder.io/v4
projectName: shortlink-operator
repo: github.com/Xiaoyu-hub/shortlink-operator
version: "3"
```

生成物与作用（Day 4 开始天天打交道）：

| 路径 | 作用 | 能不能手改 |
|---|---|---|
| `cmd/main.go` | manager 入口：建 manager → 注册 controller/webhook → `Start` | 可改（唯一手写入口） |
| `internal/controller/` | 调和逻辑（Day 6 起在这里写） | 手写 |
| `api/v1/` | CRD 类型定义（Day 4 起在这里写） | 手写 |
| `config/crd/bases/` | 由 markers 生成的 CRD YAML | **不可手改**（`make manifests` 产物） |
| `config/rbac/role.yaml` | 由 markers 生成的 RBAC | **不可手改** |
| `config/manager/`、`config/default/` | manager Deployment + kustomize 入口 | 偶尔改 kustomize patch |
| `config/samples/` | 示例 CR | 手写 |
| `Makefile` | 全部日常动作 | 可改（今天就被我们改了，见第 4 节） |
| `test/`、`hack/`、`Dockerfile`、`.github/` | envtest/e2e 骨架、boilerplate、镜像、CI | 骨架，Day 31+ 动 |
| `AGENTS.md` | kubebuilder 4.16 给 AI 助手生成的开发约定 | 可改 |

**关键认识**：脚手架里已经给好「最佳实践的形状」——
`cmd/main.go` 只做装配，真正的逻辑在 `internal/controller`，
CRD 的 schema 从 Go markers 单向生成到 `config/crd/bases`（所以那些 YAML 永远不要手改）。

---

## 三、Makefile 目标地图（每个目标什么时候用）

| 目标 | 依赖 | 干什么 | 我们的使用节奏 |
|---|---|---|---|
| `manifests` | controller-gen | 由 markers 生成 CRD + RBAC YAML | 每次改 `*_types.go` / RBAC markers 后 |
| `generate` | controller-gen | 生成 `zz_generated.deepcopy.go` | 每次改 `*_types.go` 后 |
| `fmt` / `vet` | — | `go fmt ./...` / `go vet ./...` | 每天收尾 |
| `build` | manifests+generate+fmt+vet | 编译出 `bin/manager` | 每天收尾自检 |
| `run` | 同上 | `go run ./cmd/main.go`，本机进程连集群 | 开发主力 |
| `install` / `uninstall` | kustomize+kubectl | 装/卸 CRD（Day 3 还是空的） | 换集群后 |
| `deploy` / `undeploy` | docker-build + kustomize | 把 manager **部署进集群** | 里程碑演示 |
| `test` | setup-envtest | envtest 单测（真 API Server + etcd，不起集群） | Day 5 起天天跑 |
| `test-e2e` | kind | 端到端（**需要 kind**，本机未装，Day 20 前补） | 里程碑 |

一句话：**`run` 是本机进程 + 集群 API Server；`deploy` 是把进程搬进集群。** 前者调试快，后者才是「交付形态」。

---

## 四、坑 1：make 在 PowerShell/cmd 里跑不了

**现象**（`make build` 直接失败）：

```
Makefile:184: pipe: No such file or directory
process_begin: CreateProcess(NULL, pwd, ...) failed.
make: *** [Makefile:185: /bin] Error 1        ← 注意它竟然想 mkdir "/bin"
```

**原因**：GNU make 找不到 `sh`，退回 `cmd.exe` 执行 `$(shell pwd)`，
`pwd` 失败 → `LOCALBIN ?= $(shell pwd)/bin` 变成 `/bin` → 于是它真的去 `mkdir -p "/bin"`。

**修法（不改 Makefile）**：在 MSYS2/Git Bash 里跑 make，`sh` 在 PATH 里，`pwd`/`mkdir -p`/管道全部正常。

**如果非要留在 PowerShell**：只能绕过 make，手写等价命令——
`go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.22.0 crd paths=./cmd/...` 之类，
但这等于把 Makefile 重写一遍，不划算。**结论：本项目统一用 bash + make。**

---

## 五、坑 2：`make manifests` 的 `paths="./..."` 在 Go 1.27 上必失败

**现象**：

```
"bin/controller-gen" rbac:roleName=manager-role crd webhook paths="./..." ...
-: no Go files in D:\program\open\shortlink-operator
Error: not all generators ran successfully
```

**复现（关键证据）**：新建一个干净模块（只有 `go.mod` + `cmd/main.go`，与脚手架布局一致）后跑同一个工具，同样报错：

```
$ cd /d/tmp/cgtest && go mod init example.com/cgtest && printf 'package main\n\nfunc main() {}\n' > cmd/main.go
$ /d/program/open/shortlink-operator/bin/controller-gen crd paths="./..." ...
-: no Go files in D:\tmp\cgtest          ← 不是本仓库的问题，是工具链+工具版本的问题
```

**根因**：controller-tools v0.22.0 里的 `go/packages` 在 Go 1.27 下把
`./...` 里的「模块根目录」也当成一个包去加载；而 kubebuilder 布局的模块根**没有 .go 文件**
（入口在 `cmd/main.go`），于是加载失败、整个生成器退出。
注意：这不是 K8s/Operator 的知识点，纯粹是「工具版本交叉」的坑，但**不修它就一步都走不动**。

**修法（我们改在 Makefile，最小区分度）**：

```make
# 只枚举「当前真实存在」的包目录；wildcard 是 make 内建，不依赖 shell
PKG_PATHS := $(foreach d,$(wildcard cmd api internal pkg),paths=./$(d)/...)
```

然后 `manifests` / `generate` 两个目标都用 `$(PKG_PATHS)` 取代 `paths="./..."`。
好处：`api/`、`internal/` 还没建出来时它自动跳过，Day 4 建出来后自动被纳入，**不需要再改 Makefile**。

**反例（不要这样写）**：`paths=./cmd/... paths=./api/...` 这种固定枚举，
在 `api/` 还不存在时会直接报 `chdir ...\api: The system cannot find the file specified.`
（controller-gen 把每个 `paths=` 当成一个必须存在的根）。

**验证**：

```
$ make manifests generate fmt vet build
"bin/controller-gen" rbac:roleName=manager-role crd webhook paths=./cmd/... output:crd:artifacts:config=config/crd/bases
go fmt ./...        ← 顺手把 warmup/racecheck/main.go 格式化了（Day 1 的代码没跑过 gofmt）
go vet ./...
go build -o bin/manager cmd/main.go
$ ls -l bin/manager  →  76,513,280 bytes   ✅
```

---

## 六、本地集群：k3d `operator-demo`

```bash
k3d cluster create operator-demo --kubeconfig-switch-context=false --wait
k3d kubeconfig get operator-demo > D:\tmp\kubeconfig-operator-demo.yaml
KUBECONFIG="D:\tmp\kubeconfig-operator-demo.yaml" kubectl get nodes
```

实测结果：

```
INFO[0023] Cluster 'operator-demo' created successfully!
NAME                         STATUS   ROLES           AGE   VERSION
k3d-operator-demo-server-0   Ready    control-plane   10s   v1.35.5+k3s1
```

**两个刻意的设计**：

1. `--kubeconfig-switch-context=false` + 把 kubeconfig 单独导出到 `D:\tmp\kubeconfig-operator-demo.yaml`：
   项目一（sre-from-zero）的 `k3d-sre-demo` 集群还在跑，**绝不切换全局 current-context** —— 避免「以为在 A 集群，实际把东西 apply 到 B 集群」这种最贵的错误。
   以后所有本项目命令都显式带 `KUBECONFIG=...`。
2. **隔离**：本项目自己的 CRD/Deployment/Pod 全在 `operator-demo` 里，跟项目一的短链服务互不污染
   （这也正是 AGENTS.md 里「e2e 要用隔离集群」的同一条原则）。

`make install`（Day 3 还没有 CRD）：

```
No CRDs to install; skipping.
```

这不是失败，而是**证据**：`config/crd/bases/` 空 → 说明 CRD 是 Day 4 才产出的东西。

---

## 七、自检：manager「什么都没有也先跑起来」

```bash
KUBECONFIG="D:\tmp\kubeconfig-operator-demo.yaml" ./bin/manager
```

```
2026-10-09T21:17:23+08:00	INFO	setup	Starting manager
2026-10-09T21:17:23+08:00	INFO	starting server	{"name": "health probe", "addr": "[::]:8081"}
```

健康探针实测（这一对 200 就是「空转跑起来了」的硬证据）：

```
$ curl -s -w " [http_code=%{http_code}]\n" http://localhost:8081/healthz
ok [http_code=200]
$ curl -s -w " [http_code=%{http_code}]\n" http://localhost:8081/readyz
ok [http_code=200]
```

## 八、走完 Day 3 我能做到的事

- [x] 用 `kubebuilder init` 生成骨架，并说清每个生成目录「谁生成、能不能手改」
- [x] 通读 Makefile：知道 `run` 与 `deploy` 的区别、`manifests/generate` 是单向生成、`test` 用 envtest 不起集群
- [x] 在 Windows 上让 make 正常工作（bash + MSYS2），并说清 `mkdir "/bin"` 那个报错是怎么来的
- [x] 定位并修掉 `paths="./..."` 在 Go 1.27 下的失败（含干净仓库复现证据），修法对新目录自动生效
- [x] 建出自己的隔离集群 `operator-demo`（不污染 sre-demo），并让 manager 空转 + 健康检查 200
- [ ] 遗留：`kind` 未装（`make test-e2e` 需要，Day 20 里程碑前补）
