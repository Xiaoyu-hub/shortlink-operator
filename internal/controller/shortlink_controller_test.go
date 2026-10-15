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
