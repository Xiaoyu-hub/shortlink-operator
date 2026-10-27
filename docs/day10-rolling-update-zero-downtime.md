# Day 10 — 滚动更新与「零中断」的成立条件

> ROADMAP 的自检写的是「滚动期间服务零中断（项目一压测脚本验证）」。项目一只有设计没有实现，
> 所以本日自己搭了一只探针来测。结论比预期更有意思：**真正决定零中断的是 `maxUnavailable`，
> 而教科书里必提的 `preStop` 在本环境里没有测出收益**（我也没把它留在代码里）。
>
> 更重要的是：本日三次踩到**测量工具本身失效**，那部分比滚动更新的知识更值得记。

## 一、实验装置

探针是一个 `busybox` Pod，以固定速率对 Service 的 ClusterIP 发请求并统计失败：

```sh
msg=$(wget -q -T 5 -O /dev/null http://10.43.202.156/ 2>&1)
[ -z "$msg" ] && ok=$((ok+1)) || { case "$msg" in *refused*) ref=...;; *) other=...;; esac; }
sleep 0.05
```

三个刻意的设计：**按错误类型分类**（`connection refused` 与 `download timed out` 是两回事，见第三节）、
**速率控制在 ~10 req/s**（既能抓住秒级窗口，又不至于让探针自己成为负载源）、
**按时间戳打印每一行**（便于和滚动窗口对齐）。

被测量的对象是 `demo` 这个 CR（5 副本、TCP 就绪探针）。触发滚动的方式是改 `spec.env`——
它属于被比较的字段、会改变 pod template，因而产生新 ReplicaSet 并触发标准 RollingUpdate；
用改 env 而不是改镜像，是为了省掉一次镜像导入（两者的滚动路径完全相同）。

## 二、测量结果

| 编号 | 配置 | 滚动时长 | 连接被拒 |
|------|------|----------|----------|
| M0 | 默认策略 25%/25%，无 preStop | 14s | **66 次**（密集爆发） |
| M1 | 安全策略 1/0，无 preStop | 41s | 41 次（含噪声，见第三节） |
| A | 安全策略 1/0，无 preStop（控制器冻结） | 23s | **0 次** |
| B1 | 默认策略 25%/25%，无 preStop（控制器冻结） | 16s | **5 次** |
| B2 | 默认策略 25%/25%，**有 preStop sleep 5**（控制器冻结） | 18s | **5 次** |

（A/B1/B2 是在**冻结控制器**的状态下做的：把 manager 缩到 0，由我直接改 Deployment，
这样唯一变量才是被比较的那一项。事后把 manager 恢复，它正确地把我的手工改动全部拉了回来——
顺带验证了自愈。）

**结论**：

1. **`maxUnavailable=0` 是决定性的**：实测 0 次失败，而默认的 25% 会有个位数的连接被拒。
   机制很清楚——`maxUnavailable=0` 意味着「新 Pod 必须就绪，才允许动旧 Pod」，于是就绪副本数
   在任何时刻都不会低于期望值；而 `maxUnavailable>0` 允许旧 Pod 在新 Pod 就绪前就被摘除。
2. **`preStop` 在本环境没有测出收益**（B1 与 B2 都是 5 次，且滚动时长只差 2 秒——
   如果终止真的多等 5 秒 × 5 个 Pod，时长不该只多 2 秒）。**所以我没有把它留在代码里**：
   它给每次终止增加 5 秒成本，而我拿不出它换来什么的证据。
3. **残余失败的原因未归因**。5 次/轮的被拒既没被 `maxUnavailable=0` 消除（A 是 0，但那是在安全策略下），
   也没被 `preStop` 消除。教科书说这是"端点收敛延迟"，但若真是那个窗口，preStop 让容器多活 5 秒
   就该挡住它。候选解释是 kube-proxy 规则重写期间的瞬时拒绝，**但我没有证据，所以标为未解决**。
4. 教科书里 `preStop` 的经典适用场景（应用**排空连接**很慢、或存在**长连接**如 HTTP/2、WebSocket）
   与我们的探针（短 HTTP 请求、拿到响应即断）不同。所以本日的结论应读作
   「**在本装置的采样分辨率下未复现**」，而不是「preStop 没用」。

## 三、三次测量工具失效（本日最值钱的部分）

**第一次：探针跑完就退出了。** 我写的循环上界是 20000 次请求，而探针速率约 500 req/s——
它在 37 秒内跑完并退出（`phase=Succeeded`）。之后 M1'、M2、M3 三次测量的"零失败"全部无效：
我读的是一份**已经停止写入的日志**。这和 Day 8 那次「空文件求和成 0」是同一类错误，
只不过这次是"仪表停止测量"，而上一次是"仪表根本没测到"。

修法与纪律：探针改成按**时间**上界（1 小时）而不是次数；每次测量前后都打印探针的计数，
**两次读数必须不同**，否则这次测量作废。

**第二次：探针自己成了故障源。** 把速率提到 500 req/s 之后，失败开始以「每秒 1~9 次」的
零散形态出现并持续存在。密集爆发（每秒 20~24 次）与零散分布是两种不同的现象：前者是
真中断，后者是探针在高 CPU 争抢下自己超时（`-T 2` 太短、每次 `wget` 都要 fork 一个进程）。
所以速率被压到 ~10 req/s，超时放宽到 5 秒。

**第三次：错误类型没分开。** 一开始只统计"失败"，于是「连接被拒」（路由到了一个已经没有监听
的 Pod）和「下载超时」（丢包或自身争抢）混在一个数字里。分开统计之后才看清 M0 的 66 次里
有 56 次是密集爆发（真事件），其余是噪声。

三条合起来是一条纪律：**先证明仪表在工作，再相信仪表的读数；并且把"失败"拆成有意义的类型。**

## 四、顺带发现的集群环境问题（与本日代码无关，但影响结论可信度）

**CoreDNS 一直是坏的。** 排查探针为什么连不上时发现，`kube-dns` 处于 `ImagePullBackOff` 已 7 天
（这个集群是离线建的，k3s 需要的基础镜像拉不到）。也就是说**这个集群的 DNS 从来没工作过**。

由此推出一个必须诚实记录的事实：我们暴露的 `status.url` 是
`http://<name>.<ns>.svc.cluster.local` 这样的 **DNS 名**，而它在这个环境里**从未被解析成功过**。
Day 5 起的 `Ready=True` 从来没有验证过那个 URL 真的能访问——它只是我们从 CR 字段拼出来的字符串。
（这也解释了为什么探针最终改成直连 ClusterIP：DNS 只是名字解析，不影响"端点路由"这条被测路径。）

顺带第三次遇到同一个环境问题：**k3s 需要的基础镜像在离线集群里缺失**（前两次是 pause 与
metrics-server，这次是 CoreDNS）。前两次用"从另一个节点导出镜像再导入"解决了，这次**失败了两次**
——`sre-demo` 节点里那个 coredns 镜像本身是残缺的（blob `03c9b6…` 缺失，导出时 `short read`）。
按纪律停止重试，改为绕开 DNS，并把它记为待处理项。

还有一条工具链问题：这个集群的 kubeconfig 里 `server: https://0.0.0.0:57987`，
而 `0.0.0.0` 在 Windows 上是歧义地址——它一度被宿主上另一个进程抢走连接，`kubectl` 收到的
是那个进程的 HTML 页面而不是 API Server 的 JSON。改用 `--server=https://127.0.0.1:57987` 覆盖即可
（不改文件、不动全局 context）。

## 五、遗留

| # | 事项 |
|---|------|
| 1 | 修 CoreDNS（需要可用的镜像来源），修好后重测 `status.url` 是否真的可达 |
| 2 | 未归因的残余连接被拒：需要在更高采样率下重复多轮滚动来定位，或直接用抓包/iptables 观察 |
| 3 | `preStop` 若要引入，应先构造能复现经典场景的实验（慢排空应用或长连接），再决定 |
| 4 | 本日的探针与实验步骤尚未脚本化（现在只是文档里的命令） |

## 六、面试演练收口（2026-10-26）

小测 5 题均分 82（100/95/55/70/90）通过；正式六题均分 **65**（70/80/62/65/78/63）；
口述 3 题均分 **68**（70/80/55），一处硬错已订正。结论：收口，进 Day 11。

### 六.1 演练中确立的结论

1. **`maxUnavailable=0` 保证的是「副本数不低于 replicas」，不是「流量路由正确」。** 两者是不同问题，后者靠 `preStop`。
2. **控制器不做全局最优分配，只做两个各自独立、只看局部约束的单步判断**：扩新 RS 只看「总数 < replicas+maxSurge」，不看旧 RS 有几个可用；缩旧 RS 只看「可用数够不够 replicas-maxUnavailable」。`maxU=0` 锁死「缩旧」，replicas 提升又新开 surge 空间 ⇒ 两个方向同时被用满。**控制器没有「留一个试错就够」的经济性判断**，所以坏镜像会长期吃掉 `replicas+maxSurge` 的容量——这是必须配 `progressDeadlineSeconds` 与告警的原因。
3. **卡住的 rollout 不会持续重试**：新 Pod 停在 `ErrImagePull` 时状态不再变化 ⇒ 没有 RS 事件 ⇒ 控制器不被唤醒，只能等 resync（默认 10h±jitter）。
4. **三个「版本号」不能混**：`resourceVersion`（任何写入都可能变）、`metadata.generation`（spec 变才 +1）、`deployment.kubernetes.io/revision`（只在创建新 RS 时 +1）。改 `spec.replicas` ⇒ RV 变、generation 变、**revision 不变**。
5. **`可用 >= replicas - maxU` 只是设计意图，卡住时会被打破**：实测 replicas=4、maxSurge=1、maxU=0 那轮，总数 5（=4+1 ✓），但实际可用只有 3，低于下限 4。控制器只能尽力，卡在中间态——这正是「需要告警而不是靠不等式反推行为」的理由。

### 六.2 `availableReplicas` 为什么会假绿（三处不看）

它只回答一个问题：Pod 的 `Ready` condition 是否成立。它不看：

1. **探针写得对不对** — readiness 路径 404、只探 TCP 不探业务、`initialDelaySeconds` 过长 ⇒ Ready 与实际可用脱钩。
2. **流量面** — Endpoints/kube-proxy/ingress 规则未更新、Service selector 未匹配、副本挤在同一节点 ⇒ Ready 但没人给它发流量，或节点挂掉整组不可用。
3. **进程内部** — 队列积压、下游依赖挂掉、5xx 飙升；只要探针通，照样算 available。原因是探针打的 `/healthz` 与业务链路几乎不重叠：不查库的 healthz 在数据库挂掉时仍返回 200。

**不能靠加强探针解决**：深度探针会让下游抖动变成全体 Unready ⇒ 全线雪崩。正确分工是 readiness 只探「本进程能否服务」，依赖健康交给 metrics + 告警。

### 六.3 零值、omitempty 与 SSA（含一处自我更正）

- `appsv1.Deployment` 的 `MinReadySeconds int32` 带 `omitempty`：值为 0 ⇒ key 消失 ⇒ SSA 视为「不声明」⇒ 原值保留、且**不报错**。
- `applyconfigurations` 里字段是**指针** `*int32`：`WithMinReadySeconds(0)` 会把 0 写进 JSON。**所以 SSA 表达得了「声明零值」**，表达不了的是「写 null 回退默认」——那要用 `unstructured` 或 `client.RawPatch`。列表字段清空（`env` 3→0）同属后者。
- 语义三分：`0` = 我声明它是 0；键缺失 = 我不声明；`null` = 清空/回退默认。typed struct 的 `int32` 分不出前两者，这正是 applyconfigurations 全用指针的原因。

**更正**：本文件早前「改 SSA 会破坏无偏移零写入断言（每次 PATCH 都推进 resourceVersion）」的表述**说过头了**。apiserver 在 `GuaranteedUpdate` 有 no-op 优化，内容完全相同的 patch 很可能不推进 RV（未实测）。准确说法是：SSA 下「没发写请求」这个**观测手段**失效（每轮都发 PATCH，与偏不偏移无关），幂等断言本身要改写成「发出去之后 RV / managedFields 没变」。另因 `For + GenerationChangedPredicate`，RV 变而 generation 不变的事件会被 predicate 丢弃，不会自循环。

### 六.4 `applyconfigurations` 的必要性与代价

`client.Apply` 的形参要求 `runtime.ApplyConfiguration`，而 `appsv1.Deployment` 没实现 ⇒ 编译报 `*appsv1.Deployment does not implement runtime.ApplyConfiguration`。必须引 `k8s.io/client-go/applyconfigurations/apps/v1`（字段全为指针 + `With*` 链式）。代价三条：

1. 所有权冲突（`conflict with "kubectl": .spec.replicas`），`client.ForceOwnership` 会踢掉别人的字段归属；
2. 不可变字段照样被拒（`spec.selector` 仍报 `field is immutable`）；
3. 零写入断言要改写（见六.3）。

### 六.5 测试为何没抓到 minReadySeconds 被抹回

不是「漏了一个用例」，而是**测试与实现共享了同一个错误假设**：测试的 `desired` 来自我们自己的构造器，构造器不设 `minReadySeconds` ⇒ 0 vs 0 ⇒ 就算比较也发现不了。

正确写法必须从外部注入一个「不该被我们管理的值」：apply 一个手工设 `minReadySeconds: 30` 的 Deployment → 触发一次 rollout → 断言它仍然是 30。

### 六.6 待补

- SLI/SLO 口径（Q10.5 的主要失分点）：SLI 是可测指标不是「可用性」；SLO 带窗口；SLA 是对外合同。P99 看不见错误率——1% 的 500 若耗时极短会落在最快端，P99 反而变好看，所以延迟 SLI 与错误 SLI 必须都看。
- 「旧 RS 为什么被放到 3 而不是 4」只有机制层解释，**未逐行核实** controller 算法（按深度上限封顶）；要确认需读 `pkg/controller/deployment/rolling.go` 或重跑实验抓 scale 调用日志。
