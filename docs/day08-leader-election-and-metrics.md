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

## 六、概念补遗：租约时间账、Lease 对象、分区与返回码

> 这一节是出题后回填的。把「三个参数的户口本 / 谁在什么时刻读 / 分区时到底多久自尽 / Lease 对象长什么样」逐条钉到源码行号上，含两处勘误（6.7）。
> 代码引用版本：`k8s.io/client-go v0.37.0`（`tools/leaderelection/leaderelection.go`、`resourcelock/leaselock.go`）、`sigs.k8s.io/controller-runtime v0.25.0`（`pkg/leaderelection/leader_election.go:102-108`）。

### 6.1 三个参数的户口本

| 参数 | 值 | 谁写 | 谁读 | 语义 |
|------|----|------|------|------|
| `LeaseDuration` | 15s | holder 写进 Lease 的 `spec.leaseDurationSeconds`（`leaderelection.go:447-452`） | **别人** | 「多久没心跳算你死」。不是任何进程的定时器 |
| `RenewDeadline` | 10s | 不落任何对象 | **只有 leader 自己** | 「从最后一次成功续租起，连续 10s 一次都没续上就认输」 |
| `RetryPeriod` | 2s | 不落 | 两边都用 | 竞争者轮询周期（**有 jitter**）；leader 续租间隔（**无 jitter**） |

默认值来自 `third_party/controller-runtime/pkg/manager/manager.go:54-56`；启动校验 `leaderelection.go:77-82`：
`LeaseDuration > RenewDeadline > RetryPeriod * JitterFactor(1.2)`。不满足直接 error，manager 起不来。

**`RenewDeadline < LeaseDuration` 不是凑数**：10 < 15 造出 5 秒余量，保证「旧 leader 已经自尽、新 leader 还不许上桌」，即宁空窗不双主。

### 6.2 竞争者：滑动的 `observedTime`，「读」不等于「抢」

- 入口 `acquire()`（`:252-276`）是 `wait.JitterUntilWithContext(..., RetryPeriod, JitterFactor, true)`：**immediate + sliding**，进程一启动就跑第一次，之后每 `Jitter(2s,1.2)` 跑一次，**永不停止**直到抢到。不存在「等 15 秒才开始看」。
- 能不能抢的判据是**滑动的本地计算**（`:548-554`）：`observedTime + leaseDurationSeconds > now` 算有效。
  `observedTime` 是**本机读到这条记录的时刻**（不是 leader 写的时刻），只在记录的 `spec` 序列化字节变化时刷新（`:487-491`；`leaselock.go:42-54` 用 `json.Marshal` 做 raw 比较）。
- 所以正常续租期间，每个副本读到的心跳都会把自己的 `observedTime` 往前推 ⇒ 过期判断永远为假 ⇒ 它们一直在读、一直在试，但每次都 `return false`。
- 判定用的 `leaseDurationSeconds` 取自**记录里那个值**（旧 leader 当年写的），不是自己的配置 ⇒ 滚动升级新旧混跑不会因为配置不同而互判过期。
- 抢的实质是一次**带 resourceVersion 的 PUT**（`:497-511`）：谁先把自己的身份写进 `holderIdentity` 谁赢，其余人 409 → `false` → 下个周期再来。没有锁服务、没有选主协议，只有一条 Lease 记录的 CAS。

### 6.3 leader：2s 固定节拍 + 10s 预算 + 5s 单请求超时

`renew()`（`:279-312`）是两层循环：

```go
wait.UntilWithContext(ctx, func(ctx context.Context) {                 // 外层：固定 2s，jitter=0
    err := wait.PollUntilContextTimeout(ctx, RetryPeriod, RenewDeadline, true, func(ctx context.Context) (bool, error) {
        if err := ctx.Err(); err != nil { return false, err }          // 优雅退出时短路，避免多发一次无用 PUT
        return le.tryAcquireOrRenew(ctx), nil
    })
    if err == nil { return }                                           // 续上了
    le.metrics.leaderOff(...); logger.Info("Failed to renew lease", ...); cancel()
}, le.config.RetryPeriod)
```

- 正常态：`tryAcquireOrRenew` 走 **fast path 乐观更新**（`:454-467`，用上次缓存的 Lease 对象带 RV 直接 PUT），成功后外层睡 2s 再来 ⇒ 续租间隔 = **2s + 一次 PUT 的 RTT，无 jitter**。
- 失败态：内层每 2s 重试，10s 窗口内约 5 次，全失败才返回 `context deadline exceeded` → 认输退出。**10s 是整个窗口，不是单次 PUT 超时**（`PollUntilContextTimeout` 内部就是 `context.WithTimeout`，`poll.go:45-49`）。
- **单请求还有一层超时**：controller-runtime 给 leader-election 用的 client 单独设了 `config.Timeout = max(RenewDeadline/2, 1s) = 5s`（`pkg/leaderelection/leader_election.go:102-108`，注释写明「防止单个挂起请求吃掉整次 leader 判定」）。这一层决定了黑洞分区的自尽时刻是 12s 而不是更久。

### 6.4 三条时间轴（t0 = 最后一次成功续租）

```
① kill -9 / OOMKilled / 节点掉电：进程什么都没做，没有 t0+10 事件
t0        t0+2   t0+4                     t0+15         t0+19.4
│          ✗     ✗                       │             │
续租成功   进程已死                       └─ 最早可抢 ──┴─ 最晚可抢
                                           （T_obs+15，T_obs∈(t0,t0+4.4]）

② 进程活着但续不上
   a. 快速失败（RST / refused / 403 / 5xx / 409）：
      t0+2,4,6,8 各试一次全失败 → 10s 窗口到期 → t0+10 自尽
   b. 黑洞（丢包不回 RST）：
      单次 PUT 卡 5s（config.Timeout）→ t0+7 第一次报 i/o timeout → t0+9 再试 → t0+12 被 poll 窗口掐断 → 自尽
   两种都不需要网络恢复；两侧共用 5s 余量：自尽(≤12s) < 可抢(≥15s)

③ 优雅退出（kubectl delete pod → SIGTERM）
t_sigterm      +ε                     +≤4.4
tryRelease 写 holder="" + leaseDurationSeconds=1 → 待命副本下一个 tick 抢到（实测 1~2s）
```

`tryRelease`（`:341-347`）是双保险：空 holder 让 `len(holder) > 0` 判据直接失效，`leaseDurationSeconds=1` 让按时间也立刻过期；冲突时递归重试（`:348-355`）。

### 6.5 Lease 对象本身

```yaml
apiVersion: coordination.k8s.io/v1
kind: Lease
metadata:
  name: 6c1ecb47.shortlink.dev      # = cmd/main.go 的 LeaderElectionID
  namespace: shortlink-system
spec:
  holderIdentity: "<pod名>_<uuid>"   # hostname + "_" + uuid，每次进程重启都变
  leaseDurationSeconds: 15
  acquireTime: "..."                 # 只在抢占时刷新，续租保留旧值
  renewTime: "..."                   # 每次续租刷新，MicroTime 精度（保证每次字节都不同）
  leaseTransitions: 3                # 只在抢占时 +1
```

四条性质：

1. 内置资源（`coordination.k8s.io/v1`），**与 Shortlink CRD 无关**，普通存在 etcd 里。
2. **没有 TTL、没有服务端过期、没有 ownerReference、不会被 GC**。apiserver 永远不会因为「过期」改它或删它——**过期纯由客户端算**（6.2 那条判据）。它与 etcd 的 lease（真 TTL）只是重名。
3. 写入只有三个时机（`resourcelock/leaselock.go`）：`Create`（`:57-70`，谁先建谁当）、`Update`（`:73-96`，续租/抢占/release 共用，实现是 PUT + 缓存 RV）、`Get`（`:42-54`）。
4. `became leader` / `stopped leading` 记成 **Event（`reason=LeaderElection`，挂在 Lease 对象上）**（`:99-109`）；RBAC 需要 `leases` 的 `get/create/update` + `events` 的 `create/patch`。

观测口径：`renewTime` 在动 = 活着；冻住但 < 15s = 别人抢不到（正常空窗）；冻住且 > 15s = 该有人接手了。`leaseTransitions` 在涨 = leadership 在反复易手（抖动），应进自监控。

### 6.6 续租 PUT 各种返回码的语义

| 返回 | 原因 | 后果 |
|------|------|------|
| 200 | 正常续租 | 继续当 leader |
| 409 Conflict | 缓存 RV 过期（被抢占 / 被 release / 手工改过） | 回落 slow path 重新 GET；holder≠自己 → false；10s 全 false → 自尽 |
| 403 | RBAC 缺 `leases/update`（或 SA 被换） | 每 2s 失败 → 自尽 → 重启后 acquire 也 403 → CrashLoop |
| 401 | 凭据失效（token 轮换异常） | 同上 |
| 404 | Lease 被删 | 走 GET→Create 分支，Create 成功即**立刻**重新当上 leader（`:476-483`） |
| 5xx / etcd 慢 | apiserver 侧 | 视 10s 窗口内是否还有一次成功 |
| `i/o timeout` / `context deadline exceeded` | 分区、黑洞 | 见 6.4 ② |

实现细节：**这些原因在 `renew` 里全被折叠成 `false`**（`tryAcquireOrRenew` 只返回 bool），只有 poll 窗口到期才升级成 error。所以日志只有一条 `Failed to renew lease`，真正的返回码要看审计日志或临时开 `-v=6`——这也是为什么排这类故障，`renewTime` 的停滞时长比日志更好用。

### 6.7 两处勘误

1. **轮询 jitter 的上限是 4.4s，不是 2.4s。** 先前口头结论「`Jitter(2s,1.2) ∈ [2.0,2.4)`」是错的：`Jitter(d,f) = d + rand*f*d`（`apimachinery/pkg/util/wait/wait.go:92-98`），`f=1.2` 是 **+120%**，不是 +20%。本机实测 10 万次：`min 2.000015852s / max 4.39999994s / avg 3.199237051s`。
   ⇒ 竞争者轮询间隔 ∈ **[2.0, 4.4)s（均值 3.2s）**；抢占窗口 ∈ **[t0+15, t0+19.4)**。
   ⇒ 2.2 节实测的 **20s** 反而更顺了：15s 固定成本 + 最多 4.4s 观测/轮询滞后 + Pod 删除与启动开销。
2. **「分区后要等网络恢复才知道自己续租失败」是错的。** 续租是有返回码的 HTTP PUT：快速失败毫秒级知道，黑洞靠 `config.Timeout=5s` + `RenewDeadline=10s`，**≤12s 必然自尽**，不需要网络恢复。
   要等网络恢复（或重启）才会发生的，是另一件事：**看到 `holderIdentity` 已经换成别人**。旧 leader 自尽时既不知道新 leader 是谁，也不需要知道。

### 6.8 真正的双主来自「冻结」，不来自分区

一条可证伪的推论：续租是 PUT 到同一个 apiserver，写 status 也是 PUT 到同一个 apiserver。

> **如果旧 leader 还能写 status，它的续租 PUT 也该能成功 ⇒ 它仍合法持有租约（`observedTime` 一直被刷新）⇒ 别人抢不到。**

因此**纯网络分区不会产生「旧 leader 继续写脏」**：要么续得上（合法 leader），要么续不上（≤12s 自尽）。

会产生双主的是**进程没有被调度去检查 ctx**：`SIGSTOP`、cgroup freezer、VM 挂起/迁移、极端 CPU 饿死、`io hang`、Go 死锁、cgo 长时间阻塞。Go 的定时器与 context 到期都依赖 goroutine 被调度，进程被冻结时「10 秒到了」这件事没有执行者；解冻瞬间 reconcile goroutine 可能先于 leader election 拿到 CPU，用「我还是 leader」的缓存视角写一次 status，而新 leader 已在工作。**leader election 是 liveness 机制 + 最优努力的 safety，不是 fencing token**，最终兜底仍是 level-triggered 幂等（写前 `DeepEqual`、`Status().Patch(MergeFrom(old))`、只写能用最新真值算出的字段）。

取证三步（区分分区 / 冻结）：

1. `kubectl get lease -o yaml` 看 `renewTime` 是否有 **>15s 的 gap**；
2. 比对**新 leader 的 `acquireTime`** 与旧进程退出/最后一条日志的时间戳：`acquireTime` 更早 ⇒ 有过双主窗口（真事故），更晚 ⇒ 干净交接；
3. 旧进程日志：有 `Failed to renew lease ... context deadline exceeded` ⇒ **它知情**（分区/慢 API/403）；日志里什么都没有、Pod 却活着且 `leaseTransitions` +1 ⇒ **冻结/死锁**（它没机会打印）。

另一个高频场景：**apiserver 慢**（p99 > 5s）。`config.Timeout=5s` 会让单次续租报 `i/o timeout`，但只要 10s 窗口内有一次成功，leadership 就保住——表现是日志断续报错而**不换代**；只有慢到 10s 内 0 次成功才自尽重启，形成「反复换代 + 反复重启」，`leaseTransitions` 肉眼可见地涨。

## 七、遗留

| # | 事项 |
|---|------|
| 1 | 一页纸项目简介（投递必需，也是"3 分钟讲项目"的底稿） |
| 2 | 把 metrics 的抓取绑定写成仓库产物（现在只是文档里的命令） |
| 3 | 零写入的 envtest 自动化（计数 RoundTripper） |
