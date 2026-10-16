# shortlink-operator · 一页纸

> 用途：简历附件、面试开场、以及"能讲的状态"的自我校准。
> 原则：**只写能当场演示或当场复现的东西**；做不到的部分放进"已知边界"，不装作有。

## 一句话

用一个 Kubernetes Operator，把短链服务的「声明式期望状态 + 故障自愈」固化成平台能力：
用户写一个 `Shortlink` CR 声明期望，控制器持续把它管理的 Deployment / Service 收敛到该期望，
并把**真实就绪状态**与**是否已收敛到最新声明**暴露给上层。

## 为什么做（三项目叙事）

| 项目 | 位置 | 解决的问题 |
|------|------|-----------|
| sre-from-zero | 消费侧 | 服务不可用、回滚靠人 |
| burngate | 决策侧 | "什么时候该动"变成规则 |
| **shortlink-operator（本项目）** | **生产侧** | **"怎么动"固化成控制循环：人只声明期望，控制器持续收敛** |

## 架构（一屏）

```
kubectl apply ──► Shortlink(CR)  spec: image / replicas / port / serviceType / env / maxSurge…
                        │ （期望状态，只有用户能写）
                        ▼
              ShortlinkReconciler（控制循环）
              ├─ desiredDeployment(cr)   纯函数：只读 CR，产出内存对象
              ├─ Get → 只比"本 operator 管辖的字段" → 有偏移才 Create/Update
              ├─ 同样处理 Service（额外把集群分配的 ClusterIP/NodePort 搬回去）
              └─ status 写回：readyReplicas / Ready 条件 / observedGeneration
                        │
                        ▼
          Deployment / Service（真实对象）
                        │ kube-controller-manager / kubelet 报回真实状态
                        └─► Owns(子资源) 事件反查 ownerReference → 重新入队（事件驱动，无轮询）
```

## 五个关键设计决策（每个都带取舍）

1. **spec/status 分离 + `metav1.Condition` + `observedGeneration`**，而不是自定义 `Phase` 字符串。
   `observedGeneration < generation` 就是"声明了但还没达成"的机器可读旗帜。
   取舍：条件比 Phase 难写，但能同时表达"多个维度"且可被 `kubectl wait` / 告警规则直接消费。

2. **幂等判据 = 只比"我管辖的字段"**（不能整体 `DeepEqual`）。
   API Server 会在准入时补 defaulting（`imagePullPolicy`…）与 allocation（`clusterIP`/`nodePort`），
   整体比较恒为不等 → 每轮都写（写放大 + 日志撒谎）。
   取舍：手写 diff 简单直接，但"比较集合必须等于写入集合"要靠人维护——这正是 SSA 要解决的。

3. **事件驱动 + predicate**：`For(CR, GenerationChangedPredicate)` 滤掉"自己写 status"引发的自循环；
   `Owns(子资源)` 承接真实状态变化（`readyReplicas` 走动就是要响应的信号）。
   取舍：稳态下零调和（实测 `reconcile_total` 停在 1），代价是必须理解 predicate 的边界。

4. **失败可见化**：调和失败写 `Warning` 事件 + `Ready=False`（Reason 分档：`DeploymentNotFound` /
   `ReplicasNotReady` / `DeploymentNotAvailable` / `ReconcileFailed`），且**不推进** `observedGeneration`。
   取舍：失败不再"静默退避重试"，但 status 的语义要求客户端先看 `observedGeneration` 再看 `Ready`。

5. **测试分两层**：envtest 里**真跑 manager**（真 apiserver+etcd，证明是 watch 驱动的控制循环，
   不是手调 `Reconcile`）；真集群（k3d）补 envtest 永远测不到的那半：Pod 真的起来、删了真的会回来。
   取舍：envtest 没有 kcm/scheduler/kubelet/GC，永远测不了"就绪"与级联回收——必须诚实标注。

## 实测证据（可复现，面试可直接展示）

| 结论 | 证据 / 复现方式 |
|------|----------------|
| 删 Deployment → **3 秒内被重建** | `kubectl delete deploy demo`；事件 `Normal/Created shortlink/demo` |
| 绕过 CR 手改 Deployment → **毫秒级被拉回** | `kubectl set image` 后立即读回；事件 `Normal/Updated` |
| 多副本选主：优雅退出 **1~2 秒**接管（不优雅 **20 秒**） | 租约轨迹里可见 `holder=[]` 的主动释放中间态 |
| **零写入**：16 次空转调和 → **0** 个写请求；1 次真实变更 → **恰好 1** 个 PATCH | `rest_client_requests_total` 求差（`resourceVersion` 测不出这件事） |
| 冻结期间"未收敛"可读 | `kubectl get sl`：`GENERATION 6 / OBSERVED 5` |
| 分配值空值会被 API Server **保留**，不是冲掉 | `go run ./hack/defaulting-probe` |

## 已知边界（诚实清单）

- **没有 webhook**：校验下沉到 CRD 的 OpenAPI schema（`make manifests` 生成）。
- **`selector` 在写入集合里、不在比较集合里**：行为已实测（静默偏离 → 被别的漂移触发时响亮失败），
  修法已定（报告而非修复），尚未实施。
- **未接 HPA**：目前假设"本 operator 是 replicas 的唯一写者"；接 HPA 时 `replicas` 会打架，届时应评估 SSA。
- **手写 diff 而非 Server-Side Apply**：单写者场景够用，多写者场景应迁移。
- 项目一（消费侧服务）**只有设计，未实现**——本 operator 用占位镜像即可完整演示。

## 3 分钟讲稿骨架

| 时长 | 内容 |
|------|------|
| 15s | 定位：把"声明式期望 + 自愈"固化成平台能力（三项目里的生产侧） |
| 30s | 画架构图（CR → 控制循环 → Deployment/Service，与 status 的因果链） |
| 60s | **挑一个最深的技术点讲透**（推荐二选一）：① 幂等判据为什么不能整体 DeepEqual（"API Server 是第二个作者"）；② "零写入"到底怎么证（RV 测不出，要数 API 请求） |
| 30s | 实测证据：删 Deployment 3s 重建 / 手改毫秒级拉回 / 16 次空转 0 写请求 |
| 15s | 已知边界与下一步（诚实清单 + HPA/SSA 的迁移判断） |

## 5 分钟本地复现

```bash
# 单元/集成（envtest 真跑 manager，11 specs）
go test ./internal/controller/... -count=1

# 真集群演示（k3d）
hack/build-local-image.sh operator-demo          # 离线交叉编译 + 导入镜像
kubectl config use-context k3d-operator-demo
make deploy IMG=shortlink-operator:dev
kubectl apply -f config/samples/shortlink_v1alpha1_shortlink.yaml
kubectl get sl                                   # READY / REPLICAS / GENERATION / OBSERVED / URL

# 取证探针（对着真 apiserver 压实验，本项目所有"实测"结论的来源）
go run ./hack/defaulting-probe                   # 默认值/分配值、no-op 写、SSA 字段所有权
go run ./hack/finalizer-probe                    # finalizer/删除对 generation 的影响
```

## 状态

- `main` 分支 CI 三个 job 全绿：**Lint / Tests(含 envtest) / E2E(真 kind 集群)**
- 文档：`docs/day01…day08` 逐日交付记录，含**勘误章节**（实测推翻自己先前结论的地方）
