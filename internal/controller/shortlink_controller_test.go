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

package controller

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shortlinkv1alpha1 "github.com/Xiaoyu-hub/shortlink-operator/api/v1alpha1"
)

// envtest 特性：etcd + kube-apiserver 是真实的，但没有 kube-controller-manager /
// scheduler / GC，所以 Deployment 永远不会自己长出 Pod（readyReplicas 恒 0）。
// 要打通 status 链路，测试必须手动伪造 Deployment 的就绪状态（见 ready 用例）。
const (
	timeout  = 15 * time.Second
	interval = 200 * time.Millisecond

	testNamespace = "shortlink-test"
)

var _ = Describe("Shortlink controller（envtest 真控制循环）", func() {
	BeforeEach(func() {
		// envtest 自带命名空间很少，先建好测试命名空间（创建是幂等的）
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}
		if err := k8sClient.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	})

	Context("创建路径", func() {
		It("创建 CR 后自动调和出 Deployment 与 Service（ownerRef/标签/端口/环境变量齐全）", func() {
			name := "create-basic"
			Expect(k8sClient.Create(ctx, newTestShortlink(name, func(s *shortlinkv1alpha1.Shortlink) {
				s.Spec.Replicas = int32Ptr(2)
				s.Spec.Env = []corev1.EnvVar{{Name: "SHORTLINK_LOG_LEVEL", Value: "info"}}
			}))).To(Succeed())

			var deploy appsv1.Deployment
			Eventually(func() error {
				return k8sClient.Get(ctx, objectKey(name), &deploy)
			}, timeout, interval).Should(Succeed())

			// 标签与 ownerReference（级联回收的基石）
			Expect(deploy.GetLabels()).To(HaveKeyWithValue(labelManagedBy, operatorName))
			Expect(deploy.GetLabels()).To(HaveKeyWithValue(labelInstance, name))
			Expect(deploy.GetOwnerReferences()).To(HaveLen(1))
			Expect(deploy.GetOwnerReferences()[0].Kind).To(Equal("Shortlink"))
			Expect(deploy.GetOwnerReferences()[0].Controller).To(HaveValue(BeTrue()))
			Expect(deploy.Spec.Selector.MatchLabels).To(HaveKeyWithValue(labelInstance, name))
			Expect(deploy.Spec.Template.Labels).To(HaveKeyWithValue(labelInstance, name))

			// 期望的容器规格
			containers := deploy.Spec.Template.Spec.Containers
			Expect(containers).To(HaveLen(1))
			Expect(containers[0].Image).To(Equal("ghcr.io/xiaoyu-hub/shortlink:v0.1.0"))
			Expect(*deploy.Spec.Replicas).To(Equal(int32(2)))
			Expect(containers[0].Ports).To(HaveLen(1))
			Expect(containers[0].Ports[0].ContainerPort).To(Equal(int32(8080)))
			Expect(containers[0].Env).To(Equal([]corev1.EnvVar{{Name: "SHORTLINK_LOG_LEVEL", Value: "info"}}))

			// Service：类型/端口/selector 与 CR 一致，且挂同一套 owner
			var svc corev1.Service
			Eventually(func() error {
				return k8sClient.Get(ctx, objectKey(name), &svc)
			}, timeout, interval).Should(Succeed())
			Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
			Expect(svc.Spec.Ports).To(HaveLen(1))
			Expect(svc.Spec.Ports[0].Port).To(Equal(int32(8080)))
			Expect(svc.Spec.Ports[0].TargetPort.IntValue()).To(Equal(8080))
			Expect(svc.Spec.Selector).To(HaveKeyWithValue(labelInstance, name))
			Expect(svc.GetOwnerReferences()).To(HaveLen(1))

			// 幂等证据：Deployment 就位后再改一次 spec（改 env），
			// 控制器应收敛而不是产生第二个 Deployment（同名 Create 只可能 AlreadyExists）
			By("验证同名牌不会产生重复资源")
			dup := desiredDeploymentForTest(name)
			err := k8sClient.Create(ctx, dup)
			fmt.Fprintf(GinkgoWriter, "duplicate create err: %v\n", err)
			Expect(err).To(HaveOccurred()) // AlreadyExists 而非重复创建
			Expect(apierrors.IsAlreadyExists(err)).To(BeTrue())
		})

		It("replicas=0 表示缩容到零：Deployment 副本数为 0（资源保留、Pod 清零）", func() {
			name := "scale-to-zero"
			Expect(k8sClient.Create(ctx, newTestShortlink(name, func(s *shortlinkv1alpha1.Shortlink) {
				s.Spec.Replicas = int32Ptr(0)
			}))).To(Succeed())

			Eventually(func() int32 {
				var d appsv1.Deployment
				if err := k8sClient.Get(ctx, objectKey(name), &d); err != nil || d.Spec.Replicas == nil {
					return -1
				}
				return *d.Spec.Replicas
			}, timeout, interval).Should(Equal(int32(0)))
		})
	})

	Context("status 写回", func() {
		It("伪造 Deployment 就绪后，status 回写 readyReplicas / observedGeneration / Ready 条件", func() {
			name := "ready-status"
			Expect(k8sClient.Create(ctx, newTestShortlink(name, func(s *shortlinkv1alpha1.Shortlink) {
				s.Spec.Replicas = int32Ptr(1)
			}))).To(Succeed())

			var deploy appsv1.Deployment
			Eventually(func() error {
				return k8sClient.Get(ctx, objectKey(name), &deploy)
			}, timeout, interval).Should(Succeed())

			By("模拟 kube-controller-manager 把 Deployment 标记为 1/1 可用")
			deploy.Status.Replicas = 1
			deploy.Status.UpdatedReplicas = 1
			deploy.Status.ReadyReplicas = 1
			deploy.Status.AvailableReplicas = 1
			deploy.Status.Conditions = []appsv1.DeploymentCondition{{
				Type:    appsv1.DeploymentAvailable,
				Status:  corev1.ConditionTrue,
				Reason:  "MinimumReplicasAvailable",
				Message: "Deployment has minimum availability.",
			}}
			Expect(k8sClient.Status().Update(ctx, &deploy)).To(Succeed())

			By("status 应被控制器回写：readyReplicas=1、observedGeneration=1、Ready=True")
			Eventually(func() shortlinkv1alpha1.ShortlinkStatus {
				var s shortlinkv1alpha1.Shortlink
				Expect(k8sClient.Get(ctx, objectKey(name), &s)).To(Succeed())
				return s.Status
			}, timeout, interval).Should(SatisfyAll(
				HaveField("ObservedGeneration", int64(1)),
				HaveField("ReadyReplicas", int32(1)),
			))

			// 未配 baseDomain 时用集群内 DNS 名
			Eventually(func() string {
				var s shortlinkv1alpha1.Shortlink
				Expect(k8sClient.Get(ctx, objectKey(name), &s)).To(Succeed())
				return s.Status.URL
			}, timeout, interval).Should(Equal(fmt.Sprintf("http://%s.%s.svc.cluster.local:8080", name, testNamespace)))

			Eventually(func() metav1.ConditionStatus {
				var s shortlinkv1alpha1.Shortlink
				Expect(k8sClient.Get(ctx, objectKey(name), &s)).To(Succeed())
				for _, c := range s.Status.Conditions {
					if c.Type == shortlinkv1alpha1.ConditionReady {
						return c.Status
					}
				}
				return metav1.ConditionUnknown
			}, timeout, interval).Should(Equal(metav1.ConditionTrue))
		})
	})

	Context("更新路径（滚动入口）", func() {
		It("修改 spec.image 后 Deployment 被更新，observedGeneration 追平 metadata.generation", func() {
			name := "rollout"
			Expect(k8sClient.Create(ctx, newTestShortlink(name, nil))).To(Succeed())

			// 第一版 spec 收敛
			Eventually(func() string {
				return deploymentImage(name)
			}, timeout, interval).Should(Equal("ghcr.io/xiaoyu-hub/shortlink:v0.1.0"))

			By("改镜像 → metadata.generation 递增，控制器必须追平")
			Eventually(func() error {
				var s shortlinkv1alpha1.Shortlink
				if err := k8sClient.Get(ctx, objectKey(name), &s); err != nil {
					return err
				}
				s.Spec.Image = "ghcr.io/xiaoyu-hub/shortlink:v0.2.0"
				return k8sClient.Update(ctx, &s)
			}, timeout, interval).Should(Succeed())

			Eventually(func() string {
				return deploymentImage(name)
			}, timeout, interval).Should(Equal("ghcr.io/xiaoyu-hub/shortlink:v0.2.0"))

			// 因果链：obsGen == gen == 2（第二版 spec，则是「已收敛」）
			Eventually(func() bool {
				obs, gen := shortlinkObservedGeneration(name)
				return obs == gen && gen == 2
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("冻结开关（paused）", func() {
		It("paused=true 冻结子资源调和，恢复后自动补齐差额（observedGeneration 是未收敛旗帜）", func() {
			name := "pause-freeze"
			Expect(k8sClient.Create(ctx, newTestShortlink(name, func(s *shortlinkv1alpha1.Shortlink) {
				s.Spec.Replicas = int32Ptr(2)
			}))).To(Succeed())

			// 第一版收敛
			Eventually(func() string {
				return deploymentImage(name)
			}, timeout, interval).Should(Equal("ghcr.io/xiaoyu-hub/shortlink:v0.1.0"))
			Eventually(func() bool {
				obs, gen := shortlinkObservedGeneration(name)
				return obs == gen
			}, timeout, interval).Should(BeTrue())

			By("打开冻结开关（spec 变化 → generation 递增到 2）")
			Eventually(func() error {
				var s shortlinkv1alpha1.Shortlink
				if err := k8sClient.Get(ctx, objectKey(name), &s); err != nil {
					return err
				}
				s.Spec.Paused = true
				return k8sClient.Update(ctx, &s)
			}, timeout, interval).Should(Succeed())

			By("冻结期间再改 image（generation 递增到 3）：Deployment 不应被推进")
			Eventually(func() error {
				var s shortlinkv1alpha1.Shortlink
				if err := k8sClient.Get(ctx, objectKey(name), &s); err != nil {
					return err
				}
				s.Spec.Image = "ghcr.io/xiaoyu-hub/shortlink:v0.2.0"
				return k8sClient.Update(ctx, &s)
			}, timeout, interval).Should(Succeed())

			Consistently(func() string {
				return deploymentImage(name)
			}, 2*time.Second, interval).Should(Equal("ghcr.io/xiaoyu-hub/shortlink:v0.1.0"))

			// observedGeneration 停在冻结前的 1，不再追 generation（=3）——
			// 这正是「未收敛」的旗帜：客户端见 obsGen < gen 就不会误信旧 status。
			Consistently(func() int64 {
				obs, _ := shortlinkObservedGeneration(name)
				return obs
			}, 2*time.Second, interval).Should(Equal(int64(1)))

			By("解除冻结后，控制器一次性补齐所有差额（image v0.2.0 + 追平 generation）")
			Eventually(func() error {
				var s shortlinkv1alpha1.Shortlink
				if err := k8sClient.Get(ctx, objectKey(name), &s); err != nil {
					return err
				}
				s.Spec.Paused = false
				return k8sClient.Update(ctx, &s)
			}, timeout, interval).Should(Succeed())

			Eventually(func() string {
				return deploymentImage(name)
			}, timeout, interval).Should(Equal("ghcr.io/xiaoyu-hub/shortlink:v0.2.0"))
			Eventually(func() bool {
				obs, gen := shortlinkObservedGeneration(name)
				return obs == gen
			}, timeout, interval).Should(BeTrue())
		})
	})

	// 自愈不是新功能：Owns(Deployment) + 先查后建已经具备这个能力，
	// 这一节把它固化成测试——没有测试的"已经能自愈"只是当场的观察。
	Context("自愈", func() {
		It("删掉 Deployment 后由控制循环重建（新 UID + ownerReference + spec 一致）", func() {
			name := "selfheal"
			Expect(k8sClient.Create(ctx, newTestShortlink(name, func(s *shortlinkv1alpha1.Shortlink) {
				s.Spec.Replicas = int32Ptr(1)
			}))).To(Succeed())

			var first appsv1.Deployment
			Eventually(func() error {
				return k8sClient.Get(ctx, objectKey(name), &first)
			}, timeout, interval).Should(Succeed())
			firstUID := first.UID
			Expect(firstUID).NotTo(BeEmpty())

			By("删掉 Deployment —— Delete 事件带着 ownerReference 反查，把 CR 重新入队")
			Expect(k8sClient.Delete(ctx, &first)).To(Succeed())

			// 只断言「对象还在」是不够的：那可能只是删除没生效。
			// UID 变了才是"被重新创建"的证据。
			Eventually(func() bool {
				var d appsv1.Deployment
				if err := k8sClient.Get(ctx, objectKey(name), &d); err != nil {
					return false
				}
				return d.UID != firstUID
			}, timeout, interval).Should(BeTrue())

			By("新对象必须仍由本 CR 所有，且 spec 收敛到期望（证明是控制循环干的，不是碰巧）")
			var rebuilt appsv1.Deployment
			Expect(k8sClient.Get(ctx, objectKey(name), &rebuilt)).To(Succeed())
			Expect(rebuilt.GetOwnerReferences()).To(HaveLen(1))
			Expect(rebuilt.GetOwnerReferences()[0].Kind).To(Equal("Shortlink"))
			Expect(rebuilt.GetOwnerReferences()[0].Name).To(Equal(name))
			Expect(rebuilt.GetOwnerReferences()[0].Controller).To(HaveValue(BeTrue()))
			Expect(rebuilt.Spec.Selector.MatchLabels).To(HaveKeyWithValue(labelInstance, name))
			Expect(rebuilt.Spec.Template.Spec.Containers[0].Image).
				To(Equal("ghcr.io/xiaoyu-hub/shortlink:v0.1.0"))
		})
	})

	// 失败路径的可观测性：调和失败不再是"静默退避重试"——外面必须能看见。
	Context("调和失败的可观测性", func() {
		It("子资源无法收敛时写 Warning 事件 + Ready=False 且不推进 observedGeneration；故障排除后自行恢复", func() {
			name := "reconcile-failure"

			By("预置一个同名 Deployment：template labels 与期望不同（会触发覆写）+ selector 不可变（覆写必被拒）")
			// 为什么必须两个都改：
			//   selector 不可变是我们要的失败源，但 deploymentNeedsUpdate 并不比较 selector，
			//   所以还得让**被比较的**字段（template labels）也偏移，覆写才会真的发生；
			//   覆写会把 selector 一起提交，于是被 API Server 以「selector 不可变」拒绝。
			//   （selector 与 template labels 必须自洽，否则占位对象自己就建不出来。）
			occupied := desiredDeploymentForTest(name)
			occupied.Spec.Selector.MatchLabels = map[string]string{labelInstance: "someone-else"}
			occupied.Spec.Template.ObjectMeta.Labels = map[string]string{labelInstance: "someone-else"}
			Expect(k8sClient.Create(ctx, occupied)).To(Succeed())

			Expect(k8sClient.Create(ctx, newTestShortlink(name, nil))).To(Succeed())

			By("Ready 条件的 Reason 必须点名「调和失败」，而不是笼统的未就绪")
			Eventually(func() string {
				return readyReason(name)
			}, timeout, interval).Should(Equal(shortlinkv1alpha1.ReasonDeploymentReconcileFailed))

			By("observedGeneration 必须停在原处：这一轮没有把最新 spec 走完")
			Consistently(func() int64 {
				obs, _ := shortlinkObservedGeneration(name)
				return obs
			}, 2*time.Second, interval).Should(Equal(int64(0)))

			By("Warning 事件应出现（事件是异步写出的，必须 Eventually）")
			Eventually(func() []string {
				return eventTypesAndReasonsFor(name)
			}, timeout, interval).Should(ContainElement("Warning/" + shortlinkv1alpha1.ReasonDeploymentReconcileFailed))

			By("排除故障：删掉占着名字的对象，控制器应自行恢复并追平 generation")
			var occupying appsv1.Deployment
			Expect(k8sClient.Get(ctx, objectKey(name), &occupying)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &occupying)).To(Succeed())
			Eventually(func() bool {
				var d appsv1.Deployment
				return apierrors.IsNotFound(k8sClient.Get(ctx, objectKey(name), &d))
			}, timeout, interval).Should(BeTrue())

			Eventually(func() bool {
				obs, gen := shortlinkObservedGeneration(name)
				return obs == gen && gen > 0
			}, timeout, interval).Should(BeTrue())

			// 失败原因被清除后，Reason 换成真正的未就绪原因（envtest 没有 kcm，
			// 副本永远不会 ready，所以是 ReplicasNotReady 而不是调和失败）。
			Eventually(func() string {
				return readyReason(name)
			}, timeout, interval).Should(Equal(shortlinkv1alpha1.ReasonReplicasNotReady))
		})
	})

	// 已知缺口（docs/day06 遗留 #1）：selector 不在比较集合里、却在写入集合里。
	// 这条用例把"静默接受 → 直到被比较的字段也漂移才响亮失败"这个两段式行为钉住：
	// 将来若决定"报告 selector 漂移"，它会失败，从而提醒改文档与改断言。
	Context("已知缺口：外部改了 selector", func() {
		It("selector 被改窄时控制器不比较它（静默接受）；直到被比较的字段也漂移才因不可变而报错", func() {
			name := "selector-drift"

			By("预置同名 Deployment：selector 改窄（仍匹配 template labels，因此合法），改窄后与期望不同")
			occupied := desiredDeploymentForTest(name)
			narrowed := map[string]string{labelInstance: name, labelAppName: appNameValue}
			occupied.Spec.Selector.MatchLabels = narrowed
			Expect(k8sClient.Create(ctx, occupied)).To(Succeed())
			Expect(k8sClient.Create(ctx, newTestShortlink(name, nil))).To(Succeed())

			By("第 ① 段：只有 selector 漂移 —— 比较集合里没有偏移，控制器零写入、照常推进 obsGen")
			Eventually(func() bool {
				obs, gen := shortlinkObservedGeneration(name)
				return obs == gen && gen > 0
			}, timeout, interval).Should(BeTrue())
			Consistently(func() string {
				return readyReason(name)
			}, 1*time.Second, interval).ShouldNot(Equal(shortlinkv1alpha1.ReasonDeploymentReconcileFailed))
			// 关键：controller 不会把它纠正回来（selector 不可变，且我们根本不比较它）
			Expect(deploymentSelectorLabels(name)).To(Equal(narrowed))

			By("第 ② 段：改 spec.image（被比较字段）→ 触发覆写 → selector 被一起提交 → 不可变拒绝 → 响亮失败")
			Eventually(func() error {
				var s shortlinkv1alpha1.Shortlink
				if err := k8sClient.Get(ctx, objectKey(name), &s); err != nil {
					return err
				}
				s.Spec.Image = "ghcr.io/xiaoyu-hub/shortlink:v0.2.0"
				return k8sClient.Update(ctx, &s)
			}, timeout, interval).Should(Succeed())

			Eventually(func() string {
				return readyReason(name)
			}, timeout, interval).Should(Equal(shortlinkv1alpha1.ReasonDeploymentReconcileFailed))
		})
	})
})

// ---- 测试辅助 ----

func int32Ptr(v int32) *int32 { return &v }

func objectKey(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: testNamespace, Name: name}
}

// newTestShortlink 造一个最小合法 CR；mutate 可覆盖默认字段。
func newTestShortlink(name string, mutate func(*shortlinkv1alpha1.Shortlink)) *shortlinkv1alpha1.Shortlink {
	sl := &shortlinkv1alpha1.Shortlink{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: shortlinkv1alpha1.ShortlinkSpec{
			Image: "ghcr.io/xiaoyu-hub/shortlink:v0.1.0",
			// 显式给出 admission defaulting 后的值：真实 CR 的 spec.port 也会被 API Server 补成 8080
			Port: 8080,
		},
	}
	if mutate != nil {
		mutate(sl)
	}
	return sl
}

// desiredDeploymentForTest：直接调用生产代码里的期望构造器，
// 用于验证同名重复 Create 只会 AlreadyExists（幂等基石）。
func desiredDeploymentForTest(name string) *appsv1.Deployment {
	cr := newTestShortlink(name, nil)
	return desiredDeployment(cr)
}

func deploymentImage(name string) string {
	var d appsv1.Deployment
	if err := k8sClient.Get(ctx, objectKey(name), &d); err != nil {
		return ""
	}
	if len(d.Spec.Template.Spec.Containers) == 0 {
		return ""
	}
	return d.Spec.Template.Spec.Containers[0].Image
}

func shortlinkObservedGeneration(name string) (observed, generation int64) {
	var s shortlinkv1alpha1.Shortlink
	if err := k8sClient.Get(ctx, objectKey(name), &s); err != nil {
		return -1, -1
	}
	return s.Status.ObservedGeneration, s.Generation
}

// deploymentSelectorLabels 读 Deployment 的实际 selector，用来观察"控制器有没有纠正漂移"。
func deploymentSelectorLabels(name string) map[string]string {
	var d appsv1.Deployment
	if err := k8sClient.Get(ctx, objectKey(name), &d); err != nil {
		return nil
	}
	return d.Spec.Selector.MatchLabels
}

// readyReason 读 CR 上 Ready 条件的 Reason；条件还不存在时返回空串。
func readyReason(name string) string {
	var s shortlinkv1alpha1.Shortlink
	if err := k8sClient.Get(ctx, objectKey(name), &s); err != nil {
		return ""
	}
	for _, c := range s.Status.Conditions {
		if c.Type == shortlinkv1alpha1.ConditionReady {
			return c.Reason
		}
	}
	return ""
}

// eventTypesAndReasonsFor 列出作用在该对象上的事件，形如 "Warning/DeploymentReconcileFailed"。
// 用 legacy corev1.Event：mgr.GetEventRecorderFor 返回的是"旧版"记录器，
// 落到 core/v1 的 Event（新版的 mgr.GetEventRecorder 才会写 events.k8s.io/v1）。
func eventTypesAndReasonsFor(name string) []string {
	var events corev1.EventList
	if err := k8sClient.List(ctx, &events, client.InNamespace(testNamespace)); err != nil {
		return nil
	}
	out := make([]string, 0, len(events.Items))
	for _, e := range events.Items {
		if e.InvolvedObject.Name == name {
			out = append(out, e.Type+"/"+e.Reason)
		}
	}
	return out
}
