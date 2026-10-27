# shortlink-operator

[![Lint](https://github.com/Xiaoyu-hub/shortlink-operator/actions/workflows/lint.yml/badge.svg)](https://github.com/Xiaoyu-hub/shortlink-operator/actions/workflows/lint.yml)
[![Tests](https://github.com/Xiaoyu-hub/shortlink-operator/actions/workflows/test.yml/badge.svg)](https://github.com/Xiaoyu-hub/shortlink-operator/actions/workflows/test.yml)
[![E2E](https://github.com/Xiaoyu-hub/shortlink-operator/actions/workflows/test-e2e.yml/badge.svg)](https://github.com/Xiaoyu-hub/shortlink-operator/actions/workflows/test-e2e.yml)

用 Kubernetes Operator 把短链服务的「声明式期望状态 + 故障自愈」固化成平台能力：
写一个 `Shortlink` CR 声明你要什么，控制器持续把 Deployment / Service（以及按需的 HPA）收敛到那个状态，
并把**真实就绪状态**与**是否已收敛到最新声明**暴露成机器可读字段。

个人学习项目（不是企业项目、不是生产系统）。定位是三个项目里的「生产侧」：
[sre-from-zero](https://github.com/Xiaoyu-hub/sre-from-zero)（消费侧：部署/监控/告警/手动演练）
→ burngate（决策侧：告警何时该闹）→ **本项目（生产侧：闹完之后谁来收敛）**。

## 能力边界（v1，已收口）

做到的：

- `Shortlink` CRD：`image` / `replicas` / `port` / `serviceType` / `baseDomain` / `env` / `resources` /
  `maxSurge` / `maxUnavailable` / `paused` / `autoscaling`，`replicas` 与 `autoscaling` 用 CEL 互斥校验
- 控制循环：期望对象由纯函数构造（只读 CR），与集群现状**只比本 operator 管辖的字段**，有偏移才写
- 自愈：Deployment / Service / HPA 被删或被改歪都会被拉回；`kubectl delete deploy` 后自动重建
- status 子资源：`readyReplicas` / `observedGeneration` / `Ready` 条件（`metav1.Condition`）/ `url`
- 失败可见化：Warning 事件 + `Ready=False` 分档（`DeploymentNotFound` / `ReplicasNotReady` /
  `DeploymentNotAvailable` / `DeploymentReconcileFailed`），失败时**不推进** `observedGeneration`
- 滚动更新：`maxSurge` / `maxUnavailable` 透传给 Deployment；改 `spec` 自动滚动
- HPA：声明 `spec.autoscaling` 后**让位**给 HPA——不再写 `replicas`、不再比它、覆写前搬回
- 运维面：leader election（多副本单主）、Prometheus metrics、`kubectl get sl` 打印列
- 测试：envtest 15 个 spec（真跑 manager）+ kind e2e；CI 三 job（Lint / Tests / E2E）全绿

故意没做的（见「路线图与范围」）：FaultInjection CRD（v2）、validating/defaulting webhook、
Helm chart、多集群。

## 架构

```
kubectl apply ──► Shortlink(CR)   spec: image / replicas / port / env / maxSurge / autoscaling …
                        │  （期望状态，只有用户能写）
                        ▼
              ShortlinkReconciler（控制循环）
              ├─ desiredDeployment(cr)   纯函数：只读 CR，产出内存对象
              ├─ Get → 只比"本 operator 管辖的字段" → 有偏移才 Create/Update
              ├─ 同样处理 Service（额外把集群分配的 ClusterIP / NodePort 搬回去）
              ├─ 同样处理 HPA（声明了才建；撤掉声明就删）
              └─ status 写回：readyReplicas / Ready 条件 / observedGeneration / url
                        │
                        ▼
          Deployment / Service / HPA（真实对象）
                        │ kube-controller-manager / kubelet 报回真实状态
                        └─► Owns(子资源) 事件反查 ownerReference → 重新入队（事件驱动，无轮询）
```

稳态下**不轮询**：`For(CR, GenerationChangedPredicate)` 滤掉自己写 status 引发的自循环，
子资源的真实变化靠 `Owns()` 反查。实测空转 16 次调和 = 0 个写请求。

## 关键设计决策（每条都带取舍）

1. **spec/status 分离 + `metav1.Condition` + `observedGeneration`**，而不是自定义 `Phase` 字符串。
   `observedGeneration < generation` 就是「声明了但还没达成」的机器可读旗帜，能被 `kubectl wait` 与告警规则直接消费。
   取舍：条件比 Phase 难写。
2. **幂等判据 = 只比「我管辖的字段」**，不能整体 `DeepEqual`：API Server 会在准入时补 defaulting
   （`imagePullPolicy`、`strategy`、探针默认值…）与分配值（`clusterIP`、`nodePort`），整体比较恒为不等 → 每轮都写。
   取舍：比较集合必须等于写入集合，靠人维护——这正是 SSA 要解决的问题。
3. **`selector` 在写入集合里、不在比较集合里**：手工改窄 selector 仍能匹配旧 Pod，会**静默**留下不接
   `maxSurge` 管的孤儿副本；把它放进比较集合会变成「每次 diff → Update → 被不可变字段拒绝 → 无限退避」。
   正确处置是**报告而非修复**：`SpecDiverged` 条件 + Warning 事件 + 人工介入（尚未实施，见已知限制）。
4. **失败可见化但不掩盖**：失败时写 Warning 事件、`Ready=False`、**不推进** `observedGeneration`；
   `cause` 保留子资源层的原始错误（例如 `spec.selector: Invalid value: field is immutable`），
   因为现象 → 分类 → 根因是三层，压成一句「调和失败」就丢掉了根因。
5. **HPA 用「让位」而不是 SSA 划分所有权**：声明 `autoscaling` 后必须**同时**改三处——
   期望构造器不再写 `spec.replicas`、`needsUpdate` 不再比它、覆写前把它搬回；
   连 `Ready` 的判据也从「CR 的 replicas」换成「Deployment 自己的 replicas」。
   取舍：能用，但每多一个写者就多三处改动——这就是 SSA 的价值所在。
6. **测试分两层**：envtest 里**真跑 manager**（真 apiserver + etcd，证明是 watch 驱动的控制循环，
   不是手调 `Reconcile`）；真集群（k3d）补 envtest 永远测不到的那半：Pod 真的起来、删了真的会回来、
   删 CR 真的会级联回收（envtest 没有 kube-controller-manager，Deployment 不会长出 Pod）。

## 实测证据（可复现）

| 结论 | 数字 / 证据 | 复现方式 | 出处 |
|------|------------|---------|------|
| 删 Deployment → 3 秒内被重建 | 事件 `Normal/Created`，新对象 UID 变化 | `kubectl delete deploy <name>` | docs/day07 |
| 绕过 CR 手改副本/镜像 → 毫秒级被拉回 | 0.3–0.5s 回到期望 | `kubectl scale deploy` / `kubectl set image` | docs/day07、docs/day18-19 |
| 稳态零写入 | 16 次空转调和 → **0** 个写请求；1 次真实变更 → **恰好 1** 个 PATCH | `rest_client_requests_total` 求差（`resourceVersion` 测不出这件事） | docs/day08 |
| 删 CR → 子资源级联回收 | Deployment 0.4s / Service 0.5s / Pod 2.8s 消失（三轮 2.75–3.09s） | `kubectl delete sl` 后轮询子对象 | docs/day18-19 |
| 滚动更新零中断 | `maxUnavailable=0`：0 次连接被拒；默认 25%：一轮滚动 66/5 次被拒 | 常驻压测探针（~10 req/s） | docs/day10 |
| HPA 接管副本数 | 1 → 4 → 5（上限）→ 稳定窗口后缩回 1，控制器一次都没写副本数（`Scaled` 事件 0 条） | 打 CPU 压测 | docs/day09 |
| 多副本选主 | 优雅退出 1–2 秒接管；`kill -9` 约 20 秒 | 删 leader Pod 看租约 `holder` 轨迹 | docs/day08 |
| RBAC 无越权、无被拒 | `forbidden: 0`、事件写入失败 `0`（149 行日志）；`can-i get secrets` = no | `kubectl auth can-i --list --as=<SA>` | docs/day18-19 |
| 一条命令复现 v1 闭环 | 16 项检查全 PASS（全局 56s / 跳过构建 29s） | `hack/demo-v1.sh` | docs/day18-19 |

## 快速开始

前置：Docker、k3d（或 kind）、kubectl、Go 1.26+。

```bash
# 1. 建集群
k3d cluster create operator-demo

# 2. 一条命令跑完 v1 闭环：apply CR → 三件套 → 自愈 → 拉回偏移 → 滚动 → 级联回收
bash hack/demo-v1.sh
```

`hack/demo-v1.sh` 会交叉编译 manager、打成 `shortlink-operator:dev` 并导入 k3d（全程不走公网），
然后逐项打印 PASS/FAIL 与耗时。常用开关：

```bash
SKIP_BUILD=1 bash hack/demo-v1.sh     # 镜像已在集群里，只跑演示
REPLICAS=2 bash hack/demo-v1.sh       # 多副本
IMAGE=<你的短链镜像> PORT=8000 bash hack/demo-v1.sh   # 换成项目一的短链服务
```

手工分步（等价于脚本里的步骤）：

```bash
kubectl apply -k config/crd
kubectl kustomize config/default | sed 's|controller:latest|shortlink-operator:dev|' | kubectl apply -f -
kubectl apply -f config/samples/shortlink_v1alpha1_shortlink.yaml

kubectl get sl                     # READY / REPLICAS / GENERATION / OBSERVED / URL
kubectl delete deploy shortlink-sample    # 看它自己回来
kubectl delete sl shortlink-sample        # 看子资源被 GC 带走
```

> `config/samples` 里的镜像默认是 `ghcr.io/...`，离线环境请改成节点里已有的镜像（例如 `nginx:1.27`），
> 且**不要用 `:latest`**——`imagePullPolicy` 会变成 `Always`，离线导入的镜像会被忽略。

## 测试

```bash
# Linux / CI（envtest 需要 KUBEBUILDER_ASSETS，make test 会自动装）
make test
make test-e2e          # 需要 docker，会起一个独立 kind 集群

# Windows 本机：chocolatey 的 GnuWin32 make 跑不动这套 Makefile，直接调 go
go test ./internal/controller/... -count=1
bash hack/manifests.sh   # 等价于 make manifests（重生成 CRD / RBAC）
```

测试数据：envtest **15 个 spec** 全绿（`go test` 实测 23–44s），覆盖创建 / 更新 / 删除 / 自愈 /
幂等零写入 / 冻结（`paused`）/ 失败可见化 / HPA 让位 / 双 CR 隔离。CI 三个 job 在 `main` 上全绿。

## 仓库结构

```
api/v1alpha1/shortlink_types.go        CRD schema（含 CEL 互斥校验）
internal/controller/shortlink_controller.go   控制循环（期望构造 / diff / status / 事件）
internal/controller/*_test.go         envtest（真跑 manager）
cmd/main.go                           manager 入口（leader election、metrics、健康探针）
config/                               kustomize 清单（crd / rbac / manager / samples）
hack/demo-v1.sh                       v1 一键演示（16 项检查）
hack/build-local-image.sh             离线交叉编译 + 导入 k3d
hack/manifests.sh                     本机重生成 CRD/RBAC（绕开 make）
hack/*-probe/                         取证探针：默认值/分配值、no-op 写、finalizer 对 generation 的影响
docs/dayNN-*.md                       逐日交付记录（含勘误章节：实测推翻自己结论的地方）
test/e2e/                             kind 上的 e2e
```

## 已知限制

- **没有 webhook**：校验下沉到 CRD 的 OpenAPI schema + CEL 规则，够用，但没有「跨字段之外」的校验能力。
- **手写 diff 而非 Server-Side Apply**：单/双写者场景够用（HPA 已用「搬回」解决），
  再多一个写者就应该迁移 SSA；目前 `managedFields` 不参与任何判断。
- **`selector` 漂移只报告、不修复**（第 3 条设计决策）：`SpecDiverged` 条件与事件尚未落地。
- **子资源写没有 `RetryOnConflict`**：切换 `autoscaling` 开关时，HPA 控制器可能同时改
  Deployment 的 `replicas`，我们的 `Update` 会吃 409，表现为一条 Warning 事件 + 退避重试后收敛
  （实测记录见 docs/day18-19 第四节）。行为无害，但多花了一轮调和。
- **envtest 有边界**：没有 kube-controller-manager / scheduler / kubelet，所以「Pod 真的起来」
  「级联回收」「HPA 真的扩缩容」「滚动节奏」只能在真集群验证，测试里诚实标注。
- **只在单节点 k3d 上验证过**：上面所有秒数都是「单节点 + 空集群」的读数，不是 SLA。
- **演示用占位镜像**：默认 `nginx`，因为离线集群里不一定有短链服务镜像；
  `IMAGE` / `PORT` 可换成真实服务，但「真实流量下的行为」没有验证过。
- **`status.url` 是拼出来的 DNS 名**，在演示集群里 CoreDNS 缺失（`ImagePullBackOff`），
  这个 URL 从未被真正解析过——`Ready=True` 说的是「副本就绪」，不是「域名可达」。

## 路线图与范围

**本项目按 v1 收口**（ROADMAP 的 Day 1–20）。后续两阶段是加分项，明确不做：

| 阶段 | 内容 | 状态 |
|------|------|------|
| v1（Day 1–20） | CRD + 控制循环 + 自愈 + envtest + 真集群联调 | 完成 |
| v2（Day 21–30） | FaultInjection CRD：`kubectl apply` 即故障演练 | 未做 |
| v3（Day 31–40） | webhook / Helm chart / 博客 / 多集群 | 部分提前完成（metrics、leader election、CI 已做），其余未做 |

若继续，第一件事是 v2 Day 21——它承接的是真实缺口：现在做故障演练还得手敲命令，
而「配置即演练」才是这个项目想讲的下一层故事。

## 文档索引

- `docs/project-one-pager.md` — 一页纸：定位、架构、设计取舍、3 分钟讲稿
- `docs/day02…day10` — 逐日交付记录
- `docs/day11-17-mapping.md` — Day 11–17 对账（为什么没有逐日文档、每条证据在哪、三个真缺口）
- `docs/day18-19-v1-acceptance.md` — 级联回收实测、RBAC 审计、一键演示、已知 409

## 许可证

仓库暂未附 LICENSE 文件；引用/复用前请先开 issue。
