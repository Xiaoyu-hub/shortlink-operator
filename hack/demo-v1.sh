#!/usr/bin/env bash
# Day 19 的 v1 演示脚本：一条命令跑完
#   apply CR → 三件套成型 → 删 Deployment 自愈 → 手改副本被拉回 → 改 spec 触发滚动 → 删 CR 级联回收
#
# 用法（在 Git Bash / WSL 里跑，不要用 cmd）：
#   hack/demo-v1.sh                       # 默认集群 k3d-operator-demo，镜像 nginx:1.27
#   SKIP_BUILD=1 hack/demo-v1.sh          # 跳过 manager 镜像重建（镜像与 CRD 已经在集群里）
#   IMAGE=nginx:1.27 REPLICAS=2 hack/demo-v1.sh
#
# 环境变量：
#   CLUSTER        k3d 集群名，默认 operator-demo（对应的 context 是 k3d-operator-demo）
#   NS             CR 所在的命名空间，默认 default
#   NAME           CR 名，默认 demo-v1
#   IMAGE          被管工作负载镜像，默认 nginx:1.27（必须已存在于集群节点，离线可跑）
#   PORT           容器端口，默认 80
#   REPLICAS       期望副本数，默认 1
#   MANAGER_IMAGE  operator 自身镜像，默认 shortlink-operator:dev
#   SKIP_BUILD     1 = 不重建/不导入 manager 镜像
#   TIMEOUT        单步等待上限（秒），默认 180
#
# 为什么要自己写这套检查而不是照 ROADMAP 敲 make：
#   本机 make 来自 chocolatey 的 GnuWin32 版本，跑 kubebuilder 的 Makefile 会在
#   shell 语法与路径上出错（见 skills/kubebuilder-windows-dev）。这里改成
#   kubectl + kubectl kustomize + sed，不依赖 make。
#
# 退出码：0 = 所有检查通过；1 = 有检查失败（失败项会逐条列出，脚本仍跑完不中途退出）。

set -uo pipefail

CLUSTER=${CLUSTER:-operator-demo}
CTX="k3d-$CLUSTER"
NS=${NS:-default}
NAME=${NAME:-demo-v1}
IMAGE=${IMAGE:-nginx:1.27}
PORT=${PORT:-80}
REPLICAS=${REPLICAS:-1}
MANAGER_IMAGE=${MANAGER_IMAGE:-shortlink-operator:dev}
SKIP_BUILD=${SKIP_BUILD:-0}
TIMEOUT=${TIMEOUT:-180}

OPS_NS=shortlink-operator-system
OPS_DEPLOY=shortlink-operator-controller-manager
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PASS=0
FAIL=0
ok()   { PASS=$((PASS + 1)); printf '  [PASS] %s\n' "$*"; }
bad()  { FAIL=$((FAIL + 1)); printf '  [FAIL] %s\n' "$*"; }
step() { printf '\n== %s ==\n' "$*"; }
kc()   { kubectl --context "$CTX" "$@"; }

# 毫秒时间戳；BSD date 不认 %3N 时退化成秒级
now_ms() {
  local t
  t=$(date +%s%3N 2>/dev/null)
  case "$t" in
    *N*) printf '%s000' "$(date +%s)" ;;
    *)   printf '%s' "$t" ;;
  esac
}

# wait_until <描述> <秒> <命令...>：命令返回 0 即通过，并把耗时打出来
wait_until() {
  local desc="$1" limit="$2"
  shift 2
  local start took
  start=$(now_ms)
  while :; do
    if "$@" >/dev/null 2>&1; then
      took=$(($(now_ms) - start))
      ok "$desc（${took}ms）"
      return 0
    fi
    if [ $(($(now_ms) - start)) -gt $((limit * 1000)) ]; then
      bad "$desc（超时 ${limit}s）"
      return 1
    fi
    sleep 0.2
  done
}

# ---- 判定函数（给 wait_until 用）----
ops_ready()      { [ "$(kc -n "$OPS_NS" get deploy "$OPS_DEPLOY" -o jsonpath='{.status.availableReplicas}' 2>/dev/null)" = "1" ]; }
sl_ready()       { [ "$(kc -n "$NS" get sl "$NAME" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" = "$REPLICAS" ]; }
deploy_ready()   { [ "$(kc -n "$NS" get deploy "$NAME" -o jsonpath='{.status.availableReplicas}' 2>/dev/null)" = "$REPLICAS" ]; }
deploy_gone()    { kc -n "$NS" get deploy "$NAME" -o name >/dev/null 2>&1 && return 1 || return 0; }
replicas_back()  { [ "$(kc -n "$NS" get deploy "$NAME" -o jsonpath='{.spec.replicas}' 2>/dev/null)" = "$REPLICAS" ]; }
children_gone()  { deploy_gone && ! kc -n "$NS" get svc "$NAME" -o name >/dev/null 2>&1; }

# ---------------------------------------------------------------------------
step "0. 预检"
# ---------------------------------------------------------------------------
for c in kubectl; do
  command -v "$c" >/dev/null 2>&1 || { echo "缺少命令：$c"; exit 1; }
done
if [ "$SKIP_BUILD" != "1" ]; then
  for c in docker k3d go; do
    command -v "$c" >/dev/null 2>&1 || { echo "缺少命令：$c（或设 SKIP_BUILD=1 跳过镜像重建）"; exit 1; }
  done
fi
if kc get ns "$NS" >/dev/null 2>&1; then
  ok "集群 $CLUSTER 可达（context $CTX）"
else
  bad "集群 $CLUSTER 不可达，先 k3d cluster create $CLUSTER"
  echo "预检失败，退出"
  exit 1
fi

# ---------------------------------------------------------------------------
step "1. 构建并导入 operator 镜像（SKIP_BUILD=$SKIP_BUILD）"
# ---------------------------------------------------------------------------
if [ "$SKIP_BUILD" = "1" ]; then
  ok "已跳过（假定 $MANAGER_IMAGE 已在集群里）"
else
  if IMAGE="$MANAGER_IMAGE" hack/build-local-image.sh "$CLUSTER" >/tmp/demo-v1-build.log 2>&1; then
    ok "镜像 $MANAGER_IMAGE 已导入集群（日志 /tmp/demo-v1-build.log）"
  else
    bad "镜像构建/导入失败，见 /tmp/demo-v1-build.log"
  fi
fi

# ---------------------------------------------------------------------------
step "2. 安装 CRD 并部署 manager"
# ---------------------------------------------------------------------------
if kc apply -k config/crd >/dev/null 2>&1; then
  ok "CRD 已安装（config/crd）"
else
  bad "CRD 安装失败"
fi
# manager 清单里的镜像名是脚手架占位 controller:latest，这里替换成本地镜像
if kubectl kustomize config/default 2>/dev/null | sed "s|controller:latest|$MANAGER_IMAGE|" | kc apply -f - >/dev/null 2>&1; then
  ok "manager 清单已 apply（kustomize build config/default，镜像 $MANAGER_IMAGE）"
else
  bad "manager 清单 apply 失败"
fi
wait_until "manager 可用" "$TIMEOUT" ops_ready

# ---------------------------------------------------------------------------
step "3. apply Shortlink CR，等三件套成型"
# ---------------------------------------------------------------------------
kc -n "$NS" delete sl "$NAME" --ignore-not-found >/dev/null 2>&1
cat <<EOF | kc apply -f - >/dev/null 2>&1
apiVersion: shortlink.dev/v1alpha1
kind: Shortlink
metadata:
  name: $NAME
spec:
  replicas: $REPLICAS
  image: $IMAGE
  port: $PORT
  maxSurge: 1
  maxUnavailable: 0
  serviceType: ClusterIP
  baseDomain: sh.local
  env:
    - name: ROLLOUT_MARKER
      value: v1
EOF
if kc -n "$NS" get sl "$NAME" >/dev/null 2>&1; then
  ok "CR 已创建"
else
  bad "CR 创建失败"
fi
wait_until "Deployment 建起来且 $REPLICAS 个副本就绪" "$TIMEOUT" deploy_ready
wait_until "CR status.readyReplicas = $REPLICAS" "$TIMEOUT" sl_ready
if kc -n "$NS" get svc "$NAME" >/dev/null 2>&1; then
  ok "Service 已创建"
else
  bad "Service 未创建"
fi
echo "  --- kubectl get sl,deploy,svc ---"
kc -n "$NS" get sl "$NAME"
kc -n "$NS" get deploy "$NAME"
kc -n "$NS" get svc "$NAME"

# ---------------------------------------------------------------------------
step "4. 自愈：删掉 Deployment，看控制器重建"
# ---------------------------------------------------------------------------
uid_before=$(kc -n "$NS" get deploy "$NAME" -o jsonpath='{.metadata.uid}' 2>/dev/null)
kc -n "$NS" delete deploy "$NAME" --wait=false >/dev/null 2>&1
wait_until "Deployment 被重建并回到就绪" "$TIMEOUT" deploy_ready
uid_after=$(kc -n "$NS" get deploy "$NAME" -o jsonpath='{.metadata.uid}' 2>/dev/null)
if [ -n "$uid_after" ] && [ "$uid_before" != "$uid_after" ]; then
  ok "重建的是新对象（UID 变了：${uid_before:0:8}… → ${uid_after:0:8}…）"
else
  bad "UID 未变，可能并没有真的重建"
fi

# ---------------------------------------------------------------------------
step "5. 漂移拉回：绕过 CR 手改 Deployment 的副本数"
# ---------------------------------------------------------------------------
kc -n "$NS" scale deploy "$NAME" --replicas=$((REPLICAS + 2)) >/dev/null 2>&1
wait_until "部署副本被拉回 $REPLICAS" "$TIMEOUT" replicas_back
echo "  Deployment 自己报的副本数：$(kc -n "$NS" get deploy "$NAME" -o jsonpath='{.spec.replicas}')"

# ---------------------------------------------------------------------------
step "6. 滚动更新：改 CR 的 spec.env 触发 rollout（maxUnavailable=0）"
# ---------------------------------------------------------------------------
gen_before=$(kc -n "$NS" get sl "$NAME" -o jsonpath='{.metadata.generation}' 2>/dev/null)
marker="v1-$(date +%H%M%S)"
kc -n "$NS" patch sl "$NAME" --type=merge \
  -p "{\"spec\":{\"env\":[{\"name\":\"ROLLOUT_MARKER\",\"value\":\"$marker\"}]}}" >/dev/null 2>&1
if [ "$(kc -n "$NS" get sl "$NAME" -o jsonpath='{.metadata.generation}' 2>/dev/null)" -gt "$gen_before" ] 2>/dev/null; then
  ok "CR generation 递增（$gen_before → $(kc -n "$NS" get sl "$NAME" -o jsonpath='{.metadata.generation}')）"
else
  bad "CR generation 没有递增，滚动可能没被触发"
fi
# 滚动期间每 200ms 采一次 available，看有没有掉到期望值以下
start=$(now_ms)
below=0
samples=0
while :; do
  avail=$(kc -n "$NS" get deploy "$NAME" -o jsonpath='{.status.availableReplicas}' 2>/dev/null)
  [ -z "$avail" ] && avail=0
  samples=$((samples + 1))
  [ "$avail" -lt "$REPLICAS" ] && below=$((below + 1))
  done_roll=$(kc -n "$NS" get deploy "$NAME" -o jsonpath='{range .status.conditions[?(@.type=="Progressing")]}{.reason}{end}' 2>/dev/null)
  updated=$(kc -n "$NS" get deploy "$NAME" -o jsonpath='{.status.updatedReplicas}' 2>/dev/null)
  if [ "$updated" = "$REPLICAS" ] && [ "$(kc -n "$NS" get deploy "$NAME" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" = "$REPLICAS" ]; then
    break
  fi
  if [ $(($(now_ms) - start)) -gt $((TIMEOUT * 1000)) ]; then
    bad "滚动超时（最后 Progressing=$done_roll）"
    break
  fi
  sleep 0.2
done
if [ "$below" = "0" ]; then
  ok "滚动期间 $samples 次采样里，available 从未低于 $REPLICAS"
else
  bad "滚动期间有 $below/$samples 次采样 available < $REPLICAS"
fi
echo "  滚动耗时：$((($(now_ms) - start) / 1000))s，采样 $samples 次（每轮含一次 kubectl 往返，约 200-400ms/次）"
if [ "$samples" -lt 5 ]; then
  echo "  [NOTE] 采样不足 5 次，分辨率不够下「零中断」的结论——只能说明本次滚动没被采到中断。"
  echo "         要下结论得把采样密度提上去（Day 10 用的常驻探针约 10 req/s），见 docs/day10。"
fi
if [ "$(kc -n "$NS" get deploy "$NAME" -o jsonpath='{.spec.template.spec.containers[0].env[0].value}' 2>/dev/null)" = "$marker" ]; then
  ok "Pod 模板已带上新 marker（$marker）"
else
  bad "Pod 模板没带上新 marker"
fi

# ---------------------------------------------------------------------------
step "7. status 与事件"
# ---------------------------------------------------------------------------
echo "  --- kubectl get sl $NAME ---"
kc -n "$NS" get sl "$NAME"
echo "  --- status.conditions ---"
kc -n "$NS" get sl "$NAME" -o jsonpath='{range .status.conditions[*]}{.type}={.status} reason={.reason} obsGen={.observedGeneration} msg={.message}{"\n"}{end}'
# events.k8s.io 不支持 involvedObject.name 字段选择器，要用 regarding.name
kc -n "$NS" get events.v1.events.k8s.io --field-selector regarding.name="$NAME" \
  -o custom-columns=REASON:.reason,ACTION:.action,CTRL:.reportingController,NOTE:.note 2>/dev/null |
  awk 'NR==1 || $3=="shortlink"'

# ---------------------------------------------------------------------------
step "8. 级联回收：删 CR，子资源应被 GC 带走"
# ---------------------------------------------------------------------------
start=$(now_ms)
kc -n "$NS" delete sl "$NAME" --wait=false >/dev/null 2>&1
wait_until "Deployment 与 Service 都被回收" "$TIMEOUT" children_gone
echo "  Deployment 还在吗：$(kc -n "$NS" get deploy "$NAME" -o name 2>&1 | head -n 1)"
echo "  Service    还在吗：$(kc -n "$NS" get svc "$NAME" -o name 2>&1 | head -n 1)"
# 子对象被 GC 带走后，Pod 还会在 Terminating 停留一会儿（宽限期），这是优雅终止而非孤儿
for _ in $(seq 1 50); do
  left=$(kc -n "$NS" get pods -l app.kubernetes.io/instance="$NAME" --no-headers 2>/dev/null | wc -l)
  [ "$left" = "0" ] && break
  sleep 0.2
done
echo "  残留 Pod：${left:-?} 个（宽限期内 Terminating 属正常，超过 30s 才是孤儿）"

# ---------------------------------------------------------------------------
step "汇总"
# ---------------------------------------------------------------------------
echo "  PASS=$PASS FAIL=$FAIL"
if [ "$FAIL" -gt 0 ]; then
  echo "  有检查未通过，按上面 [FAIL] 行逐条排查"
  exit 1
fi
echo "  v1 闭环演示通过：apply → 三件套 → 自愈 → 拉回偏移 → 滚动 → 级联回收"
