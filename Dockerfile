# 构建 manager 二进制
# 想从其他镜像仓库构建就覆盖 BASE_IMAGE，例如 docker.io/library/golang:1.26
ARG BASE_IMAGE=golang:1.26
FROM ${BASE_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# 拷入 Go Modules 依赖文件
COPY go.mod go.mod
COPY go.sum go.sum
# 本地 replace 的目标必须在 `go mod download` **之前**就位。
# go.mod 里有：replace sigs.k8s.io/controller-runtime => ./third_party/controller-runtime
# 而此时的 /workspace 里只有 go.mod/go.sum（COPY . . 还在后面），Go 会报：
#   reading third_party/controller-runtime/go.mod: no such file or directory
# 所以单独先拷一层 third_party/（只有 Go 源码 + 嵌套 go.mod，见 .dockerignore 的放行规则）。
# 它变化频率远低于业务代码，对依赖缓存层的影响很小。
COPY third_party/ third_party/
# 先把依赖缓存起来，再去构建和拷源码：这样既不用反复重新下载，
# 也不会因为源码改动而让已下载好的依赖层失效
RUN go mod download

# 拷入 Go 源码（哪些不拷交给 .dockerignore 过滤）
COPY . .

# 构建
# GOARCH 故意不给默认值，这样二进制会按“调用这条命令的宿主机”来编。例如在 Apple Silicon M1
# 机器上本地跑 make docker-build 时，docker 的 BUILDPLATFORM 参数会是 linux/arm64，而在 Apple x86
# 上就是 linux/amd64。所以留空就能保证容器和里面跑的二进制是同一个平台。
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager cmd/main.go

# 用 distroless 作为最小化的基础镜像，把 manager 二进制打进去
# 详情见 https://github.com/GoogleContainerTools/distroless
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
