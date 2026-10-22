# Day 18–19 — v1 收口前的三件遗留：级联回收、RBAC 审计、一键演示

> 状态：v1（Day 1–20）在这里收口。本文只记录**实测**，推断会显式标注。
> 环境：k3d `operator-demo`（k3s v1.35.5，单节点）+ Windows 11；kubectl v1.36.1。
> 与 docs/day07 的分工：day07 记的是「自愈/拉回偏移/HPA 让位」的演示，
> 本文补的是三件当时明确留到 Day 19 的事——**级联回收没验过、RBAC 没审计过、演示只存在于文档命令里**。

## 一、级联回收：删 CR 之后子资源多久消失

### 1.1 机制（先说清楚为什么该消失）

三个子对象（Deployment / Service / HPA）都通过
`controllerutil.SetControllerReference`（`shortlink_controller.go:196/251/368`）挂在了 CR 上，
`ownerReferences[].controller=true`。于是：

- CR 被删除 → API Server 不负责删子对象，**是 kube-controller-manager 里的 GC 控制器**扫到
  owner 已经不存在的对象，按级联策略删掉子对象，再连带删子对象的子对象（RS → Pod）；
- 本项目**没有 finalizer 逻辑**（`grep Finalizer internal/controller/*.go` 只命中一行 RBAC 标记，
  没有任何 `AddFinalizer`）——因为本项目不持有集群外的资源（DNS 记录、bucket 之类），
  进程里没有需要清理的东西，所以 CR 的删除是「立即从 etcd 消失」，不需要等控制器放行。

这两点决定了预期时间尺度：**CR 几乎立刻消失，子对象由 GC 异步回收**，量级是「一轮 GC 扫描」而不是「30 秒」。

### 1.2 实测

命令（PowerShell，节选；完整脚本思路见本节末尾）：

```powershell
kubectl apply -f D:\tmp\cascade-check.yaml          # replicas=1, image=nginx:1.27, port=80
# 等 status.readyReplicas == 1
$t0 = Get-Date
kubectl delete sl cascade-check --wait=false
# 每 100ms 轮询一次 deploy / svc / rs / pod 是否 NotFound
```

三轮结果（单位：秒，从 `delete sl` 返回算起）：

| 轮次 | Deployment | Service | ReplicaSet | Pod | 最后一个对象消失 |
|------|-----------|---------|-----------|-----|-----------------|
| 1 | 0.40 | 0.56 | 0.81 | 2.75 | **2.75s** |
| 2 | 0.36 | 0.54 | 1.00 | 2.99 | **2.99s** |
| 3（rbac-check，跳过 RS 单测） | — | — | — | — | **3.09s** |

顺序是稳定的：**Deployment → Service → ReplicaSet → Pod**，Pod 最后。
Pod 慢的原因**不是** GC（GC 早就把 owner 删了），而是 kubelet 的优雅终止宽限期：
容器收到 SIGTERM 后要退出，`nginx` 在这个环境里 1~3 秒内退出，所以 Pod 对象最后才消失。

> 顺带纠正一个容易误读的观察：第一轮里曾看到残留 Pod 的状态是 `Completed`，
> 而它 3 秒后就消失了。那是**正在终止**的 Pod（容器已正常退出，Pod 对象还在），
> 不是「孤儿 Pod」。判据是 `metadata.deletionTimestamp` 非空；孤儿 Pod 恰恰相反——
> 它没有任何 owner，也没有 deletionTimestamp，会一直躺在那里。

### 1.3 与 ROADMAP 的口径对齐

ROADMAP Day 13 的自检写的是「删 CR 后子资源 **10 秒内**清空」。实测 3 秒内到齐，
**但这是单节点 k3d + 空集群**的读数；GC 的扫描周期可配（kube-controller-manager 的
`--concurrent-gc-syncs` 与 GC 的 `terminations` 队列），节点数、对象数、API Server 负载都会拉长它。
所以正确的表述是「本环境 3 秒内」，不是「3 秒」。

## 二、RBAC 最小化审计（Day 18）

### 2.1 审计方法

两条命令就够，不需要读 RBAC 源码：

```bash
# 1) 这个 ServiceAccount 实际能干什么（权威答案来自 API Server，不是 role.yaml）
kubectl auth can-i --list --as=system:serviceaccount:shortlink-operator-system:shortlink-operator-controller-manager -n default

# 2) 逐条问"能不能做某件不该做的事"——这类问题比 --list 更容易看出越权
kubectl auth can-i get secrets               --as=$SA -n default   # → no
kubectl auth can-i list pods                 --as=$SA -n default   # → no
kubectl auth can-i delete shortlinks         --as=$SA -n default   # → no
```

`auth can-i --list` 的实际输出（截取资源部分）：

```
services                        [create delete get list patch update watch]
deployments.apps                [create delete get list patch update watch]
horizontalpodautoscalers.autoscaling [create delete get list patch update watch]
events                          [create patch]
events.events.k8s.io            [create patch]
shortlinks.shortlink.dev        [get list patch update watch]
shortlinks.shortlink.dev/status [get patch update]
```

对照「控制器代码真正需要什么」逐条核：

| 权限 | 用在哪 |
|------|--------|
| `services` / `deployments` / `horizontalpodautoscalers` 的 CRUD+watch | 期望构造、diff、Create/Update/Delete，`Owns()` 要求 list/watch |
| `events`（core/v1 与 events.k8s.io/v1 两组） | 失败/动作可见化；两组都授权是为消除「事件被 403 静默丢弃」整类故障，理由见 docs/day09 |
| `shortlinks` 的 get/list/watch/update/patch（**没有 create/delete**） | 控制器从不创建、也从不删除 CR——`create`/`delete` 缺席是**故意**的 |
| `shortlinks/status` 的 get/patch/update | `Status().Patch(MergeFrom(old))` |
| `deployments/status`、`services/status` 的 **get** | 读回真实状态（可用副本数、clusterIP）。注意这只是读子资源，不是写 |

### 2.2 审计发现：一条用不上的授权

`shortlinks/finalizers: update` 是 kubebuilder 脚手架带的默认行，但本项目没有 finalizer 逻辑
（第一节已说明原因）。**用不上的权限 = 白送的攻击面 = 审计时必被问「你为什么需要它」**，
所以这一行连同生成物一起删掉：

```diff
- // +kubebuilder:rbac:groups=shortlink.dev,resources=shortlinks/finalizers,verbs=update
  // +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
```

重生成（本机用 `hack/manifests.sh`，避开跑不动的 make）：

```
$ bash hack/manifests.sh
==> controller-gen paths=./api/v1alpha1 paths=./cmd paths=./internal/controller
controller-gen exit=0
$ git diff --stat config/rbac/role.yaml
 config/rbac/role.yaml | 6 ------
```

删掉后实测（重新 `apply` 到集群后）：

```
update shortlinks --subresource=finalizers: no
update shortlinks --subresource=status:     yes
```

### 2.3 一个必须记住的语法坑

`kubectl auth can-i update shortlinks/finalizers` 里斜杠**不是子资源**，是 `资源/名字`——
它问的是「能不能 update 那个叫 finalizers 的 shortlink」，答案永远是 yes（因为我们能 update 所有 shortlink），
看起来像「finalizers 权限还在」。真正的子资源要用 `--subresource`：

```bash
kubectl auth can-i update shortlinks --subresource=finalizers --as=$SA -n default
```

### 2.4 运行时确认：日志里没有 forbidden

审计只看「申请了什么」不够，还要看「跑起来有没有被拒」。
把创建/更新/删除 HPA/Deployment/Service、写 status、发事件五条路径都跑一遍后：

```
log lines: 149; forbidden: 0; event-write-fail: 0; errors: 0
```

`Unable to write event` 单独统计的原因：事件写失败**不会**让调和失败，只在 client-go 的
后台 goroutine 里 klog 打一行——不单独查，就等于没有。

## 三、一键演示：`hack/demo-v1.sh`

### 3.1 为什么不写成 Makefile target

本机 `make` 来自 chocolatey 的 GnuWin32 版本，跑 kubebuilder 生成的 Makefile 会在 shell
语法与路径分隔符上出错（`/bin/sh` 语义、`$(shell find ...)`、反斜杠路径），这不是配置问题。
所以脚本改成只依赖 `kubectl` + `kubectl kustomize` + `sed`：**不依赖 make、不依赖 kustomize 二进制**，
Git Bash 与 WSL 都能跑。

### 3.2 它跑什么

| 步骤 | 检查 |
|------|------|
| 0 预检 | 集群可达（context `k3d-<集群名>`）|
| 1 镜像 | 交叉编译 + 导入（`SKIP_BUILD=1` 可跳过）|
| 2 安装 | `kubectl apply -k config/crd`；`kubectl kustomize config/default` 里把 `controller:latest` 换成真实镜像后 apply；等 manager 可用 |
| 3 三件套 | CR → Deployment 就绪、Service 存在、`status.readyReplicas` 对齐 |
| 4 自愈 | 删 Deployment → 等回到就绪，并断言 **UID 变了**（证明是新对象，不是没删掉）|
| 5 拉回偏移 | 绕过 CR `kubectl scale` 到 +2 → 等副本数被拉回 |
| 6 滚动更新 | 改 `spec.env` → generation 递增、Pod 模板带上新值；滚动期间每 200ms 采样 `availableReplicas` |
| 7 可观测 | 打印 `status.conditions` 与本 operator 发的事件（`events.k8s.io`，按 `regarding.name` 过滤）|
| 8 级联回收 | 删 CR → Deployment/Service 消失 → 等 Pod 也消失 |

实测（两次全量 + 多次 `SKIP_BUILD=1`）：

```
PASS=16 FAIL=0        # 首次全量（含构建导入镜像）56s
PASS=16 FAIL=0        # SKIP_BUILD=1       29s
```

### 3.3 脚本里两处刻意的「不够硬」

1. **滚动零中断的采样分辨率不足。** 脚本每轮采样要一次 kubectl 往返（约 200–400ms），
   而 1 副本的 rollout 只花 1 秒 ⇒ 往往只采到 1 个点。脚本会在 `samples < 5` 时打印
   `[NOTE] 分辨率不够下结论`。真正的零中断结论来自 docs/day10 的常驻探针（约 10 req/s，
   对比 `maxUnavailable=0` 与默认 25% 的 66/5 次被拒）。
   ——这条和 day08 的「空文件求和成 0」、day10 的「读一份已经停止写入的日志」是同一类纪律：
   **先证明仪表在工作，再相信读数**。
2. **自愈耗时的口径与 docs/day07 不同。** day07 的「3 秒重建」是**对象重新出现**；
   脚本等的是**回到就绪**（Pod 调度 + 镜像在节点上 + 探针通过），实测 8 秒左右。
   两个数都对，但说的不是一件事——引用时必须带口径。

## 四、顺带记录：一次 409 与它的正确读法

在 RBAC 审计里「先关 autoscaling 再把 replicas 设成 2」这条路径上，控制器打了两行 ERROR：

```
ERROR Failed to reconcile Deployment ... error: "Operation cannot be fulfilled on deployments.apps \"rbac-check\":
the object has been modified; please apply your changes to the latest version and try again"
```

同时 CR 上出现 `Warning/DeploymentReconcileFailed` 事件。这不是新 bug，是 Day 9 那个
「所有权交接」窗口的必然产物：

- 同一个 reconcile 里我们先删 HPA、再更新 Deployment 的 `spec.replicas`；
- 而 HPA 控制器此刻可能刚把 Deployment 从 1 扩到 2（`Scaled` 事件可见），
  两次写撞在同一个 `resourceVersion` 上 ⇒ 我们的 `Update` 拿到 409。

行为上无害：`return err` → 进 rate limiter 退避 → 下一轮重新 Get 到最新版本再写。
但它确实花了**额外的调和**，也把一个无害竞态升级成了 Warning 事件。
**修法**是给子资源写加 `retry.RetryOnConflict`（读-改-写重试）；本项目**没有改**——
v1 收口阶段不做行为变更，这条记进 README 的「已知限制」，也记在这（诚实清单的一部分）。

## 五、v1 到此为止：范围声明

- **已完成**：CRD（spec/status/CEL 互斥校验）、控制循环（创建/更新/幂等 diff/失败可见化）、
  service+可选 HPA、滚动策略、自愈、status 子资源、事件（events.k8s.io）、envtest 14 个 spec、
  e2e（kind）、leader election、metrics、CI 三 job（Lint/Tests/E2E）、RBAC 最小化、一键演示、级联回收。
- **不做**：v2 的 FaultInjection CRD（Day 21–30）、v3 的 webhook / Helm chart / 博客（Day 35–39）。
  理由写在 ROADMAP 的「收口决定」一节：v1 已经是自洽可讲的交付面，v2/v3 是加分项，
  在有限准备时间内投入产出比不划算。
- 以后若要继续，第一件事应该是 **v2 Day 21**，因为 FaultInjection 承接的正是
  docs/day10 遗留里「故障注入靠手敲命令」这条真实缺口。
