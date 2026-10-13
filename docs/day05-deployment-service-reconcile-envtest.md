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
3. **错误即退避**：任何子资源操作失败都会带着 err 返回，由 workqueue 的指数退避按 key 重试，
   不会吞错也不会死循环。

## 三、幂等核心：为什么不能整体 DeepEqual

新手最常见的 operator 死循环：`reflect.DeepEqual(actual, desired)` 恒为 false，控制器对「其实一致」
的对象无限 Update。因为对象经过 API Server 准入后被补了大量**默认值**与**分配值**：

| 字段 | 谁补的 | 整体比较的后果 |
|------|--------|----------------|
| `ImagePullPolicy`、`TerminationMessagePath`、`Protocol=TCP` | API Server defaulting | desired 为空 vs actual 有值 → 恒不等 |
| Service `ClusterIP` / `ClusterIPs` / `IPFamily` | API Server 分配 | desired 为空 → 恒不等，且 Update 会把 IP 冲掉 |
| Service/NodePort 的 `nodePort` | API Server 分配 | 同上 |

正确姿势（本实现）：**只比较自己声明并负责收敛的字段子集**——Deployment 比
labels/replicas/template labels/容器 Name·Image·Env·Resources·Ports；Service 比
labels/Type/Selector/Ports(Name·Port·TargetPort·Protocol)。更新时把 ClusterIP/NodePort 先从
actual 搬回 desired 再整体覆盖，避免冲掉分配值。

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

- Day 5 面试题 5 问待作答（见聊天）；答对进 Day 6：自愈专项用例（删 Deployment 自动重建已有机制，
  补为正式测试）+ 事件记录 + 滚动策略字段（maxUnavailable/maxSurge）。
- `make test` 在本机仍会卡在 setup-envtest（DNS 劫持），已在第二节备注绕过路径；CI（Linux）无此问题。