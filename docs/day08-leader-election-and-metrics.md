# Day 8 — leader election 深挖 + metrics（并顺手把「零写入」证了）

> v1 收尾的第一件：把"多副本安全"和"可被监控"这两件**生产必需**的事落地。
> 两件都是**真集群实测**，不是读文档抄结论。
> （CI 跑绿与一页纸项目简介留到下一轮。）

## 一、交付清单

| 产出 | 位置 | 说明 |
|------|------|------|
| `LeaderElectionReleaseOnCancel: true` + 实测注释 | `cmd/main.go` | A/B 实测（见第二节），注释里写清前提与代价 |
| leader election A/B 实测 | 本文档第二节 | 关闭 20s ↔ 开启 1~2s 接管 |
| metrics 实测 + 抓取姿势 | 本文档第三节 | HTTPS + authn/authz，需要自行绑定 `metrics-reader` |
| **零写入的严格验证** | 本文档第四节 | 16 次空转调和 → 0 个新写请求；1 次真实变更 → 恰好 1 个 PATCH |

## 二、leader election：机制与实测

### 2.1 机制（一张图）

```
副本 A ─┐                      Lease 对象
副本 B ─┼─ 抢同一个 Lease  ──► shortlink-operator-system/6c1ecb47.shortlink.dev
副本 C ─┘   （coordination.k8s.io/v1）   holderIdentity: <pod名>_<uuid>
                                        leaseDurationSeconds: 15
     只有拿到 Lease 的副本启动控制器；其余待命、每 2s 重试。
```

- **为什么需要**：多个副本同时跑同一套控制循环 = 对同一批对象重复写、互相覆盖（每个副本都有自己的 workqueue 和缓存，彼此不可见）。leader election 把"同时只有一个在动手"变成集群层面的保证。
- **三个参数**（controller-runtime 默认）：`leaseDuration=15s`（租约有效期）、`renewDeadline=10s`（续约超时）、`retryPeriod=2s`（待命者重试间隔）。
- **容错语义**：leader 崩溃后，最坏要等 `leaseDuration` 才有人接管——**这就是可用性缺口**（下面用实测把两个数摆出来）。

### 2.2 A/B 实测（k3d，2 副本，删除 leader Pod）

| 配置 | 接管耗时 | 租约轨迹（细采样） |
|------|----------|--------------------|
| `ReleaseOnCancel` **关闭** | **20s** | 租约到期自然失效，无人主动释放 |
| `ReleaseOnCancel` **开启** | **1~2s**（两次复测一致） | `holder=[<旧leader>]` → **`holder=[]`（主动释放）** → 1s 后 `holder=[<新leader>]` |

`holder=[]` 这个中间态就是"主动释放"的直接证据——leader 收到 SIGTERM 时把租约清空，待命副本下一次重试（2s 内）就能拿到，不必等 15s 租约过期。

**为什么能安全开启**：前提是"收到信号后进程立即退出"。本项目 `mgr.Start(...)` 返回后直接结束、没有任何清理动作，所以安全；若将来在 `Start` 之后加了清理逻辑，必须重新评估。

### 2.3 两个实测踩到的坑

1. ~~待命副本是 `0/1 Ready`：controller-runtime 的 `readyz` 与"是否已当选 leader"绑定，非 leader 故意报未就绪。~~
   **上句是错的，已实测否定**：3 副本时**包括两个非 leader 在内全部 `1/1 Ready`**（`ready=true`）。
   查证两条：
   - 源码上 `readyz` 只是 `healthz.Ping`（`cmd/main.go:209`），健康检查服务器也不受 leadership 门控
     （`manager.Server.OnlyServeWhenLeader` 未设置，见 `pkg/manager/server.go:92`）；
   - 我最初看到的 `0/1` 那个 Pod **才创建 12 秒**，只是还没通过首次探测
     （探针 `initialDelaySeconds=5 / periodSeconds=10`）。

   ⇒ 又是一个同类教训（day05 的 RV 判据、day07 的 `k3d image import`、day08 的空文件求和，这是第 4 次）：
   **不要从一次快照推断机制**。要判断"这是有意设计还是暂时状态"，要么复测（等 40 秒再看）、
   要么读源码——两样都比编一个听起来合理的解释便宜。这条错误一度被我写进文档当结论。
2. **同一个镜像 tag 重新 `make deploy` 不会重启 Pod**：Pod 模板没变 → 不触发滚动更新 → **换二进制是幻觉**，leader 还在跑旧进程。本次实测就被这个坑骗了一次（"开启 ReleaseOnCancel 后仍 17s"），实际是旧二进制仍在当 leader。换二进制必须 `kubectl rollout restart` 或改 tag。

## 三、metrics：默认就有，但默认拿不到

### 3.1 安全模型（脚手架默认就很严）

- `--metrics-bind-address=:8443` + `--metrics-secure=true`（默认）→ metrics 走 **HTTPS**；
- 且启用了 `filters.WithAuthenticationAndAuthorization` → **需要带 token 且通过 RBAC 鉴权**；
  实测：不带 token 直接 `curl` → **401**。
- 仓库里的 `metrics-reader` ClusterRole 只授予 `get /metrics`（nonResourceURL），**没有绑定**——绑定关系由抓取方自己建（脚手架惯例）。抓取姿势：

```bash
NS=shortlink-operator-system
kubectl -n $NS create sa metrics-scraper
kubectl create clusterrolebinding shortlink-metrics-scraper \
  --clusterrole=shortlink-operator-metrics-reader --serviceaccount=$NS:metrics-scraper
TOKEN=$(kubectl -n $NS create token metrics-scraper --duration=60m)
kubectl -n $NS port-forward svc/shortlink-operator-controller-manager-metrics-service 18443:8443 &
curl -sk -H "Authorization: Bearer $TOKEN" https://127.0.0.1:18443/metrics
```

### 3.2 哪些指标真有用（以及一个实测发现）

| 指标 | 用途 |
|------|------|
| `controller_runtime_reconcile_total{result="success\|error\|requeue"}` | 调和吞吐与失败率（**error 是告警首选**） |
| `controller_runtime_reconcile_errors_total` | 失败计数（比看日志快） |
| `workqueue_depth` / `workqueue_adds_total` | 队列积压：持续 > 0 说明处理不过来或事件风暴 |
| `rest_client_requests_total{method,code}` | **我们自己向 API Server 发了多少请求**（下一节的关键工具） |
| `leader_election_master_status` 等 | 谁在当 leader（配合 `kubectl get lease` 使用） |

**实测发现**：接管后安静下来时 `controller_runtime_reconcile_total{result="success"}` **停在 1**（只有接管后那一次全量调和），再也不涨。这是"**事件驱动、不轮询**"最直接的证据——它比任何文字描述都硬。（默认 informer resync 是 10 小时量级，所以稳态下确实没有周期性调和。）

## 四、把「零写入」证掉（Day 5 的遗留，终于有办法了）

Day 5 第十节 10.4 的结论是：**「resourceVersion 不变」证明不了零写入**（no-op 写会被 API Server dedupe），要验必须"数 API 请求"。metrics 里的 `rest_client_requests_total` 正好就是那把尺子。

**方法**：临时关掉 leader election（`--leader-elect=false`），否则租约续约的 PUT 会污染计数；然后用"被比较字段之外的变化"触发空转调和。

**结果**：

| 阶段 | 写请求（POST/PUT/PATCH/DELETE 合计） | reconcile 累计 |
|------|--------------------------------------|----------------|
| 基线 | 2 | 1 |
| 触发 **8 次空转**（给 Deployment 加 annotation，每个都会经 `Owns` 入队） | **2**（零增长） | **17** |
| 一次**真实变更**（改 CR 的 `baseDomain`，只影响 `status.url`） | **3**（恰好 +1） | 18 |

逐细节看真实变更那一步的指标：

```
rest_client_requests_total{code="200",method="PATCH"} 1     ← 只有一次 status 写回
rest_client_requests_total{code="201",method="POST"}  2     ← 与基线相同（启动期产生，未增长）
```

结论：

1. **16 次空转调和 → 0 个新写请求**：`deploymentNeedsUpdate` + `patchStatus` 的"无变化不写"是真的（这里"无变化"包括"annotation 变了但不是我管的字段"）。
2. **1 次真实变更 → 恰好 1 个写请求**（status PATCH）：说明写入是**按差异**发生的，不是按调和次数。
3. 基线那 2 个 POST 在空转前后不变；**推断**是 metrics 鉴权用的 `SubjectAccessReview`（被缓存后不再重复），但我没有单独验证——不影响结论就标注为推断。
4. 这也给 Day 6 遗留第 4 项（零写入测试）一个**真集群做法**：抓 `rest_client_requests_total` 求差。要在 envtest 里自动化，需要包一层计数 RoundTripper。

### 4.1 测量方法学（本项目第三次踩到"工具有毒"）

第一次抓基线时 `curl` 其实失败了，`/tmp/m1.txt` 是**空文件**；而我的求和脚本对空文件输出 `0`——于是"写请求总数 = 0"看起来完美支持结论，其实是**自证式的假阴性**。

修法：**先验证捕获非空**（`grep -c '' 文件` > 0）再解读数字，并且**同时打印一个"应该增长的对照组"**（这里就是 `reconcile` 计数：如果它没涨，说明空转根本没发生，结论无效）。

前两次同类教训：`resourceVersion` 测不出多余写请求（day05 10.4）、`k3d image import` 报成功但镜像没进节点（day07 坑 2）。**共同点：验收看结果，不看返回值；解读数字前先确认数字是真的。**

## 五、CI 跑绿：四个根因，全是"本机能跑、Linux 上不行"

接手时三个 job（Tests / E2E / Lint）**全红**，而且不是当天才红的。逐个查 CI 日志（`gh run view <id> --log-failed`）定位到四类根因——**没有一个是 K8s 知识，全是跨平台与构建上下文**：

| # | 现象（CI 日志原文） | 根因 | 修法 |
|---|---------------------|------|------|
| 1 | `open hack\boilerplate.go.txt: no such file or directory` → `make generate` 失败 | Makefile 里用了 **Windows 反斜杠**路径；Windows 能跑，Linux 上是字面文件名 | 改成正斜杠 `hack/boilerplate.go.txt`（两平台都成立） |
| 2 | `Set ENVTEST_VERSION manually (controller-runtime replace has no tag)` → `version must not be empty` | `go list -m` 对**指向本地目录的 replace** 取不到版本（`.Replace.Version` 为空） | `gomodver` 宏：有带版本 replace 用 `.Replace.Version`，否则回退 `.Version`（require 行版本）。实测推出 `v0.25.0` |
| 3 | `reading third_party/controller-runtime/go.mod: no such file or directory`（docker build） | ①`.dockerignore` 只放行根目录的 `go.mod`；②**更关键**：`COPY . .` 在 `RUN go mod download` 之后，那一刻容器里压根没有 `third_party/` | ①补 `!**/go.mod`、`!**/go.sum`；②在 download **之前**显式 `COPY third_party/ third_party/` |
| 4 | 34 个 lint 问题（errcheck/goconst/lll/modernize/revive/staticcheck/unused） | 混合：探针文件被误纳入 lint、Day 1 的 warmup 代码、以及一处真实 deprecation | 修掉全部；其中 `mgr.GetEventRecorderFor`（已 deprecated）**真迁移**到 `mgr.GetEventRecorder`（events.k8s.io/v1），RBAC 组随之改，测试改读 `eventsv1.EventList` |

**三条方法论**（比修法本身更值得记）：

1. **"本机能跑"是最有欺骗性的验证**。根因 1、2 都是"Windows 上好好的"——真正跨平台的写法（正斜杠、对本地 replace 的版本回退）能同时成立。
2. **本地装上 CI 用的同一套工具再修**。装 golangci-lint 时遇到自定义插件在 Windows 下构建失败（`plugin "logcheck" not found`），临时禁用插件块后在本地把 34 个问题收敛到 0，最后**恢复插件配置**再推——不然就要靠一轮轮 CI 试。
3. **修完要能自己验证，而不是"推上去看"**。根因 3 的第一版修法（只改 `.dockerignore`）就是猜的，推上去照样红；第二版用**只依赖本地 busybox 的探针 build** 验证了 `COPY` 顺序确实让 `third_party/controller-runtime/go.mod` 就位，才推。

顺带修正 `docs/day05` 第九节的一句错话（"CI（Linux）无此问题"）——实测 CI 踩的是**同一个** ENVTEST_VERSION 坑，而且根因不是网络而是 replace。

## 六、遗留

| # | 事项 |
|---|------|
| 1 | 一页纸项目简介（投递必需，也是"3 分钟讲项目"的底稿） |
| 2 | 把 metrics 的抓取绑定写成仓库产物（现在只是文档里的命令） |
| 3 | 零写入的 envtest 自动化（计数 RoundTripper） |
