# 构建/推送镜像时使用的镜像 URL
IMG ?= controller:latest
# YEAR 定义用于替换样板文件头部 YEAR 占位符的年份值。
YEAR ?= $(shell date +%Y)

# 取得当前 golang 的安装路径（默认是 GOPATH/bin，除非设了 GOBIN）
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL 定义构建镜像时用的容器工具。
# 注意：下面的目标命令只在脚手架默认的 Docker 上验证过。
# 你想换成其他工具（比如 podman）也可以，但需要自己验证一遍。
CONTAINER_TOOL ?= docker

# 把 SHELL 设成 bash，recipe 里才能用 bash 的语法。
# 这些选项的意思是：某行命令返回非零、或管道中任一环节失败，就当成失败退出。
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ 通用

# help 目标会把所有目标及其描述打印出来，并按分类分组。
# 分类由 '##@' 后面的文字表示，目标描述由 '##' 后面的文字表示。
# 那段 awk 负责把本次 make 载入的全部 Makefile 读一遍，找出形如
# 「xyz: ## something」的行，再把目标名与帮助文字排好版。之后，
# 如果碰到 「##@ something」这样的行，就把 something 打成分类标题。
# 关于终端格式化用的 ANSI 控制字符的用法：
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# 关于 awk 命令的用法：
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## 显示本帮助。
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ 开发

.PHONY: manifests
manifests: controller-gen ## 生成 WebhookConfiguration、ClusterRole 与 CustomResourceDefinition 对象。
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook $(PKG_PATHS) output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## 生成包含 DeepCopy、DeepCopyInto、DeepCopyObject 方法实现的代码。
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) $(PKG_PATHS)

.PHONY: fmt
fmt: ## 对代码跑 go fmt。
	go fmt ./...

.PHONY: vet
vet: ## 对代码跑 go vet。
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## 跑测试。
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# TODO(user)：要换掉 e2e 测试的载体，就改 'tests/e2e' 下的配置。
# 默认配置假设 Kind 已装好，并在本地构建/加载 Manager 的 Docker 镜像。
# kubectl kuberc 默认关闭以保证测试隔离；想开启就设：
# - KUBECTL_KUBERC=true
# CertManager 默认会装；想跳过就设：
# - CERT_MANAGER_INSTALL_SKIP=true
KIND_CLUSTER ?= shortlink-operator-test-e2e

.PHONY: setup-test-e2e
setup-test-e2e: ## 如果 kind 集群不存在，就为 e2e 测试建一个
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) ;; \
	esac

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## 跑 e2e 测试。需要一个用 Kind 搭的隔离环境。
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) go test -tags=e2e ./test/e2e/ -v -ginkgo.v
	$(MAKE) cleanup-test-e2e

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## 拆掉 e2e 测试用的 kind 集群
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: lint
lint: golangci-lint ## 跑 golangci-lint 静态检查
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## 跑 golangci-lint 并自动修复问题
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## 校验 golangci-lint 的配置
	"$(GOLANGCI_LINT)" config verify

##@ 构建

.PHONY: build
build: manifests generate fmt vet ## 构建 manager 二进制。
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## 直接在本机跑一个控制器。
	go run ./cmd/main.go

# 想为其他平台构建 manager 镜像，可以使用 --platform 参数。
# （即 docker build --platform linux/arm64）但前提是开了 docker BuildKit。
# 详情见：https://docs.docker.com/develop/develop-images/build_enhancements/
# 也可以覆盖 BASE_IMAGE 从其他镜像仓库构建，例如
# make docker-build IMG=<img> BASE_IMAGE=docker.io/library/golang:1.26
.PHONY: docker-build
docker-build: ## 构建 manager 的 docker 镜像。
	$(CONTAINER_TOOL) build $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) -t ${IMG} .

.PHONY: docker-push
docker-push: ## 推送 manager 的 docker 镜像。
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS 定义 manager 镜像要构建哪些目标平台，用来支持多架构。
# （例如 make docker-buildx IMG=myregistry/mypoperator:0.0.1）。用这个选项前需要：
# - 能用 docker buildx。详情见：https://docs.docker.com/build/buildx/
# - 已开启 BuildKit。详情见：https://docs.docker.com/develop/develop-images/build_enhancements/
# - 能把镜像推到你自己的仓库（若 IMG=<myregistry/image:<tag>> 设得不对，导出会失败）
# 如果交付物需要兼容多种平台，建议采用这个选项。
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## 为 manager 构建并推送支持多平台的 docker 镜像
	# 复制现有 Dockerfile，把 --platform=${BUILDPLATFORM} 插进 Dockerfile.cross，同时保留原 Dockerfile 不动
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name shortlink-operator-builder
	$(CONTAINER_TOOL) buildx use shortlink-operator-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm shortlink-operator-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## 生成一份把 CRD 与 Deployment 合在一起的 YAML。
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default > dist/install.yaml

##@ 部署

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## 把 CRD 装进 ~/.kube/config 指定的 K8s 集群。
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## 从 ~/.kube/config 指定的 K8s 集群卸载 CRD。删除期间若想忽略 not found 错误，传 ignore-not-found=true。
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## 把控制器部署到 ~/.kube/config 指定的 K8s 集群。
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## 从 ~/.kube/config 指定的 K8s 集群卸掉控制器。删除期间若想忽略 not found 错误，传 ignore-not-found=true。
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ 依赖

## 依赖安装位置
# 本机实测（Go 1.27.1 + controller-tools v0.22.0）：`paths="./..."` 会把「模块根目录」也当成一个包去加载，
# 而 kubebuilder 布局的模块根没有 .go 文件，于是报 `-: no Go files in <repo root>`（新建的空仓库同样复现）。
# 同理 `paths=./api/...` 会把 `api/` 目录自身当包加载，而 api/ 下只有 v1alpha1/ 子包、没有 .go 文件，
# 于是报 `-: no Go files in <repo>/api`（Day 4 新建 api/ 时实测踩到）。
# 因此改为枚举「真正含 .go 文件」的目录，逐个用 dir 精确加载（不带 /...）。
# 注意：make 本身要在 MSYS2 / Git Bash 里跑（Makefile 里有 pwd、mkdir -p、find 管道）。详见 docs/day03-local-cluster-and-scaffold.md
PKG_PATHS := $(foreach d,$(shell find cmd api internal pkg -type f -name '*.go' 2>/dev/null | xargs -n1 dirname 2>/dev/null | sort -u),paths=./$(d))

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## 工具二进制
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## 工具版本
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.22.0

#ENVTEST_VERSION：setup-envtest 要用的 controller-runtime 版本，从 go.mod 里读
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v")

#ENVTEST_K8S_VERSION：部署 ENVTEST 二进制时使用的 Kubernetes 版本（例如 1.31）
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.13.1
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## 需要的话，把 kustomize 下载到本地。
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## 需要的话，把 controller-gen 下载到本地。
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## 把 ENVTEST 需要的二进制下载到本地 bin 目录。
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## 需要的话，把 setup-envtest 下载到本地。
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## 需要的话，把 golangci-lint 下载到本地。
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@test -f .custom-gcl.yml && { \
		echo "Building custom golangci-lint with plugins..." && \
		$(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	} || true

# go-install-tool：当目标二进制不存在时，用 'go install' 把某个包装成指定名字的二进制放到指定位置
# $1 - 二进制目标路径（含文件名）
# $2 - 可安装的包地址
# $3 - 包的特定版本
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

# 取 go.mod 里某模块的版本号。replace 有两种形态，必须都覆盖：
#   1) 带版本的替换（=> other/module v1.2.3）：用 .Replace.Version；
#   2) 指向本地目录的替换（=> ./third_party/xxx）：.Replace.Version 是空串，
#      必须回退到 .Version（require 行声明的版本），否则推导出空值，
#      setup-envtest 会以 "version must not be empty" 失败（本机与 CI 都会中招）。
# 注意：注释不要写进 define 体内 —— define 会原样保留，注释会泄漏进 shell 命令。
define gomodver
$(shell go list -m -f '{{if and .Replace .Replace.Version}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
