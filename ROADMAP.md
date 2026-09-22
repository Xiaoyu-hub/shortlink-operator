# Kubernetes Operator 端到端学习项目 — 四十日计划与交付物

> 目标：2 个月主力时间（40 个工作日 × 8h）用 kubebuilder 从零开发一个 Kubernetes Operator，把项目一的短链服务变成「声明式期望状态 + 控制循环 + 故障自愈」的平台能力——用监控发现问题，用代码把预防与自愈固化成平台能力。
> 定位：个人学习项目，诚实展示学习路径。三项目闭环：项目一（sre-from-zero）消费侧 → 项目二（burngate）决策侧 → 本项目（shortlink-operator）生产侧。
> 时间单位：Day 1–40 为工作日（8h/天），周末弹性缓冲，总量不变。

---

## 〇、阶段总览

| 阶段 | 范围 | 天数 | 里程碑 |
|------|------|------|--------|
| **v1（核心）** | Shortlink CRD + 自愈控制循环 + envtest 单测 | Day 1–20 | 删 Pod 30s 自愈、改版本自动滚动 |
| **v2（加分）** | FaultInjection CRD 声明式故障演练 | Day 21–30 | 一条 `kubectl apply` 即演练，配置即演练 |
| **v3（弹性）** | 观测性 + leader election + webhook + CI | Day 31–40 | 可发布：CI 绿、自监控、多副本安全 |

被管理对象 = 项目一的短链服务；两个项目讲同一个故事，叙事闭环。

---

## 一、四十日计划

### 阶段 v1（Day 1–20）核心：Shortlink CRD + 自愈控制循环

#### Day 1 — Go 环境 + 语法热身
- 任务：装 Go 1.22+，跑通 go module 项目；复习 goroutine / channel / interface（reconciler 只用到这三样）
- 产出：本地 `go run` 一个 http 小服务
- 自检：不查文档能写出并发安全的计数器（Mutex + goroutine）

#### Day 2 — controller-runtime 心智模型
- 任务：读 Kubebuilder Book（CronJob 教程）前三章；吃透「期望状态 → watch → Reconcile → 现实趋近」闭环
- 产出：能徒手画出控制循环闭环图
- 自检：能解释 reconcile 为何会被反复触发、为何必须幂等

#### Day 3 — 本地集群 + 脚手架跑通
- 任务：装 kind/k3d；`kubebuilder init` 生成骨架，通读 Makefile 目标（generate/manifests/test/build/install/deploy）
- 关键命令：
  - `kubebuilder init --domain shortlink.dev --repo github.com/<you>/shortlink-operator`
  - `k3d cluster create operator-demo`
- 产出：空骨架 `make build` 通过
- 自检：`make install` 后 manager 在 kind 里只空转也能跑起来

#### Day 4 — CRD 设计（spec）
- 任务：定义 Shortlink spec：`replicas(int32)` / `image(带tag)` / 滚动策略（`maxUnavailable`、`maxSurge`）；写 `+kubebuilder` 标记（object:root、subresource:status、printcolumn）
- 产出：`api/v1/shortlink_types.go` 草案；`make generate && make manifests`
- 自检：生成的 CRD YAML 里 status 是子资源、有 kubectl get 展示列

#### Day 5 — CRD 设计（status + 校验）
- 任务：定义 status：`readyReplicas` / `observedGeneration` / `conditions`（用 metav1.Condition）；补 defaulting/validation（replicas ≥ 0、滚动策略默认值）
- 产出：types 完整；`make manifests` 后 apply 到 kind 验证 schema
- 自检：apply 非法 CR 被 API server 拒绝

#### Day 6 — Reconciler v0（空壳 + 日志）
- 任务：实现空 Reconcile（打印 req.NamespacedName），注册进 manager
- 产出：apply Shortlink 后 manager 日志出现处理记录
- 自检：删 CR 也能看到 reconcile 记录（证明 watch 生效）

#### Day 7 — 调和 Deployment（创建路径）
- 任务：由 Shortlink 生成期望 Deployment（同名、镜像取自 spec.image、副本取 spec.replicas、探针/资源复用项目一 YAML）；`controllerutil.SetControllerReference` 挂 owner
- 产出：apply CR → Deployment 自动创建
- 自检：删 Deployment 会自动重建（自愈第一根支柱）

#### Day 8 — 幂等与更新路径
- 任务：Diff 检测——镜像/副本/标签变了才 Update；改用 `ctrl.CreateOrUpdate` 重写为幂等
- 产出：reconcile 对「无变化」场景零 API 写入
- 自检：连续触发 10 次 reconcile，resourceVersion 不变

#### Day 9 — 调和 Service + HPA
- 任务：Service（复用项目一 selector/port）；spec 声明 min/max/cpuTarget → 按需 create/delete HPA
- 产出：apply CR → Deployment + Service + HPA 三件套一次成型
- 自检：`kubectl get deploy,svc,hpa` 齐；删 HPA 自动重建

#### Day 10 — 滚动更新（镜像版本变更）
- 任务：image tag 变更 → 更新 Deployment 触发标准滚动；确认 maxUnavailable/maxSurge 生效（有 readinessProbe 才有意义）
- 产出：改 CR spec.image → Deployment 滚动、旧 Pod 平滑下线
- 自检：滚动期间服务零中断（项目一压测脚本验证）

#### Day 11 — 自愈：副本异常
- 任务：处理「实际 ready 数 < spec」场景；Deployment 被删/被改 → 恢复期望；跟踪 observedGeneration
- 产出：`kubectl delete pod` → readyReplicas 短暂下降后恢复
- 自检：30 秒内 readyReplicas 回到 spec.replicas，事件里有恢复记录

#### Day 12 — status 子资源回写
- 任务：reconcile 末尾计算 readyReplicas / conditions（Available、Progressing），写 status 子资源（与 spec 分离，避免写冲突）
- 产出：`kubectl get shortlink -o yaml` 能看到 status；`kubectl get` 有 READY 列
- 自检：删 Pod 后 status 与 Deployment 同步变化；并发写 status 无冲突

#### Day 13 — EventRecorder + 级联回收
- 任务：关键动作发事件（Created / Updated / Reconciled）；验证删 CR → Deployment/Service/HPA 级联删除
- 产出：`kubectl describe shortlink` 有事件时间线
- 自检：删 CR 后子资源 10 秒内清空（ownerReference 生效）

#### Day 14 — envtest 测试基建
- 任务：搭 envtest（setup-envtest 拉 KUBEBUILDER_ASSETS）；写 TestMain 起 test env；写测试 Helper（建测试 CR / 断言 Deployment）
- 产出：`make test` 能起真实 API server 跑 reconcile
- 自检：第一条用例「创建 CR → 断言 Deployment 存在」跑通且 0 失败

#### Day 15 — 单测：创建 / 更新 / 删除三态
- 任务：覆盖：创建 CR→建子资源；改 spec→更新；删 CR→回收
- 产出：3 条核心用例全绿
- 自检：`make test` 全绿；连跑 3 次无 flaky

#### Day 16 — 单测：自愈场景
- 任务：用例：删除 Deployment → 一次 reconcile 自动重建；ready 数不符 → status 修正
- 产出：自愈用例绿
- 自检：故意删子资源后重新 reconcile，断言恢复（「自愈」有可验证证据）

#### Day 17 — 单测：幂等与隔离
- 任务：用例：连续 reconcile 无 diff；两个 CR 并行调和互不污染
- 产出：幂等/隔离用例绿
- 自检：`make test` 全绿，coverage 覆盖 reconcile 核心分支

#### Day 18 — RBAC 最小化 + 安装清单
- 任务：用 `+kubebuilder:rbac` 标记收敛权限（仅 deployments/services/hpa/status 子资源最小集）；精简 config/rbac
- 产出：运行时 ClusterRole 权限可读可审计（不再是 `*/*`）
- 自检：kind 上跑到关键操作，日志无 forbidden

#### Day 19 — v1 真集群联调
- 任务：把项目一短链服务镜像作为被管对象跑完整闭环：apply CR → 三件套 → 滚动 → 自愈 → status/事件
- 产出：`script/demo-v1.sh` 演示脚本
- 自检：按脚本 10 分钟完整复现；录屏 V1

#### Day 20 — v1 收口
- 任务：README 骨架（架构图 / 快速开始 / 已知限制 v1 部分）；git 提交规范整洁（清晰 commit 面试会看）
- 产出：v1 里程碑可用
- 自检：全新 kind 集群从零 pull + apply 跑通

### 阶段 v2（Day 21–30）加分：FaultInjection CRD 声明式故障演练

#### Day 21 — FaultInjection CRD 设计
- 任务：spec：`target`（短链 deployment）/ `faultType(cpu|latency)` / `duration` / 强度参数（cpuPercent、latencyMs）；status：`phase` / `runs`
- 产出：CRD 生成；apply 校验 schema
- 自检：非法字段被 API server 拒绝

#### Day 22 — 注入载体验证
- 任务：定注入方案：CPU 用 stress-ng 打进目标 Pod（同网络命名空间思路）；延迟用 tc netem（后续验证权限）；先做 CPU 原型
- 产出：可手跑的单 Pod 注入脚本
- 自检：注入期间项目一 Grafana CPU 面板可见拉升

#### Day 23 — Reconciler：CPU 注入
- 任务：FI CR → 创建注入 Pod（label 标识来源 FI CR + ownerRef）；`duration` 到期自动删除
- 产出：`kubectl apply` FI → 注入 Pod 出现 → 计时结束自动消失
- 自检：注入期间目标短链 P99 抬升（验证注入有效）

#### Day 24 — Reconciler：延迟注入
- 任务：tc netem 方案（网络延迟 + 所需权限）；校准参数使 P99 稳定上抬
- 产出：latency 类型可用（与 CPU 共用 CR 结构）
- 自检：P99 面板稳定上抬且恢复后回落

#### Day 25 — 状态机 + 幂等
- 任务：FI 生命周期 `Pending → Injecting → Done`；事件记录；重复 apply 幂等；同 target 已在注入时的冲突处理
- 产出：`kubectl get faultinjection` 有 PHASE 列
- 自检：并发 apply 同一 FI 不产生双份注入

#### Day 26 — envtest：FaultInjection 单测
- 任务：用例：apply→注入 Pod 创建；到期→清理；幂等
- 产出：FI 用例绿
- 自检：`make test` 全绿（v1 + v2）

#### Day 27 — 与项目一故障脚本对照
- 任务：项目一的手动故障注入（`?slow=2` / kill pod）全部改写为 FI CR；写对照文档
- 产出：`docs/fault-matrix.md`：手动 vs 声明式 对照表
- 自检：声明式版本逐条覆盖项目一的全部演练场景

#### Day 28 — 全链路联动验证
- 任务：FI 注入 → 项目一监控/告警点燃 → 自愈恢复 → 告警熄灭，全程录屏
- 产出：演练录屏 + 截图（Grafana 面板 + 告警时间戳）
- 自检：能讲清「演练 → 观测 → 告警 → 自愈」完整故事

#### Day 29 — 演示脚本 + README v2
- 任务：`script/demo-v2.sh`（一条 FI apply → 观察 → 恢复）；README 补 v2 章节
- 产出：v2 演示一键可跑
- 自检：换个人按脚本 10 分钟跑通

#### Day 30 — v2 收口
- 任务：整理 FI 边界与已知限制（单节点 kind 限制、tc netem 权限说明）；git 整洁
- 产出：v2 里程碑可用
- 自检：三项目联动故事能对着录屏讲 3 分钟

### 阶段 v3（Day 31–40）弹性：观测性 + 可靠性 + CI/CD

#### Day 31 — Operator 自观测：metrics
- 任务：开启 controller-runtime metrics（:8080）；加自定义指标：reconcile 总数/失败数/时长直方图、工作队列深度
- 产出：`/metrics` 有 operator 专属指标
- 自检：`curl /metrics` 能看到自定义 counter/histogram

#### Day 32 — 事件 + 告警规则
- 任务：关键调和动作发事件；写 PrometheusRule：调和失败率超阈值、队列堆积 → 判断为异常
- 产出：`config/prometheus/` 规则 YAML（项目一监控栈直接消费）
- 自检：人为制造 reconcile 失败，告警 2 分钟内点燃

#### Day 33 — 接入项目一监控
- 任务：ServiceMonitor 让项目一 Prometheus 抓 operator；Grafana 加 operator 面板（调和次数/失败/RR 状态）
- 产出：Grafana Operator 面板
- 自检：面板随演练/自愈活动实时跳动

#### Day 34 — Leader Election
- 任务：manager 开启 LeaderElection（Lease 资源）；验证多副本仅 leader 调和
- 产出：operator 跑 2 副本无冲突（日志可见 leader 身份）
- 自检：kill 主副本，副副本数秒内接管（日志 + 行为双重验证）

#### Day 35 — Validating Webhook
- 任务：`+kubebuilder:webhook` 标记；实现 validateShortlink（replicas 范围 / 镜像非空）；部署（cert-manager 或内置证书）
- 产出：apply 非法 CR 被拒（AdmissionReview）
- 自检：`kubectl apply` replicas=-1 → 拒绝；合法 CR 不受影响

#### Day 36 — Defaulting + webhook 单测
- 任务：Mutating webhook 补默认值（replicas 缺省=2、滚动策略缺省）；envtest 里写 webhook 用例
- 产出：缺省字段 apply 后自动补全；webhook 行为有测试覆盖
- 自检：省略字段 apply → 查看 CR 已被补默认值；测试全绿

#### Day 37 — CI：质量门禁
- 任务：GitHub Actions：golangci-lint + go vet + `make test`（CI 内装 KUBEBUILDER_ASSETS）+ 覆盖率上传
- 产出：PR 必过 lint + 单测门禁；红线为 `make test` 全绿
- 自检：故意引入一个 lint 错误，PR 被拦截

#### Day 38 — CI：构建 + 发布
- 任务：Actions 构建 operator 镜像（ghcr.io）+ Helm chart；打 tag 自动 release 出 artifact
- 产出：push 新 tag → 自动出镜像 + chart
- 自检：用 CI 产物在干净 kind 上 install operator 成功

#### Day 39 — README 全量 + 博客
- 任务：完整 README（架构图 + 三阶段快速开始 + 已知限制）；发布 1 篇技术博客
- 产出：仓库门面完整（含录屏/截图链接）
- 自检：换台机器按 README 从零复现一次

#### Day 40 — 简历 + 最终自检
- 任务：按第二节模板写简历条目；把第三节问答清单过一遍；全量 `make test` + 三阶段 demo 复跑
- 产出：可投递状态（repo + CI 绿 + 博客 + 简历条目 + 录屏）
- 自检：三项目叙事（消费 → 决策 → 平台）连续讲 5 分钟不卡壳

---

## 二、简历怎么写

### 项目板块模板

```
项目名称：基于 kubebuilder 的 Kubernetes Operator —— 短链服务声明式管理 + 故障自愈（个人学习项目）
时间：2026.10 - 2026.11
技术栈：Go · Kubernetes · Kubebuilder/controller-runtime · CRD · envtest · kind · GitHub Actions

- 用 kubebuilder 从零开发 Kubernetes Operator（CRD + 控制循环 + 自愈）：
  控制器调和 Deployment/Service/HPA，Pod 异常 30 秒内自动恢复，镜像变更自动滚动更新
- 设计 FaultInjection CRD 实现声明式故障演练（CPU/延迟注入），
  把项目一的手动故障注入脚本升级为"配置即演练"，与监控告警形成闭环
- 编写 envtest 集成测试（真实 API server）覆盖创建/更新/删除/自愈/幂等场景，
  GitHub Actions 门禁 lint + 单测 + 镜像构建 + release 全自动
- Operator 自身暴露 metrics/事件并接入 Prometheus 告警；
  leader election 保证多副本单主调和；validating webhook 拒绝非法 CR
- 撰写技术博客《用 kubebuilder 写一个会自愈的短链服务》（X 字，平台阅读量 XXX）
```

### 写法要点

1. **用动词开头**：「用 kubebuilder 开发」「设计」「编写」「实现」「撰写」
2. **量化数字**：30 秒自愈、三件套（Deployment/Service/HPA）、用例覆盖几态、博客字数
3. **绑定工具到动作**：不是孤立列「CRD、envtest」，而是「用 envtest 写覆盖自愈的集成测试」「用 ownerReference 实现级联回收」
4. **诚实定位**：标题写「个人学习项目」；Operator 是应届生简历里稀缺条目，但要经得起追问
5. **链接齐全**：GitHub 仓库 + 博客链接 + 录屏/截图（自愈录屏是最硬的证据）

### 简历项目放置位置

- 三个 SRE 项目不用全堆：简历项目区放 **2-3 个**，按「JD 命中度 + 差异化」排序
- Operator 项目（本项目）命中度最高，**放第一位**；burngate（决策引擎）第二位；sre-from-zero（闭环建设）视篇幅取舍
- 三者在简历的叙事线：消费侧（会用工具）→ 决策侧（能造工具）→ 生产侧（能写控制面代码）

### 不要做的事

- ❌ 写「生产级」「支撑百万 QPS」——单节点 kind + 压测 100 QPS，别夸大
- ❌ 写没用过的技术（没碰 Tilt / ArgoCD / OLM / ChaosMesh 就别写，webhook/RBAC 写了就必须能讲）
- ❌ 只写「开发了 CRD」没有行为证据——附 envtest 测试用例截图 + 自愈录屏
- ❌ 伪装成企业项目或实习产出——诚实是零经验候选人最大的加分项

---

## 三、诚实回答清单（面试高频问题 + 诚实回答）

### 问 1：这是真实项目吗？公司项目还是个人项目？
**诚实回答**：
> 个人学习项目。我意识到 Operator 能力是平台/SRE 方向非常稀缺的应届条目，所以投入 2 个月主力时间用 kubebuilder 从零写了一个管理短链服务的 Operator——CRD、控制循环、自愈、envtest 测试、CI 全链路，目标不是模拟生产，而是完整掌握「写控制面代码」这件事。

### 问 2：为什么不直接用 Helm 或 kustomize？
**诚实回答**：
> Helm/kustomize 是「一次性 apply」，交付完就没有人了——Pod 被删了没人拉起、镜像变了没人滚动。Operator 是常驻控制循环，把「期望状态」写进 CRD，控制器持续把现实拉向期望。本项目证明的是后者：把运维经验（自愈、滚动、演练）沉淀成代码而非文档。

### 问 3：为什么不用现成的 Operator 工具或混沌框架（ChaosMesh 等）？
**诚实回答**：
> 两个原因：① 学习目标是理解控制循环本身，Scaffold 我选了官方 kubebuilder（生产同款写法），但 reconcile 逻辑、status 语义、测试都是自己写的；② 故障注入 v2 只做 CPU/延迟两种最小类型，目的是承接项目一的手动脚本、验证「配置即演练」的闭环，ChaosMesh 级的网络分区/磁盘 IO 写进了 Roadmap 扩展项，诚实标注了差距。

### 问 4：和项目一（短链服务）不是重复了吗？
**诚实回答**（核心叙事，必须讲顺）：
> 完全不重复，是视角的反转。项目一是**消费侧**：我部署它、监控它、看告警、手动注入故障；这个是**生产侧**：我写控制面代码，让 K8s 替我把「部署 + 自愈 + 演练」全部自动化。被管对象相同，能力完全不同——这正是「用监控发现问题，用代码把问题预防与自愈固化为平台能力」。

### 问 5：和项目二（burngate）什么关系？
**诚实回答**：
> 项目二是告警决策引擎（Error Budget / burn rate），解决「什么时候该闹」；本项目负责「闹完之后怎么办」——自愈还原、演练验证。三者是一条线：消费（项目一）→ 决策（项目二）→ 执行固化为平台能力（本项目）。

### 问 6：envtest 是什么？为什么不用真集群测试？
**诚实回答**：
> envtest 是 controller-runtime 内置的测试环境：用 etcd + kube-apiserver 起一个真实的 API server，不 mock，控制循环的创建/更新/删除/自愈逻辑都是真跑。区别：真集群验证运行时行为（RBAC、镜像、网络），envtest 把调和的正确性变成可在 CI 里重复的断言——所以 CI 门禁跑 envtest，演示/联调用 kind 真集群。

### 问 7：reconcile 为什么会反复触发？为什么必须幂等？
**诚实回答**：
> watch 到三类变化都会触发：CR 变化、我管理的子资源（Deployment/Service/HPA）变化、以及我写入的 status 变化。所以同一期望状态会被调度器重复调和——reconcile 必须幂等：先 diff 期望与现实，只有不一致才写。我用「连续 10 次 reconcile、resourceVersion 不变」作为幂等的可验证证据。

### 问 8：leader election 怎么实现？为什么要多副本？
**诚实回答**：
> controller-runtime 的 manager 开 LeaderElection 后，用 Lease 锁竞争 leader，只有 leader 跑 Reconcile，其他副本 standby。这样 operator 自身可以被多副本部署（高可用），且不会两个副本同时对同一资源调和导致写冲突。验证方式：kill 主副本，副副本几秒内接管。

### 问 9：validating webhook 部署在哪？有什么用？
**诚实回答**：
> 部署在集群内（operator 同 namespace），由 API server 在 CR 写入前回调（AdmissionReview）。我在 webhook 里校验 replicas 范围、镜像非空等 spec 约束——把错误在「写入」时就拦截，而不是等控制器调和时才发现。写 defaulting 的 Mutating webhook 负责补默认值（replicas 缺省、滚动策略缺省）。

### 问 10：最大的不足是什么？
**诚实回答**（主动列差距是加分项）：
> 1. **无真实流量**：kind 单节点，自愈/滚动验证的是机制正确性，不是真实负载下的行为
> 2. **故障类型有限**：只做了 CPU/延迟注入，ChaosMesh 级的网络分区/磁盘 IO/杀 Pod 未做
> 3. **未上 OLM/OperatorHub**：分发还停留在 Helm chart，没走 OLM bundle
> 4. **无多集群/跨 AZ**：leader election 证明了单集群高可用，跨集群失败域没有
>
> 这些差距我清楚，也是后续扩展方向。

---

## 附：检查清单（项目交付前自检）

- [ ] 全新 kind 集群能从零一键安装 operator + 三阶段 demo 复现
- [ ] `make test`（envtest）全绿，覆盖创建/更新/删除/自愈/幂等场景
- [ ] 自愈演示录屏：`kubectl delete pod` 后 30 秒内 readyReplicas 恢复
- [ ] 滚动更新验证：改 CR image → Deployment 滚动且服务零中断
- [ ] FaultInjection 演示录屏：CPU/延迟注入 → 项目一告警点燃 → 恢复后熄灭
- [ ] `docs/fault-matrix.md`：声明式演练逐条覆盖项目一手动脚本
- [ ] operator `/metrics` 被项目一 Prometheus 抓到，Grafana 有面板
- [ ] leader election 双副本 kill 主验证过（日志 + 行为证据）
- [ ] validating webhook 拒收非法 CR，有 envtest 测试覆盖
- [ ] GitHub Actions 全绿：lint + vet + test + 构建 + release
- [ ] README 有架构图 / 三阶段快速开始 / 已知限制；博客已发布（带链接）
- [ ] 简历条目按模板写好；三项目叙事（消费 → 决策 → 平台）讲 5 分钟不卡壳