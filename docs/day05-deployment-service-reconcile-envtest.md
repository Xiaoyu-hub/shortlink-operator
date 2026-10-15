# Day 5 — Deployment/Service 调和 + envtest 真控制循环

> 本文档随 feat(day5) 提交一起推送。Day 4 面试题 4 问已判分通过（3 处修正，见聊天记录）。
> 本日把 ROADMAP Day 6–8/12–14 的前半段压缩成一次交付：Deployment/Service 调和、status 写回、
> envtest 真控制循环测试。HPA、滚动策略字段、事件、自愈专项用例留在后续 Day 补齐。

## 一、交付清单

| 产出 | 位置 | 说明 |
|------|------|------|
| 控制器实现 | `internal/controller/shortlink_controller.go` | 调和 Deployment/Service + status 写回，全量重写 |
| envtest 测试 | `internal/controller/shortlink_controller_test.go` | 5 条用例（创建/缩容零/status/滚动/冻结） |
| 测试基建 | `internal/controller/suite_test.go` | 起真实 manager（控制循环跑起来），不再手调 Reconcile |
| RBAC | `config/rbac/role.yaml` | `make manifests` 重新生成（deployments/services 入列） |
| envtest 二进制 | `bin/k8s/1.37.0-windows-amd64/` | 本机手动下载（原因见第七节） |

## 二、控制循环落地：三条路径

每次 Reconcile 都从「读最新 Shortlink」开始，然后分三路收敛：

```
Reconcile(req)
  ├─ Get(Shortlink) 失败 → NotFound：无事可做（子资源靠 ownerReference 级联回收）
  ├─ Spec.Paused == true → 冻结：跳过子资源，只刷新 status 可观测量
  └─ 正常：
       ├─ reconcileDeployment：不存在→Create；偏移→Update；一致→零写入
       ├─ reconcileService  ：同上（额外保留 ClusterIP/NodePort 等 API Server 分配值）
       └─ updateStatus：readyReplicas / URL / Ready 条件 / observedGeneration
```

**三条防呆设计**：

1. **先查后建**：Create 前先 Get；创建撞 `AlreadyExists` 视为「并发窗口内别的 reconcile 先建了」，
   事件驱动的 Owns watch 会再触发一次 diff，这次只走收敛分支。绝不允许「查不到就直接建」变成双份资源。
2. **幂等判据**：期望构造器每次从 CR 重建（desired），与实际对象按「本 operator 管理的字段」比对；
   无偏移 → 一次 API 都不写。连续 reconcile 不改变任何 resourceVersion。
   （这条判据的有效性已被实测削弱，见第十节 10.4：它测不出「判据过宽」导致的冗余写请求。）
3. **错误即退避**：任何子资源操作失败都会带着 err 返回，由 workqueue 的指数退避按 key 重试，
   不会吞错也不会死循环。

## 三、幂等核心：为什么不能整体 DeepEqual

新手最常见的 operator 死循环：`reflect.DeepEqual(actual, desired)` 恒为 false，控制器对「其实一致」
的对象无限 Update。因为对象经过 API Server 准入后被补了大量**默认值**与**分配值**：

| 字段 | 谁补的 | 整体比较的后果 |
|------|--------|----------------|
| `ImagePullPolicy`、`TerminationMessagePath`、`Protocol=TCP` | API Server defaulting | desired 为空 vs actual 有值 → 恒不等 |
| Service `ClusterIP` / `ClusterIPs` / `IPFamily` | API Server 分配 | desired 为空 → 恒不等（**注意：Update 并不会把分配值冲掉**，见第十节实测） |
| Service/NodePort 的 `nodePort` | API Server 分配 | 同上 |

正确姿势（本实现）：**只比较自己声明并负责收敛的字段子集**——Deployment 比
labels/replicas/template labels/容器 Name·Image·Env·Resources·Ports；Service 比
labels/Type/Selector/Ports(Name·Port·TargetPort·Protocol)。更新时把 ClusterIP/NodePort 先从
actual 搬回 desired 再整体覆盖，显式声明「这几个值归 API Server 所有、本控制器不持有」。

## 四、status 写回与因果链

- **Ready 条件** = Deployment 存在 && `Available=True` && `readyReplicas == 期望副本数`；
  each 条件自带 `ObservedGeneration`。envtest 里 0 副本时 0==0 且 Available=True → "0/0 replicas ready"，
  语义是「期望状态已达成」（缩容到零也是一种达成）。
- **observedGeneration 只在正常路径推进**。冻结（paused）时刻意不推进——这条链代表
  「最新 spec 已被调和」，冻结状态下我们**没有**调和，所以保持冻结前的版本。客户端见
  `obsGen < metadata.generation` 就知道资源未收敛，不会误信旧的 Ready=True（Day 4 因果链的落地）。
- **无变化不写**：先 DeepEqual(old.Status, new.Status)，一致直接 return，杜绝 status 写放大与自触发。
- **用 `Status().Patch(MergeFrom(old))` 而非 Update**：并发下别的副本改了 status 时 merge patch
  不要求 resourceVersion 一致，比 Update 撞 409 更宽容（409 仍会发生，属于预期分支，重试即可）。

## 五、watch 拓扑与自循环防疫

```
For(Shortlink)   -- GenerationChangedPredicate：只放行 spec 变化（generation 递增）
Owns(Deployment) -- 子资源任何变化（含 status.readyReplicas 走动）反查 owner 入队
Owns(Service)    -- 同上
```

两件事的因果：

1. **我们写 status 为什么不形成无限循环？** 写 status 会触发 Shortlink 自己的 watch 事件，
   但 GenerationChangedPredicate 把 status-only 更新滤掉了（generation 没变）。真正的「再调和」
   来自 Deployment/Service 的事件——比如 Deployment 就绪数变化，那正是我们想响应的信号。
2. **为什么不需要轮询/RequeueAfter？** Deployment 的 status 变化（Pod 就绪/异常/滚动进度）都会经
   Owns 入队；这是**事件驱动**。轮询是备胎，不是主引擎。

## 六、ownerReference 与 GC

- 创建子资源前 `controllerutil.SetControllerReference(cr, child, scheme)`：Deployment/Service 挂上
  `ownerReferences`（controller=true）。删 Shortlink 时由 kube-controller-manager 的 GC 控制器级联删除。
- **envtest 没有 GC 控制器**，所以删除路径在本机测试里测不到（子资源不会自动消失）——这条留到
  Day 19 真集群联调验证。测试改用「重复同名 Create 只返回 AlreadyExists」夯实「不产生重复资源」。
- 注意：测试辅助构造器里 `desiredDeploymentForTest` 直接调生产代码 `desiredDeployment()`，
  好处是测试的不是「抄一份期望」，而是与控制器完全同一份构造逻辑。

## 七、envtest：原理与 Windows 实测

envtest = controller-runtime 内置测试环境，**etcd + kube-apiserver 都是真实二进制**：
CRD 安装、默认值、校验、subresource、Table API 全是真的。测试里 manager + 控制器真跑，
所以「创建 CR → Deployment 自动出现」是 watch 驱动的真实收敛，不是手调函数。

**envtest 缺什么**：kube-controller-manager / scheduler / GC 都不在。后果：

- Deployment 永远不会自己长出 Pod → `status.readyReplicas` 恒 0；
- 测试必须**手动伪造** Deployment 状态（见 ready-status 用例）来打通 status 链路；
- 伪造时的新发现：Deployment status 有跨字段校验 `readyReplicas ≤ status.replicas`，
  必须先设 `Status.Replicas` 再设 `ReadyReplicas`，否则 422。

**Windows 上跑 envtest 的三个实测坑**：

1. `make setup-envtest` 下载索引走 `raw.githubusercontent.com`（被 DNS 劫持到 127.0.0.1）→ 502。
   绕过：`gh release download envtest-v1.37.0 --repo kubernetes-sigs/controller-tools --pattern "*windows-amd64.tar.gz"`
   直拉 GitHub release asset，解压到 `bin/k8s/1.37.0-windows-amd64/`（suite_test.go 的
   `getFirstFoundEnvTestBinaryDir` 自动探测该目录，无需设 KUBEBUILDER_ASSETS）。
2. Makefile 的 `ENVTEST_VERSION` 用 `go list -m` 从 go.mod 推导，但 controller-runtime 被 replace 到本地
   补丁目录（无版本号）→ 推导失败。要跑 `make setup-envtest` 需手传 `ENVTEST_VERSION=v0.25.0`；
   本机直接 `go test ./internal/controller/...` 即可（二进制已就位）。
3. Ginkgo 明确禁止 `go test -count>1`（会直接报配置错误）；验证稳定性用连续 `-count=1` 多次。
4. 测试里直接构造 CR 不经过 admission defaulting，`spec.port` 是 0 → 期望 Deployment 校验失败
   （`containerPort: Required value`）。测试构造器显式补 `Port: 8080`，等价于真实 CR 的默认值。

## 八、测试结果（本机实测）

```
go test ./... -count=1  → 全绿（controller 16.4s / warmup 0.1s / warmup/cmd 5.4s）
go test ./internal/controller/... -count=1 连续 3 次 → 11.7s / 11.4s / 11.0s 全绿
```

Ginkgo 输出摘录：

```
Ran 5 of 5 Specs in 10.815 seconds
SUCCESS! -- 5 Passed | 0 Failed | 0 Pending | 0 Skipped
```

5 条用例覆盖：

| 用例 | 断言重点 |
|------|----------|
| 创建 CR → Deployment+Service 自动出现 | 标签/ownerRef/端口/env/selector；同名重复 Create 只 AlreadyExists |
| replicas=0 | Deployment 缩容到 0（资源保留、Pod 清零）——Day 4 指针语义的落地 |
| 伪造 Deployment 就绪 | status 回写 readyReplicas/observedGeneration/Ready=True/集群内 URL |
| 改 spec.image | Deployment 镜像被推进；observedGeneration 追平 metadata.generation |
| paused 冻结 ↔ 解冻 | 冻结期子资源不动、obsGen 停在冻结前；解冻后一次性补齐差额 |

## 九、下一步

- Day 5 的问答与复盘已并入 `docs/day05`（见第十节勘误）与下面的 9.1；Day 6 按 9.1 执行。
- `make test` 在本机仍会卡在 setup-envtest（DNS 劫持），已在第二节备注绕过路径；CI（Linux）无此问题。

### 9.1 Day 6 待办清单（Day 5 复盘产出，按依赖排序）

| # | 事项 | 来源 / 为什么现在做 |
|---|------|---------------------|
| 1 | **自愈专项用例**：删 Deployment → 自动重建 | 机制已具备（`Owns` + 先查后建），缺的是把它**固化成测试**。断言必须有「新对象 UID ≠ 旧 UID」，并同时验证 ownerReference 与 spec 一致 |
| 2 | **失败 condition + 事件记录** | 现在子资源调和失败只 `return err`，status 一个字不写 → 外部只看到 `Ready=True`（陈旧值）。要补：`Ready=False` + 区分 `ReplicasNotReady` / `DeploymentNotAvailable`、message 带 err；加 `EventRecorder` 与 `events` RBAC |
| 3 | **滚动策略 + readinessProbe** | `spec.strategy.maxUnavailable/maxSurge` 进 CRD；probe 进 pod template —— **有 probe 才谈得上"滚动零中断"**。注意 `deploymentNeedsUpdate` 的比较集合要同步扩：**比较集合必须等于写入集合** |
| 4 | **零写入测试** | 见 10.4：`resourceVersion` 测不出多余写请求，需包一层计数 `RoundTripper` 数请求 |
| 5 | **接管守卫** | `r.Get` 到同名子资源后先 `metav1.IsControlledBy(actual, cr)`，不是自己的就拒绝改写并报事件（防静默接管） |
| 6 | **（若引入）finalizer 走单轮式** | 见 10.5/finalizer 实测：先落地 finalizer 再动外部世界，且不依赖"加完 finalizer 会再来一轮" |
| 7 | **`paused` 语义加测试锁定** | 语义已在勘误中定案为「冻结 = 完全不干预」，需要一条测试把它钉住，防止后人改回"缩到 0" |

## 十、勘误（2026-10-15 用真 kube-apiserver 实测修正）

第三节原先断言「Update 会把 IP 冲掉」——那是**推理，不是实测**。实测**不成立**，本文档按实测改写。

### 10.1 默认值 vs 分配值：先把字段清单拉出来

实测环境：envtest 内的真 kube-apiserver（`--service-cluster-ip-range=10.0.0.0/24`）。
提交一个最小 Deployment（容器只有 `name`/`image`/`resources`）与最小 Service（只有 `selector` + 一个
`port`），再 Get 回来对比：

| 对象 | 我提交的 | API Server 存下来多出的字段 |
|------|----------|------------------------------|
| Deployment | 容器 3 字段 | 容器多 `imagePullPolicy`、`terminationMessagePath`、`terminationMessagePolicy`；Pod 层多 `dnsPolicy`、`restartPolicy`、`schedulerName`、`securityContext`、`terminationGracePeriodSeconds`；spec 层多 `replicas: 1`、`revisionHistoryLimit: 10`、`progressDeadlineSeconds: 600`、`strategy{type: RollingUpdate, maxSurge/maxUnavailable: 25%}` |
| Service | spec 2 字段、port 2 字段 | `type: ClusterIP`、`protocol: TCP`、`sessionAffinity: None`、`internalTrafficPolicy: Cluster`、`ipFamilies: [IPv4]`、`ipFamilyPolicy: SingleStack`、`clusterIP: 10.0.0.x`、`clusterIPs: [10.0.0.x]` |

**分类判据**（比记「默认值 / 分配值」两个名词更管用）：

- **默认值**：只依赖该对象自身，任何客户端都能算出同一结果 → **可被覆盖**（desired 里显式写
  `protocol: TCP` 完全合法）。
- **分配值**：依赖**整个集群的全局状态**（唯一性由集群统一发放）→ 由 API Server 分配、全局唯一：
  `clusterIP`/`clusterIPs`/`ipFamilies`、NodePort 的 `nodePort`。

### 10.2 分配值的真实危险：不是「被冲掉」，是「被搬到别的对象上」

| 实验 | 操作 | 实测结果 |
|------|------|----------|
| a | Update 时把 `clusterIP` 清空（`""` + `clusterIPs: nil`） | **成功，旧值被保留**（`10.0.0.73` 原样）；即本实现那三行搬运即便删掉也不会出事 |
| a-2 | Update 时把 `clusterIP` 改成另一个具体值 | **拒绝**：`spec.clusterIPs[0]: Invalid value: [...]: may not change once set` |
| b | NodePort 服务把 `nodePort` 清成 0 再 Update | **成功，旧值被保留**（31888 不变）；对照：全新创建的服务拿到的是另一个值（32291），说明分配器不保证给同值 |
| c | 把已有 Service `DeepCopy` 改名后 Create | **拒绝**：`spec.clusterIPs: Invalid value: [...]: provided IP is already allocated` |

结论改写：

1. **空值 = 修复语义**（API Server 保住旧值）；**显式改值 = 不可变拒绝**。两者是完全不同的路径。
2. 分配值真正会咬人的场景是**对象复用**：把已经被 API Server 加工过（因而携带分配值）的对象
   再拿去 Create、改名 Create、或跨 reconcile 缓存复用 → 撞 `may not change once set` /
   `already allocated`，且**失败原因不易一眼看出**。
3. 因此 `desiredService(cr)`「每次从 CR 重建新对象」不只是纯函数洁癖，而是**正确性的必要条件**：
   新建对象天然不携带上一次的分配值。反过来，`reconcileService` 里搬运 ClusterIP 的三行属于
   **冗余但显式**（声明「这几个值归 API Server，本控制器不持有」），**不是防事故的必需品**。

### 10.3 冻结语义勘误（paused）

`desiredReplicas` 原有 `if cr.Spec.Paused { return 0 }`，注释写「paused=true 时缩到 0」。这与实际行为
矛盾，且是**死代码**：`Reconcile` 在 `Paused` 时提前 return
（`internal/controller/shortlink_controller.go:94`），`reconcileDeployment` → `desiredDeployment`
根本不会被调用；`updateStatus` 里算出的 `desired` 只在 `advanceGeneration == true` 时使用，而 paused
路径传的是 `false`。三条调用路径都不经过它。

实际语义 = **冻结即完全不干预**（与 k8s `Deployment.spec.paused`「停止推进、维持现状」一致）：
副本保持冻结前的值，**不缩容**。已删除该死分支并改写注释。

### 10.4 幂等的可观测判据要改写（「resourceVersion 不变」证明不了什么）

第二节写了「连续 reconcile 不改变任何 resourceVersion」，ROADMAP Day 8 的自检也写着「连续触发 10 次
reconcile，resourceVersion 不变」。实测发现这个判据**几乎测不出错误**：

| 实验 | 操作 | resourceVersion |
|------|------|-----------------|
| 4a | Update，内容与已存对象一字不差 | **不变** |
| 4b | 空 merge patch | **不变** |
| 5 | 模拟「把 nodePort 纳入判据」的控制器：连续 3 轮都想把 `nodePort` 写成 0（API Server 每轮都把旧值保留回来） | **3 轮全不变** |
| 6（对照） | 真的加了一个 label | **变**（221 → 225，确认探针本身没问题） |

结论：**当一次写请求的净效果与已存对象一致时，API Server 不落盘、不 bump resourceVersion、也不产生
watch 事件**（修复语义发生在比对之前）。于是：

1. 一个「判据恒 true、每次调和都发 Update」的坏控制器，只要它的期望与现实的差异**全部落在 API Server
   会自己修复/补全的字段上**，就**不会**产生存储写放大，也**不会**自触发成热循环——但每次调和都白白多一次
   API 往返，而且日志每轮都喊「Updated Deployment」，**排障时会骗人**。
2. 真正的写循环需要**持久化的真实差异**：mutating webhook 持续注入、另一个写者持续改同一字段（HPA 改
   `replicas`、你改回去）、或者你写了非幂等的值（时间戳、无去重的 append）。此时写会落地 → 产生事件 →
   再调和 → 再写。
3. 因此「零写入」这条幂等主张**不能靠 resourceVersion 验证**，必须**数 API 请求**（包一层计数
   RoundTripper，或看 `rest_client_requests_total`）。这是 Day 6 测试的改进项。
4. 附带结论：读到 RV 从来没变，**不能**推断「我的 needsUpdate 判据是对的」。

> 本节机制层面的定位（no-op 写在 apiserver 的哪一层被 dedupe 掉）未做源码级确认，只做了行为实测；
> 实测版本为 envtest 内的 k8s 1.37，属版本相关行为，跨版本需复验。
>
> 本次实测用的探针脚本在 `hack/defaulting-probe/`，另有 `hack/finalizer-probe/`（见 10.5）。
> 两者都是对着真 kube-apiserver 压实验的小工具，已入库；`go run ./hack/defaulting-probe` 可复现下面的全部结论。

### 10.5 勘误：`deletionTimestamp` 会 bump generation（finalizer 不会被 predicate 卡死）

常见说法（我也一度这么讲）：CRD 的 `metadata.generation` 只在 spec 变化时递增，所以
`For(&X{}, builder.WithPredicates(predicate.GenerationChangedPredicate{}))` 会**滤掉"删除开始"的事件**，
导致 finalizer 永远等不到清理机会、对象卡在 Terminating。

用真 CRD + 真 predicate 实测（`go run ./hack/finalizer-probe`，复跑两次一致）：

| 步骤 | `metadata.generation` | predicate 放行？ |
|------|----------------------|------------------|
| 1. 创建 CR | 1 | — |
| 2. 改 `spec.port` | 1 → 2 | ✅ 放行 |
| 3. **只加 finalizer**（纯 metadata 变化） | 2 → **2** | ❌ **被过滤** |
| 4. **delete（finalizer 在 → 设置 `deletionTimestamp`）** | 2 → **3** | ✅ **放行** |
| 5. 移除 finalizer | — | 对象随即消失 |

结论：

1. **"删除开始"会 bump generation**（至少在本实测版本 k8s 1.37），所以**不会**卡死。原说法与版本相关，
   我未对更早版本做实测——**工程上不要依赖它**：要跨版本稳妥，就在 predicate 上显式覆写 `Update`
   放行 `deletionTimestamp` 变化（或对 finalizer 场景改用 `ResourceVersionChangedPredicate`）。
2. **"加 finalizer"那一次更新确实被过滤**（第 3 步）→ 因此**不要写跨轮次依赖 finalizer 的控制器**：
   `轮 1 只加 finalizer 然后 return、轮 2 再干活` 这种结构，轮 2 不会被这次更新唤醒。
   正确写法是**单轮式**：同一轮里判定 finalizer 不存在 → 加上 → 不 return，继续把活干完。
3. **顺序不变量**：finalizer 是「对象被删时一定有人来打扫」的凭证，所以**必须先让 finalizer 落地，
   再去创建/修改外部资源**。反过来做（先建外部资源、进程崩溃、finalizer 没写上），会留下无人回收的孤儿资源。