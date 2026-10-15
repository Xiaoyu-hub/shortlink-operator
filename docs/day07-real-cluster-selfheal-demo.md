# Day 7 — 真集群自愈演示（k3d / k8s 1.35）

> Day 5/6 的验证全部在 envtest 里完成——**那里没有 kube-controller-manager、没有 scheduler、
> 没有 kubelet**，所以"Pod 真的起来了""就绪数真的动过""删了真的会自动回来"这些事，
> 一条都没被证明过。本次把它补上：真集群、真 Pod、真探针、真自愈。
>
> 本日是 v1 里**唯一能演给人看**的产出。

## 一、交付清单

| 产出 | 位置 | 说明 |
|------|------|------|
| readinessProbe + 滚动策略字段 | `internal/controller/shortlink_controller.go`、`api/v1alpha1/shortlink_types.go` | 已在 `628fab7` 提交（比较与写入一起加） |
| 离线镜像构建脚本 | `hack/build-local-image.sh` | 交叉编译 + 自拼最小上下文 + 导入 k3d，全程不走公网 |
| 演示 CR | `config/samples/shortlink_v1alpha1_shortlink.yaml` | 补上 maxSurge/maxUnavailable 示例与镜像注意事项 |
| 本文档 | `docs/day07-real-cluster-selfheal-demo.md` | 五个演示的实测输出 + 环境踩坑 |

集群：k3d `operator-demo`（k3s v1.35.5）。管理器以 Deployment 形式部署，**默认开启 leader election**
（`Lease shortlink-operator-system/6c1ecb47.shortlink.dev`，日志可见——那是 Day 8 的主题）。

## 二、环境踩坑（四个，全部实测踩到并解决）

### 坑 1：集群里任何 Pod 都起不来 —— 节点没有 sandbox（pause）镜像

```
FailedCreatePodSandBox: failed to get sandbox image "rancher/mirrored-pause:3.6":
failed to pull image ... dial tcp 103.97.176.73:443: connect: connection refused
```

原因：`operator-demo` 是 Day 3 建的**空转集群**，从来没起过 Pod，所以 k3s 的 pause 镜像从未被拉取；
而本机到 `registry-1.docker.io` 的连接被拒。

解法：从**已经运行过工作负载**的 `sre-demo` 节点把镜像搬过来（离线搬运）：

```bash
export MSYS_NO_PATHCONV=1        # ← Git Bash 会把容器内路径 /tmp/x 改写成 Windows 路径，必须关掉
docker exec k3d-sre-demo-server-0 ctr -n k8s.io images export /tmp/pause.tar docker.io/rancher/mirrored-pause:3.6
docker cp k3d-sre-demo-server-0:/tmp/pause.tar bin/pause.tar
docker cp bin/pause.tar k3d-operator-demo-server-0:/tmp/pause.tar
docker exec k3d-operator-demo-server-0 ctr -n k8s.io images import /tmp/pause.tar
```

**这类"节点级基础镜像缺失"是离线/受限网络的经典故障**，而且它的表现是"连 Pod 都创建不出来"，
极易被误判成自己的镜像或配置有问题（Pod 卡在 `ContainerCreating`，describe 才能看到真因）。

### 坑 2：`k3d image import` 对多架构 manifest list 静默失败

`k3d image import nginx:latest -c operator-demo` **报成功**，但节点 containerd 里查不到该镜像
（`ctr -n k8s.io images ls | grep nginx` 为空）→ Pod 仍然去拉公网。

解法：绕开 k3d，直接 `docker save` + `ctr images import` 灌进节点（与坑 1 同一条路径）：

```bash
docker save -o bin/nginx.tar nginx:1.27
docker cp bin/nginx.tar k3d-operator-demo-server-0:/tmp/nginx.tar
docker exec k3d-operator-demo-server-0 ctr -n k8s.io images import /tmp/nginx.tar
```

教训：**"命令返回成功"不等于"目标状态达成"**。验收要看结果（在节点里 ls 到镜像），不看返回值。

### 坑 3：`:latest` 标签会让默认 `imagePullPolicy` 变成 `Always`

即使镜像已经导进节点，用 `nginx:latest` 时 Pod 依然 `ImagePullBackOff`：

```
Failed to pull image "nginx:latest": ... dial tcp ...: connect: connection refused
```

因为 K8s 的默认规则是：**tag 为 `latest`（或省略 tag）→ `imagePullPolicy` 默认 `Always`**，
于是本地已有的镜像被忽略、每次都要去 registry 拉。换成显式版本 tag（`nginx:1.27`）后立刻正常。

### 坑 4：仓库根目录的 Dockerfile 在本机用不了（两个原因）

1. 多阶段构建要拉 `golang:1.26` 与 `gcr.io/distroless/static:nonroot`，受限网络下会卡住；
2. 根 `.dockerignore` 默认忽略一切、只放行 Go 源码与 `go.mod/go.sum`
   —— 于是 `bin/` 下交叉编译出的二进制**进不了构建上下文**（`COPY bin/manager-linux /manager` 直接报 not found）。

解法：`hack/build-local-image.sh` 在 `bin/` 下自拼一个最小上下文（只有 linux 二进制 + 极简 Dockerfile，
基础镜像用本机已有的 `busybox`）。**生产/CI 仍应使用根目录的 Dockerfile**（distroless 更小、无 shell、
攻击面更小）——这是本地演示的权宜，不是替代。

## 三、五个演示（全部实测）

### 演示 A：删 Pod（**诚实标注：这一半是 Deployment 控制器的功劳**）

```
删掉 demo-77477c764f-nxxcx
--- 2s 后 ---    demo   False   1          2      ← readyReplicas 掉到 1，我们的控制循环观测到并写回
--- 27s 后 ---   demo   True    2          2      ← 恢复
```

**Pod 的重建是 kube-controller-manager 里的 Deployment/ReplicaSet 控制器干的，不是我们。**
我们的贡献是：`Owns(Deployment)` 收到 Deployment 的 status 变化 → 反查 owner → 重新调和 →
把**真实就绪状态**写回 CR 的 status。这条链在 envtest 里永远测不到。

### 演示 B：删 Deployment 本身（**这一半才是我们的功劳**）

```
kubectl delete deploy demo        # 3 秒后
NAME   READY   UP-TO-DATE   AVAILABLE   AGE
demo   0/2     2            0           3s        ← 已被控制器重建
事件：5s  Normal  Created  shortlink/demo  Created Deployment demo
```

机制：Delete 事件带着 ownerReference 反查 → 入队 → 先查后建 → 重建。**全程无轮询**。
事件是我们发的（`Recorder`），`kubectl describe sl demo` 里能看到。

### 演示 C：绕过 CR 直接改 Deployment → 被拉回

```
kubectl set image deploy/demo shortlink=nginx:1.28
改完立刻看：nginx:1.27  pull=IfNotPresent  probe=80     ← 毫秒级就被改回来了
事件：Normal  Updated  shortlink/demo  Updated Deployment demo
```

**注意 `probe=80` 仍在**：整体覆盖会把 `desired` 的全部内容写回去，探针也是我们声明的一部分。
（顺带这也证明了运行中的管理器带着 Day 7 的探针代码。）

### 演示 D：冻结（paused）——「未收敛」是可读的

```
冻结中（CR: paused=true, replicas=4）：
NAME   READY   REPLICAS   OBSERVED   URL
demo   False   2          2          http://demo.default.svc.cluster.local
Deployment: 2/2                              ← 子资源完全没动

解冻后：
demo   True    4          5          http://demo.default.svc.cluster.local     ← obsGen 一次跳到 5，副本补齐
```

**OBSERVED 停在 2 是设计意图**：冻结期间我们对最新 spec（generation 5）没有做任何调和，
所以代表"最后完整调和的 spec 版本"的 `observedGeneration` 不许推进。
`OBSERVED < generation` 就是"声明了但没达成"的机器可读旗帜——这是 Day 4 设计的因果链在真集群里的样子。

### 演示 E：改 CR 的 `spec.image` → 标准滚动

补丁 `nginx:latest → nginx:1.27` 后，新的 ReplicaSet 建起来、旧 RS 逐步缩容到 0，
最终 `2/2 available`。滚动节奏由默认 `maxSurge=25% / maxUnavailable=25%` 决定；
若在 CR 里声明 `maxSurge: 1 / maxUnavailable: 0`，则会被推进到 Deployment 上（envtest 有断言）。
**有了 readinessProbe，"就绪"才真正代表能接流量**——否则滚动期间新旧 Pod 无法区分，零中断无从谈起。

## 四、必须讲清的一条分界：谁在自愈什么

| 故障 | 谁修复 | 我们的角色 |
|------|--------|-----------|
| 删掉 Pod | **kube-controller-manager**（Deployment/RS 控制器） | 观测 → 写回 status（`Owns` + 事件驱动） |
| 删掉 Deployment/Service | **本 operator** | 先查后建 + ownerReference 反查入队 |
| 手工改 Deployment（漂移） | **本 operator** | diff 收敛 + `Normal/Updated` 事件 |
| Pod 就绪数变化 | 无人"修复"，本身就是正常过程 | 观测 → 刷新 `readyReplicas` / `Ready` 条件 |

ROADMAP 里写的"删 Pod 30s 自愈"是个**叙事捷径**：删 Pod 的自愈来自 Deployment 抽象本身，
而这个抽象是 K8s 提供的、不是我们写的。面试里如果把这个算成自己的功劳，一追问就露馅。
**正确说法**：我做的 operator 管理的是"更高一层的期望状态"——它保证自己管理的对象（Deployment/Service）
即使被删除或被手工篡改也会被拉回，并把真实就绪状态、以及"是否已收敛到最新 spec"暴露给上层。

## 五、未归因的观察（记下来，不当作结论）

删除→重建过程中，同一个 pod template 先后出现过两个不同的 ReplicaSet 名：
`demo-77477c764f` 与 `demo-79b5c99d44`。

我验证到的：

- 重建后的 Deployment **内容与期望逐字段一致**（镜像、`imagePullPolicy: IfNotPresent`、端口 80、TCP 探针、标签）；
- 连续两次"删除→重建"都得到 `demo-77477c764f` ⇒ **期望构造器是确定性的**；
- 演示 C 的拉回同样复用了同一个 RS ⇒ 创建路径与更新路径产出同一份模板。

我不知道的：那一次为何不同。最可能是 kube-controller-manager 的 pod-template-hash
**冲突计数**（`status.collisionCount`）在"旧 RS 尚未回收"时被计入 hash，但我**没有实测到**证据——
重建后 `collisionCount` 归零，历史对象已随旧 Deployment 一起被回收，取不到了。

对正确性的影响：**没有**。模板内容一致，最坏情况是多出一次无害的 rollout。
记为未解观察，等下次复现时抓 `status.collisionCount` 与旧 RS 的存续时间。

## 六、遗留

| # | 事项 |
|---|------|
| 1 | 把上面五个演示脚本化成 e2e（ROADMAP Day 19；顺带能进 CI） |
| 2 | 级联回收（删 CR → 子资源消失）只能在真集群验证，本次未做——留 Day 19 |
| 3 | HPA（Day 9）会写 `spec.replicas`，与我们的 `replicas` 比较形成打架，届时评估 SSA |
| 4 | `hack/build-local-image.sh` 目前只在 Windows + Docker Desktop + k3d 实测过 |
