# Day 6（切片 1）— 失败可见化 + 自愈固化

> 承接 `docs/day05` 的 9.1 待办清单第 1、2 项。Day 5 的控制循环已经能收敛，但**不会说话**：
> 调和失败只 `return err`，status 一个字都不写，外部看到的是上一轮留下的 `Ready=True`。
> 本次把它变成「失败可被外部观测」，并把「删了自动重建」这条已具备的能力固化成测试。
>
> 滚动策略字段 + readinessProbe 挪到 Day 7（与真集群演示一起做，probe 只有在真集群里才看得出效果）。

## 一、交付清单

| 产出 | 位置 | 说明 |
|------|------|------|
| 失败可见化 | `internal/controller/shortlink_controller.go` | `reportFailure`：Warning 事件 + `Ready=False`，**不推进** observedGeneration |
| status 写出口统一 | 同上 | `patchStatus` 成为唯一写出口（Diff 后才写、`MergeFrom`） |
| Ready 条件分档 | 同上 + `api/v1alpha1/shortlink_types.go` | Reason 常量集中定义，拆出 4 种未就绪原因 |
| 事件记录 | 同上 | `Recorder` 字段（`SetupWithManager` 兜底注入）+ `events` RBAC（create/patch） |
| 自愈用例 | `internal/controller/shortlink_controller_test.go` | 删 Deployment → 断言**新 UID** + ownerReference + spec |
| 失败路径用例 | 同上 | 造一次真实调和失败 → 断言事件/条件/obsGen 不动 → 排障后自动恢复 |
| RBAC | `config/rbac/role.yaml` | `make manifests` 重新生成（events 入列） |

实测：`go test ./... -count=1` 全绿，controller 套件 **7 specs**（原 5 + 新 2）。

## 二、Day 5 留下的缺口

Day 5 的 `Reconcile` 在失败时是这样的：

```go
if err := r.reconcileDeployment(ctx, &cr); err != nil {
    log.Error(err, "Failed to reconcile Deployment", ...)
    return ctrl.Result{}, err          // ← status 一个字都没写
}
```

于是失败时外部看到的 status 是**上一轮的陈旧快照**：

```
metadata.generation       = 3    （用户改了 spec.image）
status.observedGeneration = 2    （控制器没追上）
status.conditions[Ready]  = True ← 陈旧结论，且没有任何东西指出"这一轮失败了"
```

日志里有错误，但 `kubectl get sl` / 监控 / 告警全都看不见——**这是"控制器在静默重试"的典型形态**。

## 三、设计：把 status 分成「可观测量」和「结论」两类

| 分类 | 字段 | 更新时机 | 理由 |
|------|------|----------|------|
| **可观测量** | `readyReplicas` | 每次观测都刷新（成功/失败/冻结都刷） | 它来自集群的真实观测（`Deployment.status`），越新越准 |
| **派生值** | `url` | 每次调和都重算（成功/失败/冻结都刷） | 它由 **spec** 算出（`baseDomain`/`port`），不是观测来的；重算无害。注意它跟随的是**意图**而不是现状——失败/冻结时它指向的是"将来可达"的地址（边界的讨论见第七节遗留 #6） |
| **结论** | `observedGeneration`、`conditions[Ready]` | **只有成功走完一轮**才更新 | 它们是"对最新 spec 的判断"，失败时推进就等于谎报收敛 |

这条划分同时解释了两处已有的设计：冻结（paused）时 `advanceGeneration=false`，以及现在的失败路径。

### 3.1 `reportFailure` 的两条刻意设计

1. **不推进 `status.observedGeneration`**——它代表「最新 spec 已被完整调和」。失败这一轮没走完，
   推进它客户端就会以为可以信任 `Ready`。保持不动，`obsGen < gen` 本身就是"未收敛"的旗帜。
2. **条件自身的 `ObservedGeneration` 仍记当前 generation**。两者语义不同，不能一起省略：
   顶层 obsGen 回答"最新 spec 处理完了吗"，条件里的 obsGen 回答"**这条 False 说的是哪一版 spec**"。
   这次失败确实是针对当前这版 spec 得出的结论，所以记当前值。

```go
func (r *ShortlinkReconciler) reportFailure(ctx, cr, reason string, cause error) error {
    r.Recorder.Event(cr, corev1.EventTypeWarning, reason, cause.Error())
    _, err := r.patchStatus(ctx, cr, func(s *...ShortlinkStatus) {
        s.ReadyReplicas = st.ready
        s.URL = statusURL(cr)
        meta.SetStatusCondition(&s.Conditions, metav1.Condition{
            Type: ConditionReady, Status: metav1.ConditionFalse,
            Reason: reason, Message: cause.Error(),
            ObservedGeneration: cr.Generation,   // 结论针对当前 spec
        })
    })
    if err != nil { log.Error(...) }         // status 写失败只记日志
    return cause                             // 返回原始 cause，让退避重试对准真正的故障
}
```

**返回 `cause` 而不是 status 写失败的错误**：workqueue 的退避重试应该由真正的故障驱动；
status 写失败只记日志，避免它掩盖原始错误。

### 3.2 Ready 条件分档：消灭"自相矛盾的 message"

Day 5 只有两种 Reason（`ReplicasReady` / `ReplicasNotReady`），于是"副本数已达标但 Deployment 不可用"
会输出一句自相矛盾的话：

```
Reason: ReplicasNotReady   Message: expected 1 replicas, 1 reported ready   ← 读者会懵
```

现在四种未就绪原因各有 Reason（常量集中在 `api/v1alpha1/shortlink_types.go`）：

| 场景 | Reason | Message 示例 |
|------|--------|--------------|
| 子资源还不存在（刚建、缓存未同步） | `DeploymentNotFound` | `Deployment sl-x not found yet` |
| 就绪数 != 期望 | `ReplicasNotReady` | `0/1 replicas ready` |
| 就绪数达标但 Deployment 不可用 | `DeploymentNotAvailable` | `1/1 replicas ready, but Deployment is not available yet` |
| 调和失败（Create/Update 被拒等） | `DeploymentReconcileFailed` / `ServiceReconcileFailed` | `Deployment.apps "x" is invalid: ...` |
| 全部达标 | `ReplicasReady` | `1/1 replicas ready` |

**Reason 是"机器可分类、可告警"的那一半信息**：字符串散落各处时，监控规则和测试断言会随重构悄悄失配，
所以集中定义为常量。为区分 `!found` 与 `!available`，观测函数改为返回结构体（`deploymentState`），
而不是 Day 5 的 `(int32, bool)` 二元组。

## 四、事件：动作 vs 状态

本次共用 4 类事件：`Normal/Created`、`Normal/Updated`、`Normal/Scaled`（副本数变化时单独记一条，
排障时"谁在什么时候缩了容"一眼可见）、`Warning/<条件 Reason>`。

三个容易踩的点：

1. **`mgr.GetEventRecorderFor` 写的是 legacy `core/v1` Event**（在新版 controller-runtime 里已被标记
   deprecated），所以 RBAC 是 `groups=""`；若改用非 deprecated 的 `mgr.GetEventRecorder`，写的是
   `events.k8s.io/v1`，RBAC 也要跟着换组。读源码确认：`GetEventRecorderFor` → `deprecatedRecorder`
   → `deprecatedBroadcaster.NewRecorder(scheme, corev1.EventSource{...})`。
2. **事件是异步写出的**（broadcaster + 相关性聚合），测试里必须 `Eventually` 等它落地，不能立刻断言。
3. **事件有 TTL（默认 1 小时），条件才长期可读**。所以二者不能互相替代：事件回答"发生过什么"，
   condition 回答"现在怎么样"。排障顺序通常是"先看 condition 定位状态，再看事件还原时间线"。

## 五、两条新用例

### 5.1 自愈（把已有能力固化成测试）

操作：建 CR → 等 Deployment 出现并记下 UID → 删掉它 → 等"重新出现且 **UID 不同**"。

断言里每一步都不是凑数：

| 断言 | 防的是什么 |
|------|------------|
| **新对象 UID ≠ 旧 UID** | 只断言"对象还在"是无效证据——可能删除根本没生效。UID 变了才是"被重建" |
| ownerReference 仍指向本 CR 且 `controller=true` | 证明是**控制循环**建的，不是别人碰巧建了个同名的 |
| selector / 镜像 / template labels 与期望一致 | 证明重建走的是期望构造器，而不是残留旧对象 |

机制说明：删除 Deployment 产生 Delete 事件 → `Owns(Deployment)` 用**被删对象上的
ownerReference** 反查到 CR → 入队 → `reconcileDeployment` 先查后建 → 重建。**全程无轮询。**

### 5.2 失败路径：怎么在 envtest 里造一次"可控的真实失败"

这是本次最有意思的测试设计问题：**CRD 校验会拦住非法 spec**，所以"提交非法 spec"造不出失败；
envtest 里也没有任何外部依赖可以拔掉。可行路径是**预置一个导致调和必然失败的既有资源**：

```go
occupied := desiredDeploymentForTest(name)
occupied.Spec.Selector.MatchLabels        = {instance: "someone-else"}   // 不可变字段
occupied.Spec.Template.ObjectMeta.Labels  = {instance: "someone-else"}   // 被比较的字段
Create(occupied)
Create(CR)
```

为什么必须两个都改（第一版测试就栽在这里）：

- 只改 selector → **占位对象自己都建不出来**：API Server 要求 `selector` 必须匹配 template labels；
- 而且 `deploymentNeedsUpdate` **根本不比较 selector**，只靠 selector 偏移控制器会认为"无偏移"，
  于是根本不发 Update、也就没有失败；
- 两个都改：template labels 的偏移触发覆写 → 覆写会把 selector 一起提交 → API Server 以
  「selector 不可变」拒绝 → 控制器进入失败分支。

这条用例断言四件事：`Ready=False` 且 Reason 是 `DeploymentReconcileFailed`；`observedGeneration`
**Consistently 停在 0**（不是瞬时值）；`Warning/DeploymentReconcileFailed` 事件出现；
把占位对象删掉后**自动恢复**（obsGen 追平、Reason 换成真正的未就绪原因 `ReplicasNotReady`
——envtest 没有 kcm，副本永远不会 ready）。

顺带一个**新发现**（见第七节）：这条用例之所以需要"两个都改"，本身就暴露了"比较集合 != 写入集合"。

## 六、实测结果

```
go test ./... -count=1
ok  github.com/Xiaoyu-hub/shortlink-operator/internal/controller   35.4s   （7 specs）
ok  .../warmup  ok  .../warmup/cmd
```

## 七、一条设计原则：`Ready` 回答"能不能用"，不回答"是否逐字满足"

selector 漂移这个案例引出的原则，值得单独记一节（已写进 `api/v1alpha1/shortlink_types.go`
的 `ConditionReady` 注释）：

- **`Ready` 是给消费者看的契约**：`kubectl wait --for=condition=Ready`、告警规则、SLO 口径、
  上游 operator（"等 Shortlink Ready 再挂 Ingress"）都问同一件事——**现在能不能用它**。
- **把"配置漂移"混进"可用性"会毁掉这个信号**：
  - `kubectl wait` / 发布流水线被永久阻塞；
  - 监控把"可用"报成"不可用" → 误报淹没真故障（告警疲劳）；
  - 上游依赖者基于错误的可用性做决策。
- **什么才该让 `Ready=False`**：期望的服务能力没达成——副本没起来、Deployment 不可用、
  子资源根本没建出来。都是"消费者此刻不能用"。
- **"spec 没被逐字满足"放哪儿**：独立条件（如 `SpecDiverged`）+ Warning 事件。
  消费者读 `Ready` 决定能不能用，运维读 `SpecDiverged` 决定要不要人工介入，两者不互相污染
  ——这正是 Day 4 选 `metav1.Condition` 而非单个 Phase 字符串的收益在这里兑现。

通用形式（面试可直接说）：**条件的语义应由"观察者的问题"定义，而不是由"控制器的实现细节"定义。**

### 7.1 实测：selector 漂移的两段式行为（`go test`，用例见 `selector-drift`）

补一条用例把上面的分析钉成实测（8 specs 全绿）：

| 阶段 | 操作 | 实测结果 |
|------|------|----------|
| ① | 预置同名 Deployment，selector 被**改窄**（仍匹配 template labels，因此对象合法） | 控制器**零写入、不报错**、`observedGeneration` 照常追平；selector 保持被改窄的样子（**静默且持久**） |
| ② | 再改 `spec.image`（**被比较**的字段） | 覆写被触发 → selector 随整体覆盖一起提交 → API Server 以「不可变」拒绝 → `Ready=False/DeploymentReconcileFailed`（**响亮失败**） |

⇒ **同一处漂移，命运取决于"有没有别的字段先动"**：没有 → 静默；有 → 响亮。这就是"比较集合 != 写入集合"的可观测后果。

## 八、遗留与发现

| # | 事项 | 说明 |
|---|------|------|
| 1 | **selector 在写入集合里、不在比较集合里** | `deploymentNeedsUpdate` 不比较 `Spec.Selector`，但覆写分支会写它。这是 day02"裂缝 1"的具体实例。**两种后果都存在，取决于外来的 selector 改动是否仍匹配 template labels**：<br>① 改了 `instance` 值、或加了 template labels 里不存在的键 → API Server 直接拒绝（selector 必须匹配 template labels），或必须连带改 template labels → 而 template labels **在**比较集合里 → 控制器发现偏移 → 覆写 → 被 selector 不可变拒绝 → **响亮失败**（5.2 用例的场景）；<br>② 把 selector 收窄、或加一个 template labels 里**已有**的键 → 对象合法、template labels 未变、控制器不比较 selector → **静默且持久**的偏离，且 selector 不可变，永远修不回来。<br>**注意修法不是"把 selector 加进比较集合"**——那会把①的"一次性报错"变成**每次调和都报错**（无限退避 + Ready 常驻 False），把局部瑕疵升级成对象级故障。正确方向是**报告而非修复**：检测到不可变字段与期望不符时，只写 Warning 事件 + 专门 Reason，不尝试写回（两段式行为的实测见 7.1） |
| 2 | 失败不重试的分类 | 目前所有 err 都退避重试；终态错误（如 5.2 的不可变冲突）会无限重试。Day 7+ 可加"终态错误"识别 |
| 3 | 滚动策略 + readinessProbe | 挪到 Day 7（probe 需真集群才看得出效果）；注意 `needsUpdate` 的比较集合要同步扩 |
| 4 | 零写入测试 | 待做：计数 `RoundTripper`（见 day05 第十节 10.4） |
| 5 | 接管守卫 | 待做：`metav1.IsControlledBy`（5.2 的场景在加了守卫后会变成另一种失败原因） |
| 6 | `url` 的分类边界 | 现在按"派生值"处理、每次调和都刷新。若把 `url` 当成对用户的**承诺**（"服务现在在这里"），它就该被 `observedGeneration` 门控、只在成功时更新；代价是失败期间 status 不再新鲜。当前选择"刷新 + 靠 obsGen/Ready 表达未收敛"，需要时再改 |
