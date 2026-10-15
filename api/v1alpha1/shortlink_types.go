/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// 设计原则（Day 4）：
//  1. spec 是「用户声明的期望状态」，只有用户（或准入期的 defaulting webhook）能写；
//     status 是「控制器观测到的真实状态」，只有控制器能写。两者方向不能反。
//  2. spec 里禁止出现「观测类」字段（当前副本数、Pod IP、健康度、时间戳）——那些一律进 status，
//     否则用户声明的期望值与控制器写入的观测值会互相打架（写放大 + 语义混淆）。
//  3. 所有可选字段标 +optional + omitempty；需要区分「未设置」和「显式零值」的字段用指针。
//  4. 校验尽量下沉到 CRD 的 OpenAPI schema（API Server 准入时静态校验，不依赖控制器联网判断），
//     controller-gen 会把下面的 +kubebuilder:validation:* 标记编译进 config/crd/bases/*.yaml。

// ConditionReady 表示 Shortlink 的期望状态已被满足（Deployment 可用且 Service 已就绪）。
// 条件类型常量集中定义，避免控制器与文档里各处字符串手写不一致。
//
// 语义边界（重要）：它回答的是「**消费者现在能不能用**」，**不是**「spec 是否被逐字满足」。
// 控制器内部的失败（字段不可变冲突、权限、并发重试）只有在**导致服务能力缺失**时才算未就绪；
// 「配置与声明不一致、但服务照常可用」那类问题应写进独立的条件（例如将来的 SpecDiverged）
// 与事件里。否则 kubectl wait、告警规则、上游 operator 会把"可用"读成"不可用"，
// 真故障会被这些误报淹没。
const (
	ConditionReady = "Ready"
)

// Ready 条件的 Reason 取值。同样集中定义：Reason 是「机器可分类、可告警」的那一半信息，
// 字符串散落各处时，监控规则和测试断言会随重构悄悄失配。
//
// 分档原则：**不同原因必须给不同 Reason**。曾经所有未就绪都写成 ReplicasNotReady，
// 于是「副本数已达标但 Deployment 不可用」会输出一句自相矛盾的 message
// （expected 1 replicas, 1 reported ready），排障时反而误导。
const (
	// ReasonReplicasReady → Ready=True：期望副本数已全部就绪且 Deployment 可用。
	ReasonReplicasReady = "ReplicasReady"
	// ReasonReplicasNotReady → Ready=False：就绪副本数与期望不符。
	ReasonReplicasNotReady = "ReplicasNotReady"
	// ReasonDeploymentNotAvailable → Ready=False：副本数已达标，但 Deployment 的 Available
	// 条件还不为 True（仍在滚动、或进度超时 ProgressDeadlineExceeded）。
	ReasonDeploymentNotAvailable = "DeploymentNotAvailable"
	// ReasonDeploymentNotFound → Ready=False：子资源 Deployment 尚不存在。
	// 首次创建后缓存未同步时可能短暂出现，下一轮事件会纠正。
	ReasonDeploymentNotFound = "DeploymentNotFound"
	// ReasonDeploymentReconcileFailed / ReasonServiceReconcileFailed → Ready=False：
	// 调和子资源时出错，正由 workqueue 退避重试；此时 status.observedGeneration 不推进。
	ReasonDeploymentReconcileFailed = "DeploymentReconcileFailed"
	ReasonServiceReconcileFailed    = "ServiceReconcileFailed"
)

// ShortlinkSpec 描述短链服务的期望状态。
// 注意：这里只放「用户想要什么」，不放「现在怎么样了」。
type ShortlinkSpec struct {
	// replicas 是期望的 Pod 副本数。
	// 用 *int32 而不是 int32：Go 的零值 0 与「用户没写这个字段」无法区分，
	// 而这两者语义完全不同（0 = 明确缩容到 0，未设置 = 用 default）。
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	Replicas *int32 `json:"replicas,omitempty"`

	// image 是短链服务容器镜像，唯一必填字段。
	// 不给 default 是有意的：镜像版本属于用户意图，控制器不该替用户猜一个版本。
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// port 是容器监听端口，同时作为 Service 的目标端口。
	// +optional
	// +kubebuilder:default=8080
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	// serviceType 决定 Service 如何暴露。
	// 用 Enum 而不是自由字符串：非法值在 kubectl apply 那一刻就被 API Server 拒绝，
	// 不会等到控制器调和时才炸。
	// +optional
	// +kubebuilder:default=ClusterIP
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	ServiceType corev1.ServiceType `json:"serviceType,omitempty"`

	// baseDomain 是短链域名（如 sh.example.com），用于拼接 status.url。
	// Pattern 是域名形态的粗校验，真正的 DNS 可用性只能靠运行时探测。
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	BaseDomain string `json:"baseDomain,omitempty"`

	// env 是追加到容器上的环境变量（同名覆盖镜像内置默认值）。
	// listType=map + listMapKey=name：以 name 为键做「合并」语义，
	// 两个不同的控制器/用户各改一个变量时不会互相覆盖整张列表。
	// +optional
	// +listType=map
	// +listMapKey=name
	Env []corev1.EnvVar `json:"env,omitempty"`

	// resources 是容器资源诉求，直接复用 corev1 类型以避免自造轮子。
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// paused 为 true 时控制器只维护 status、不再推进底层资源（故障演练/排障开关）。
	// +optional
	// +kubebuilder:default=false
	Paused bool `json:"paused,omitempty"`

	// maxSurge / maxUnavailable 控制滚动更新的节奏，语义与 Deployment 完全一致
	//（可写百分比 "25%" 或绝对数 1）。
	//
	// 刻意用指针且**不给 CRD 默认值**：nil 的语义是「本 operator 不声明这个字段」，
	// 于是控制器既不写它、也不比较它，让 Deployment 自己的默认值（25%/25%）生效。
	// 这与 replicas 的指针语义同源——必须区分「用户声明了」与「用户没声明」，
	// 否则控制器会去和 API Server 的默认值较劲，形成永久偏移。
	// +optional
	MaxSurge *intstr.IntOrString `json:"maxSurge,omitempty"`

	// +optional
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`
}

// ShortlinkStatus 描述控制器观测到的真实状态。
// 刻意不提供 Phase 这种自定义字符串阶段：K8s API 惯例是用 metav1.Condition 表达
// 「多维度、可累积、可携带 reason/message/lastTransitionTime」的状态，
// 单个 Phase 字符串既是有限状态机漏抽象，也无法同时表达 Ready 与 Progressing。
type ShortlinkStatus struct {
	// conditions 是标准化的状态条件列表。
	// observedGeneration 写在每条 condition 上，使「这条结论对应哪一版 spec」可被工具链核对。
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// observedGeneration 是控制器最后一次完整处理的 spec 版本号（metadata.generation）。
	// 只有它 == metadata.generation 时，上面的 conditions 才代表「最新 spec 已被处理完」；
	// 否则说明控制器还没追上用户刚提交的改动。这是 spec/status 之间的因果链。
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// readyReplicas 是 Deployment 当前 Ready 的副本数。
	// 该值来自 API Server（Deployment.status.readyReplicas）的真实观测，
	// 不是控制器本地计数——本地计数在重启/多副本 manager 下会漂。
	// +optional
	// +kubebuilder:validation:Minimum=0
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// url 是短链服务的访问入口（ClusterIP 时为集群内地址，NodePort/LoadBalancer 时为对外地址）。
	// +optional
	URL string `json:"url,omitempty"`
}

// Shortlink 是长链转短链服务的声明式描述：写一个 Shortlink，
// 控制器负责把 Deployment / Service 等一组底层资源收敛到该期望状态。
//
// printcolumn 让 `kubectl get shortlinks` / `kubectl get sl` 直接可读，
// 不必每次 -o yaml 翻 status.conditions。
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sl,categories=shortlink
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.readyReplicas`
// Generation / Observed 成对显示，让「控制器是否已追上最新 spec」在 `kubectl get sl` 的默认视图里
// 一眼可见——否则 Ready=True 可能是很久以前那版 spec 的结论（见 status 的因果链说明）。
// 判读方式：Observed < Generation 即为未收敛（冻结 / 调和失败 / 还在处理途中都可能）。
// 更精炼的替代方案是加一个 Synced 条件（Crossplane 的托管资源就是 Synced + Ready 双条件），
// 但那是更大的改动，暂记为候选。
// +kubebuilder:printcolumn:name="Generation",type=integer,JSONPath=`.metadata.generation`
// +kubebuilder:printcolumn:name="Observed",type=integer,JSONPath=`.status.observedGeneration`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Shortlink struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Shortlink
	// +required
	Spec ShortlinkSpec `json:"spec"`

	// status defines the observed state of Shortlink
	// +optional
	Status ShortlinkStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ShortlinkList contains a list of Shortlink
type ShortlinkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Shortlink `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Shortlink{}, &ShortlinkList{})
		return nil
	})
}
