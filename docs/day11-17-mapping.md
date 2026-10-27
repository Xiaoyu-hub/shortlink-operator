# Day 11–17 对账：谁实现了、证据在哪、缺口是什么

> 写于 2026-10-26（v1 收口之后）。这份文档是**补记**，不是当时的交付记录。
>
> 为什么要有它：`docs/` 目录只有 `day02`…`day10` 和 `day18-19`，`git log` 里也没有 `day11`–`day17` 的提交。
> 任何人（包括三个月后的我）翻到这里都会问一句「Day 11–17 是不是没做」。真实情况是**做了，但没有逐日留痕**——
> 那几天的活被折叠进 `day05` / `day06` / `day07` 的提交里了。本文把时间线和证据对平，
> 顺便把三个真缺口写在明面上。

## 一、结论先说

| 判断 | 结论 |
|---|---|
| Day 1–20 的功能是否落地 | 是。CRD、控制循环、自愈、status、事件、envtest、RBAC、真集群联调全部有可执行证据 |
| Day 11–17 是否有独立文档 | 没有（本文是补记） |
| Day 11–17 是否有独立提交 | 没有，内容分散在 `02dafe4`(day5)、`4095c5b`(day6)、`628fab7`/`db54e2a`(day7)、`0ae5eb3`(幂等口径)、`68c844f`(打印列) 等提交 |
| 有没有「写了 ROADMAP 但根本没做」的项 | 只有两处口径不同：Day 15 的「删 CR 回收」在 envtest 里原理上测不了（改由真集群验），Day 17 的「两个 CR 并行不互污染」当时无用例（2026-10-26 已补） |

## 二、逐日对账

表里「落地位置」只写**生产代码的函数名**与**测试的 It 名**，不写行号——
测试文件一旦插入用例，行号就漂；函数名与 It 名是稳定锚点（`grep -n` 即可定位）。

| Day | ROADMAP 要求 | 落地位置（代码） | 证据（行为/实测） | 状态 |
|---|---|---|---|---|
| 10 | 改 `spec.image` 触发标准滚动，`maxUnavailable`/`maxSurge` 生效 | `desiredStrategy`、`rollingUpdateDiffers`、`readinessProbeEqual`（都在 `shortlink_controller.go`） | It「声明的 maxSurge/maxUnavailable 与 readinessProbe 都落到 Deployment 上」；`docs/day10` 实测：`maxUnavailable=0` 时压测探针 **0 次**连接被拒，默认 25% 时一轮滚动 **66 / 5 次**被拒 | 完成 |
| 11 | 实际 ready 数 < spec → 恢复期望；Deployment 被删/被改 → 拉回；跟踪 observedGeneration | `reconcileDeployment`（先查后建 + 偏移检测）、`deploymentNeedsUpdate` 比较 `replicas`、`ownsReplicas`（声明 autoscaling 时让位）、`observeDeployment` | It「删掉 Deployment 后由控制循环重建（新 UID + ownerReference + spec 一致）」；`docs/day07` 真集群：手工改副本/镜像 **0.3–0.5s** 被拉回，删 Deployment **3s 内**重建；`hack/demo-v1.sh` 有对应断言 | 完成（自检口径见缺口 2） |
| 12 | 回写 `readyReplicas`/`conditions`，读写分离；`kubectl get` 有 READY 列 | `updateStatus`、`patchStatus`（`Status().Patch(client.MergeFrom(old))`，DeepEqual 无变化不写）、`readyCondition`、`expectedReplicas` | It「伪造 Deployment 就绪后，status 回写 readyReplicas / observedGeneration / Ready 条件」；打印列 `Observed`/`Generation` 来自 `bc84493`、`68c844f` | 完成 |
| 13 | 关键动作发事件；删 CR → 子资源 10 秒内清空 | `Recorder.Eventf` 共 10 处：`Created`/`Updated`/`Deleted`/`Scaled`/`Warning`（见 `reconcileDeployment`、`reconcileService`、`reconcileHPA`、`reportFailure`）+ `SetControllerReference` | 测试用 `eventTypesAndReasonsFor` 断言 `Warning/DeploymentReconcileFailed`；级联回收真集群实测 Deployment 0.36–0.40s / Service 0.54–0.56s / Pod 2.75–3.09s（`docs/day18-19` 第一节）；`hack/demo-v1.sh` 第 16 项 | 完成 |
| 14 | envtest 起真 apiserver，`make test` 能跑真控制循环 | `internal/controller/suite_test.go`（`TestControllers` 启动**真 manager**，不是手调 `Reconcile`） | 15 个 spec 全绿（`go test ./internal/controller/... -count=1`，2026-10-26 实测 29–44s） | 完成 |
| 15 | 创建 / 更新 / 删除三态用例 | 创建、更新两条路径在生产代码里是 `reconcileDeployment` 的两支 | It「创建 CR 后自动调和出 Deployment 与 Service」、It「修改 spec.image 后 Deployment 被更新」 | 部分：**删除态测不了**，见缺口 2 |
| 16 | 删子资源 → 一次 reconcile 自动重建 | `Owns(&appsv1.Deployment{})` 反查入队 + 先查后建 | It「删掉 Deployment 后由控制循环重建」——断言 **UID 变化**，而不是「对象还在」（后者只能证明删除没生效） | 完成 |
| 17 | 连续 reconcile 无 diff；两个 CR 并行互不污染；覆盖核心分支 | 期望构造是纯函数（`desiredDeployment` 只读 cr），子对象按**名字** Get 而不是按宽 selector List | 幂等：`docs/day05 §10.4` 先推翻了「RV 不变 = 零写入」这个旧判据，`docs/day08` 改用 `rest_client_requests_total` 求差实测：**16 次空转调和 → 0 个写请求；1 次真实变更 → 恰好 1 个 PATCH**。隔离：2026-10-26 新增 It「各自只写自己的子资源；改 A 的镜像、删 B 的 Deployment 都不影响对方」 | 完成（隔离用例为补做） |

## 三、为什么没有逐日文档

不是偷懒，是**开发节奏和 ROADMAP 的切法不一样**。ROADMAP 按「一天一个能力点」切，
实际是「一次提交关掉一个能力面」，于是一次提交覆盖了 ROADMAP 的好几天：

| 提交 | ROADMAP 里对应 |
|---|---|
| `02dafe4` feat(day5): Deployment/Service 调和 + envtest 真控制循环 | Day 7、Day 9、Day 14 |
| `68c844f` / `bc84493` 打印列 | Day 12 的可观测面 |
| `0ae5eb3` / `15160ef` 幂等判据与取证探针 | Day 8、Day 17 |
| `4095c5b` feat(day6): 失败可见化 + 自愈用例固化 | Day 11、Day 13、Day 16 |
| `9539d55`/`ee23ac2`/`adb8c62` selector 漂移、mutable-wipe 实测 | Day 17 的幂等边界 |
| `628fab7` feat(day7): readinessProbe + 滚动策略 | Day 10 |
| `db54e2a` docs(day7): 真集群自愈演示 | Day 11、Day 19 |

也就是说：**文档缺的是「Day 11–17」这个编号，不是那些天的工作**。
强行补七篇逐日文档只会产出七篇互相重复的说明——本文的表格是更有用的形态。

## 四、三个真缺口（写在明面上）

### 缺口 1：Day 11–17 没有当日文档

影响的是**面试追问时的可查性**：如果对方顺着 ROADMAP 问「Day 14 你做了什么」，我原本只能口头回忆。
处置：本文补上对账表；不再补逐日长文。**这是记录缺口，不是实现缺口。**

### 缺口 2：`删 CR → 子资源回收` 在 envtest 里测不出来（原理限制，不是漏做）

envtest = etcd + kube-apiserver，**没有 kube-controller-manager 里的 GC 控制器**。
ownerReference 已经挂上了，但「谁去执行级联」这件事不在 envtest 的进程里，所以：

- 用例能证明的：ownerRef 正确（Kind/Name/Controller=true）、新对象仍由同一 CR 所有
- 用例证明不了的：删 CR 后子对象会消失

处置：这一条改由真集群验（`docs/day18-19` 第一节，三轮实测 2.75–3.09s），并进了 `hack/demo-v1.sh` 的断言。
同理，`readyReplicas` 回到期望值、「30 秒内自愈」这类**需要 kubelet 造 Pod** 的自检项，也只能在真集群验。

### 缺口 3：Day 17 的「两个 CR 并行互不污染」当时没有用例

串味（cross-talk）的典型来源有三个，本项目恰好都没踩——但「没踩」只是当场的观察：

1. 拿包级变量/共享缓冲构造期望值 → 本项目 `desiredDeployment` 是纯函数，只读 cr
2. 按宽 selector 去 List 别人的子资源 → 本项目按 `(namespace, name)` Get
3. 用 CR 名字之外的键定位子对象 → 子对象名恒等于 CR 名

处置：2026-10-26 补了用例 `Context("隔离：两个 CR 并行调和互不污染")`，断言四层：

- 每个 Deployment/Service 的 controller ownerRef 指向各自的 CR，selector 里的 `instance` 只指自己
- 三处刻意不同的 spec（副本 1 vs 3、镜像 v0.1.0 vs v0.2.0、B 多一个 env）各落各家
- 只改 A 的镜像后，B 的 Deployment **resourceVersion 不变**（RV 是客户端唯一能证明「没被写过」的信号），副本/镜像原样
- 删 B 的 Deployment → 由 B 自己重建（UID 变化），期间 A 的 RV 不变

> 一个诚实的边界：这条用例证明的是「在当前实现下没有共享状态」，
> 不是「证明不存在任何并发缺陷」——`-race` 跑的是测试进程，`MaxConcurrentReconciles` 默认 1
> 意味着同一个 controller 的 Reconcile 本身是串行的，真正的并发压力要靠多副本 + leader election
> 才有意义（`docs/day08`）。

## 五、如果被问「Day 11–17 你做了什么」

一句话版：

> 那七天做的是 v1 里「让控制循环能被信任」的部分：自愈（删了重建、改歪拉回）、
> status 回写和打印列、事件与级联回收、envtest 真控制循环基建，以及三条 unittest（创建/更新/自愈）。
> 其中「删 CR 回收」在 envtest 里原理上测不到（没 GC 控制器），我改到真集群实测 2.75–3.09s；
> 「幂等零写入」的判据被我自己推翻了——`resourceVersion` 不变证明不了零请求，
> 得看 `rest_client_requests_total`，实测 16 次空转 0 写请求。
