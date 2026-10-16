# Build the manager binary
# Override BASE_IMAGE to build from another registry, e.g. docker.io/library/golang:1.26
ARG BASE_IMAGE=golang:1.26
FROM ${BASE_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# 本地 replace 的目标必须在 `go mod download` **之前**就位。
# go.mod 里有：replace sigs.k8s.io/controller-runtime => ./third_party/controller-runtime
# 而此时的 /workspace 里只有 go.mod/go.sum（COPY . . 还在后面），Go 会报：
#   reading third_party/controller-runtime/go.mod: no such file or directory
# 所以单独先拷一层 third_party/（只有 Go 源码 + 嵌套 go.mod，见 .dockerignore 的放行规则）。
# 它变化频率远低于业务代码，对依赖缓存层的影响很小。
COPY third_party/ third_party/
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build
# the GOARCH has no default value to allow the binary to be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager cmd/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
