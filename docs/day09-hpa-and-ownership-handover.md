# Day 9 — HPA：把 replicas 的所有权让出去

> Day 5 讨论过两条原则，今天在代码上落地：
> ① 「**字段所有权必须重新划分**」——多写者出现时（HPA），控制器要把不属于自己的字段让出去；
> ② 「**比较集合必须等于写入集合**」——让位必须**同时**从不写、不比、覆写前搬回三处下手。

## 一、交付清单

| 产出 | 位置 | 说明 |
|------|------|------|
| `autoscaling` 声明 + CEL 互斥 | `api/v1alpha1/shortlink_types.go` | `minReplicas`/`maxReplicas`/`targetCPUUtilizationPercentage`；与 `replicas` 互斥 |
| **replicas 所有权让位** | `internal/controller/shortlink_controller.go` | `ownsReplicas()`：不写、不比、覆写前搬回 |
| HPA 调和 | 同上 | `desiredHPA` / `reconcileHPA` / `hpaNeedsUpdate`；`Owns(HPA)` 纳入 watch |
| Ready 判据随所有权改变 | 同上 | `expectedReplicas()`：HPA 模式下用 Deployment 自己的 `spec.replicas` |
| RBAC | `config/rbac/role.yaml` | `autoscaling` 组 + **core/v1 events 补回**（见第五节） |
| 测试 | `internal/controller/shortlink_controller_test.go` | 14 specs（+3：HPA 创建/让位、自愈与按需删除、CEL 互斥） |
| 真集群演示 | 本文档第四节 | 1 → 4 → 5（上限）→ 缩回 1 |

## 二、核心：让位要**三处同时**动手

`replicas` 从"我拥有"变成"HPA 拥有"，必须同时改三处，缺一处都会出问题：

| 动作 | 代码 | 缺了会怎样 |
|------|------|-----------|
| **不写**（期望构造器里留空） | `desiredDeployment`：`if !ownsReplicas(cr) { dep.Spec.Replicas = nil }` | 创建 Deployment 时就写上 replicas，与 HPA 的初始值打架 |
| **不比**（比较集合里移除） | `deploymentNeedsUpdate`：`desired.Spec.Replicas != nil && ...` | HPA 写 5 → 我判"有偏移"→ 每轮写回我的值 → **来回打**（每次都是真写入 + 事件风暴） |
| **覆写前搬回** | `reconcileDeployment`：`if !ownsReplicas(cr) { actual.Spec.Replicas = currentReplicas }` | 整体覆盖会把 `spec.replicas` 抹成默认值 1 —— 这就是 Day 6 7.2 那个"静默抹掉"的同款问题，只不过这次受害者是 HPA |

**注意第三条**：它是"反向搬运"模式，和 `reconcileService` 搬 `clusterIP` **是同一个模式、不同的动机**——

| | 搬什么 | 为什么搬 |
|---|---|---|
| Service | `clusterIP`/`nodePort`… | 那些是**集群分配且不可变**的字段 |
| Deployment（今天） | `spec.replicas` | 那个字段的**所有权在 HPA**（不是"不可变"，是"不是我的"） |

**连带的一条**：`Ready` 的判据也得跟着换。HPA 模式下 CR 里的 `replicas` 已经不代表期望了，"就绪"应该对齐 **Deployment 自己的 `spec.replicas`**（HPA 写的值）：

```go
func expectedReplicas(cr, st deploymentState) int32 {
	if !ownsReplicas(cr) && st.specReplicas != nil {
		return *st.specReplicas        // ← HPA 写的，此刻的真实期望
	}
	return desiredReplicas(cr)
}
```

> **所有权变更会连带改变"什么算达成"**。同一个 `Ready` 条件，两种模式下算的是不同的东西——这类连带影响最容易漏。

## 三、CRD 层：CEL 互斥，以及**为什么必须移除 `replicas` 的默认值**

声明了 `autoscaling` 又声明 `replicas`，是矛盾的需求（"我固定 3 个" vs "让 HPA 伸缩"）。用 CEL 在**准入期**静态拒绝：

```go
// +kubebuilder:validation:XValidation:rule="!(has(self.autoscaling) && has(self.replicas))",
//   message="replicas 与 autoscaling 互斥：启用 HPA 后副本数由 HPA 管理，请移除 replicas"
```

**但这条规则在早期版本上是形同虚设的**：`replicas` 带着 `+kubebuilder:default=1`，而 API Server 的**默认值填充发生在校验之前** → `has(self.replicas)` **恒为真** → 规则会把**所有**带 autoscaling 的 CR 都拒掉。

所以本次**移除了 `replicas` 的 CRD 默认值**。这顺带揭穿了一个更早的设计裂缝：

> Day 4 特意用 `*int32` 来区分「未设置」与「显式 0」——**而 `+kubebuilder:default=1` 又把"未设置"填成了 1**，
> 指针想表达的信息在准入期就被抹掉了。两者本质上互相打架。
> 移除默认值后，nil 的含义恢复为"用户没写"，控制器在 `desiredReplicas` 里兜底成 1（行为不变）。

（实测：移除默认值后，CEL 互斥用例通过——`apierrors.IsInvalid` ✓。）

另外 `targetCPUUtilizationPercentage` 是**必填标量**（不是指针）：HPA 的 Utilization 指标类型必须有目标值，缺了会被 API Server 以 `must specify averageUtilization` 拒绝——与其让它在控制器运行时炸，不如让准入期拦住。

## 四、真集群演示（k3d + metrics-server）

前置修复：集群里 `metrics-server` 一直 `ImagePullBackOff`（拉不到镜像）→ **离线搬运**
（从 `sre-demo` 节点导出、`ctr images import` 进 `operator-demo` 节点，与 day07 的 pause 镜像同一手法）。
修好后 `kubectl top` 可用，HPA 才有真实指标。

CR 声明：`autoscaling{min 1, max 5, target 60%}` + `resources.requests.cpu: 50m`（**HPA 靠 requests 算利用率**）。

| 阶段 | 操作 | 实测 |
|------|------|------|
| 初始 | apply CR | HPA 创建（`Deployment/autoscale`，1–5，目标 60%）；Deployment `1/1`（replicas 由 API Server 默认 1，我们没声明） |
| 加压 | 在 Pod 里起 2 个忙循环 | `kubectl top` 显示 2001m/50m → HPA 报 **`cpu: 4000%/60%`** |
| 扩容 | — | HPA 1 → 4 → **5（上限）**，`5/5 available` |
| 撤压 | 删掉烧 CPU 的 Pod | 利用率掉到 `2%/60%`；**HPA 默认 5 分钟稳定窗口**过后缩回 **1** |

**"成功让位"的两个证据**（这比数字更重要）：

```bash
kubectl get events --field-selector reason=Scaled     # → No resources found
kubectl logs ... | grep -c "Updating Deployment"      # → 0
```

也就是说：整轮自动扩缩容期间，**控制器一次都没写过 Deployment**（`Scaled` 事件是我们改副本数时才发的，一次都没有；日志里也没有覆写记录）。而 `kubectl get sl autoscale` 全程 `READY=True`、`REPLICAS` 跟着 HPA 的真实值走。

## 五、两个实测踩坑（本节最值钱）

### 坑 1：新增一个 `Owns()` 必须同时补 RBAC —— 否则**整个控制器停摆**

加了 `Owns(&autoscalingv2.HorizontalPodAutoscaler{})` 后，控制器需要为该类型建立 informer（list/watch）。
我漏了 RBAC（`make manifests` 生成了 `role.yaml`，但集群里的 ClusterRole 没更新），结果是：

- informer 的 reflector 起不来（Forbidden）；
- **控制器完全没启动** → 不只是"新功能不工作"，而是**所有调和都停了**（连 `kubectl get sl` 的 status 都是空的）。

教训：`Owns()` / `Watches()` 是"给控制器加一条输入"，加输入的代价是**权限**。RBAC 与 watch 必须同一次部署到位；
排障时"什么都没有发生"的第一反应应该是**看控制器有没有真的起来**（这里日志里的 reflector 堆栈是关键线索，而 `0/1 Ready` 的 manager Pod 也早就在提示）。

### 坑 2：事件到底落在哪个 API 组？（观察不一致，已按稳妥方式处理）

迁移到 `mgr.GetEventRecorder`（新事件 API）后：

| 环境 | 观察 |
|------|------|
| envtest | 测试读 `events.k8s.io/v1` 的 `EventList`，能查到我们的事件（断言通过） |
| 真集群 | `kubectl get events`（默认视图）能看到；**`events.v1.events.k8s.io` 里查不到** |

即"记录器最终写进哪个 API 组"受 controller-runtime 内部装配影响。我原本把 RBAC 从 `""/events` 换成了 `events.k8s.io/events`，
**这一换有真实风险**：若实际写的是 core/v1，事件会被 API Server 以 Forbidden 静默拒绝——**事件丢失是最难发现的一类故障**（它不影响功能，只影响可观测性）。

处理：**两组都授权**。并把"我不知道到底写哪个"这件事写进注释，而不是猜一个。

## 六、遗留

| # | 事项 |
|---|------|
| 1 | 一页纸里补 HPA 这一节的取舍（已有一页纸，待增补） |
| 2 | `hpaNeedsUpdate` 只比较 CPU 指标，但整体覆盖会抹掉别的指标——与 Deployment 同款的已知取舍 |
| 3 | HPA 的 `behavior`（缩容稳定窗口/速率策略）未暴露到 CRD |
| 4 | envtest 测不到 HPA 真实扩缩容（HPA 控制器在 kcm 里），只能测对象编排与让位；真效果靠本日的 k3d 演示 |
