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
	"context"
	"fmt"
	"reflect"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	shortlinkv1alpha1 "github.com/Xiaoyu-hub/shortlink-operator/api/v1alpha1"
)

// 标签与命名约定：所有由本 operator 管理的子资源共用同一套标签。
// Service 的 selector 与 Deployment 的 pod template labels 必须一致（K8s 硬约束），
// 统一从这里取，避免两处手写漂移。
const (
	labelAppName   = "app.kubernetes.io/name"
	labelInstance  = "app.kubernetes.io/instance"
	labelManagedBy = "app.kubernetes.io/managed-by"

	appNameValue  = "shortlink"
	operatorName  = "shortlink-operator"
	containerName = "shortlink"
	httpPortName  = "http"
)

// ShortlinkReconciler reconciles a Shortlink object
type ShortlinkReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=shortlink.dev,resources=shortlinks,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=shortlink.dev,resources=shortlinks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=shortlink.dev,resources=shortlinks/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments/status,verbs=get
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services/status,verbs=get

// Reconcile 把集群中的 Deployment/Service 收敛到 Shortlink 声明的期望状态。
//
// 三条路径：
//  1. 不存在 → 创建（撞 AlreadyExists 视为并发窗口内他方已建，按已存在收敛）；
//  2. 存在但偏移 → 更新（只覆盖本 operator 管理的字段，见 deploymentNeedsUpdate）；
//  3. 无偏移 → 不写任何 API（幂等：连续 reconcile 不产生写入）。
//
// paused=true 是排障冻结开关：跳过子资源调和，只刷新 status 的可观测量，
// observedGeneration 停在冻结前版本——客户端看到 obsGen < gen 就知道「尚未收敛」。
func (r *ShortlinkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("Reconciling Shortlink", "name", req.Name, "namespace", req.Namespace)

	var cr shortlinkv1alpha1.Shortlink
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			// 对象已删除：子资源由 ownerReference 级联回收，控制器无事可做。
			log.Info("Shortlink not found; children are owned and will be garbage-collected",
				"name", req.Name, "namespace", req.Namespace)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if cr.Spec.Paused {
		// 冻结：不再推进底层资源，status 也只刷新可观测量（readyReplicas/url），
		// conditions/observedGeneration 保持冻结前快照，避免谎报「已收敛最新 spec」。
		log.Info("Shortlink paused; skipping child reconciliation", "name", cr.Name, "namespace", cr.Namespace)
		return ctrl.Result{}, r.updateStatus(ctx, &cr, false)
	}

	if err := r.reconcileDeployment(ctx, &cr); err != nil {
		log.Error(err, "Failed to reconcile Deployment", "name", cr.Name, "namespace", cr.Namespace)
		return ctrl.Result{}, err
	}
	if err := r.reconcileService(ctx, &cr); err != nil {
		log.Error(err, "Failed to reconcile Service", "name", cr.Name, "namespace", cr.Namespace)
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.updateStatus(ctx, &cr, true)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ShortlinkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// For：主资源变化入队。加 GenerationChangedPredicate 只放行 spec 变化
		// （generation 递增），滤掉 status-only 更新——我们自己写 status 会再次触发
		// Shortlink 的 watch，不滤掉就形成「写 status → 再调和 → 再写」的自循环噪音。
		For(&shortlinkv1alpha1.Shortlink{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// Owns：子资源任何变化（含 status 的 readyReplicas 走动）反查 owner 入队。
		// 这是「删 Pod 后自愈」「滚动后 status 刷新」的事件来源，不需要轮询。
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Named("shortlink").
		Complete(r)
}

// ---- 调和子资源 ----

func (r *ShortlinkReconciler) reconcileDeployment(ctx context.Context, cr *shortlinkv1alpha1.Shortlink) error {
	log := log.FromContext(ctx)

	desired := desiredDeployment(cr)
	if err := controllerutil.SetControllerReference(cr, desired, r.Scheme); err != nil {
		return err
	}

	var actual appsv1.Deployment
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &actual)
	if err == nil {
		if deploymentNeedsUpdate(&actual, desired) {
			log.Info("Updating Deployment", "name", actual.Name, "namespace", actual.Namespace)
			actual.Spec = desired.Spec
			actual.Labels = desired.Labels
			return r.Update(ctx, &actual)
		}
		return nil // 无偏移：零写入
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	log.Info("Creating Deployment", "name", desired.Name, "namespace", desired.Namespace)
	if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	// AlreadyExists：并发窗口内别的 reconcile 先建好了；事件驱动的 Owns 会再触发一次，
	// 下一次走 diff 分支统一收敛。这里直接返回即可。
	return nil
}

func (r *ShortlinkReconciler) reconcileService(ctx context.Context, cr *shortlinkv1alpha1.Shortlink) error {
	log := log.FromContext(ctx)

	desired := desiredService(cr)
	if err := controllerutil.SetControllerReference(cr, desired, r.Scheme); err != nil {
		return err
	}

	var actual corev1.Service
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &actual)
	if err == nil {
		if serviceNeedsUpdate(&actual, desired) {
			log.Info("Updating Service", "name", actual.Name, "namespace", actual.Namespace)
			// ClusterIP / ClusterIPs / IPFamily / NodePort 是 API Server 分配的值（全局唯一、
			// 由集群统一发放），本控制器不持有。这里把真实值搬回期望对象，是显式声明
			// 「这几个字段的作者是 API Server」，而不是防事故的必需品——实测（见
			// docs/day05 第十节）：即使不搬、传空值，API Server 也会保留旧值；
			// 真正的雷是把带过分配值的对象复用到别的对象上（Create / 改名 / 重建）。
			desired.Spec.ClusterIP = actual.Spec.ClusterIP
			desired.Spec.ClusterIPs = actual.Spec.ClusterIPs
			desired.Spec.IPFamilies = actual.Spec.IPFamilies
			desired.Spec.IPFamilyPolicy = actual.Spec.IPFamilyPolicy
			for i := range desired.Spec.Ports {
				if i < len(actual.Spec.Ports) {
					desired.Spec.Ports[i].NodePort = actual.Spec.Ports[i].NodePort
				}
			}
			actual.Spec = desired.Spec
			actual.Labels = desired.Labels
			return r.Update(ctx, &actual)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	log.Info("Creating Service", "name", desired.Name, "namespace", desired.Namespace)
	if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// ---- status 写回 ----

// updateStatus 把「对期望状态的最新观察」写回 status。
// advanceGeneration=true 时同时推进 observedGeneration 并刷新 Ready 条件；
// 冻结（paused）时只刷新 readyReplicas / url——那条因果链代表「最新 spec 已被调和」，
// 冻结状态下我们刻意没有调和，所以不推进它。
func (r *ShortlinkReconciler) updateStatus(ctx context.Context, cr *shortlinkv1alpha1.Shortlink, advanceGeneration bool) error {
	log := log.FromContext(ctx)

	readyReplicas, available := r.deploymentObservation(ctx, cr)
	desired := desiredReplicas(cr)

	old := cr.DeepCopy()

	cr.Status.ReadyReplicas = readyReplicas
	cr.Status.URL = statusURL(cr)

	if advanceGeneration {
		cr.Status.ObservedGeneration = cr.Generation

		cond := metav1.Condition{
			Type:               shortlinkv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             "ReplicasNotReady",
			Message:            fmt.Sprintf("expected %d replicas, %d reported ready", desired, readyReplicas),
			ObservedGeneration: cr.Generation,
		}
		if available && readyReplicas == desired {
			cond.Status = metav1.ConditionTrue
			cond.Reason = "ReplicasReady"
			cond.Message = fmt.Sprintf("%d/%d replicas ready", readyReplicas, desired)
		}
		meta.SetStatusCondition(&cr.Status.Conditions, cond)
	}

	if equality.Semantic.DeepEqual(old.Status, cr.Status) {
		return nil // 无变化不写：避免 status 写放大与自触发
	}

	if err := r.Status().Patch(ctx, cr, client.MergeFrom(old)); err != nil {
		return err
	}
	log.Info("Updated Shortlink status",
		"name", cr.Name, "namespace", cr.Namespace,
		"readyReplicas", readyReplicas, "observedGeneration", cr.Status.ObservedGeneration)
	return nil
}

// deploymentObservation 从 Deployment.status 读真实观测。
// 数据源是 API Server（informer 缓存），绝不使用控制器本地计数——本地计数会在
// 多副本 manager / 重启下漂移。
func (r *ShortlinkReconciler) deploymentObservation(ctx context.Context, cr *shortlinkv1alpha1.Shortlink) (int32, bool) {
	var deploy appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKeyFromObject(cr), &deploy); err != nil {
		return 0, false // NotFound 或读失败都按「未就绪」处理；后续事件会再次触发收敛
	}
	available := false
	for _, c := range deploy.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable && c.Status == corev1.ConditionTrue {
			available = true
			break
		}
	}
	return deploy.Status.ReadyReplicas, available
}

// ---- 期望对象构造 ----

// 期望副本数：取 spec.replicas；spec.replicas 为 nil 时兜底为 1。
//
// 这里**刻意不处理** paused：冻结的语义是「完全不干预子资源」（由 Reconcile 的 Paused
// 分支提前 return 实现），不是「把副本缩到 0」。曾有过 `if cr.Spec.Paused { return 0 }`
// 的分支，那是走不到的死代码——paused 时 Reconcile 根本不会调用本函数所在的构造链，
// 留着只会误导读者以为冻结会缩容。
//
// nil 兜底也不是给生产路径用的：spec.replicas 带 +kubebuilder:default=1，从 API Server
// 读回来的对象永远非 nil。真正会走到这里的是绕过 admission defaulting 的构造路径
// （例如测试直接 new 出来的 CR）。
func desiredReplicas(cr *shortlinkv1alpha1.Shortlink) int32 {
	if cr.Spec.Replicas != nil {
		return *cr.Spec.Replicas
	}
	return 1
}

func appLabels(cr *shortlinkv1alpha1.Shortlink) map[string]string {
	return map[string]string{
		labelAppName:   appNameValue,
		labelInstance:  cr.Name,
		labelManagedBy: operatorName,
	}
}

func desiredDeployment(cr *shortlinkv1alpha1.Shortlink) *appsv1.Deployment {
	labels := appLabels(cr)
	replicas := desiredReplicas(cr)
	port := cr.Spec.Port
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.Name,
			Namespace: cr.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{labelInstance: cr.Name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:      containerName,
						Image:     cr.Spec.Image,
						Ports:     []corev1.ContainerPort{{Name: httpPortName, ContainerPort: port, Protocol: corev1.ProtocolTCP}},
						Env:       cr.Spec.Env,
						Resources: cr.Spec.Resources,
					}},
				},
			},
		},
	}
}

func desiredService(cr *shortlinkv1alpha1.Shortlink) *corev1.Service {
	svcType := cr.Spec.ServiceType
	if svcType == "" {
		svcType = corev1.ServiceTypeClusterIP
	}
	port := cr.Spec.Port
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.Name,
			Namespace: cr.Namespace,
			Labels:    appLabels(cr),
		},
		Spec: corev1.ServiceSpec{
			Type:     svcType,
			Selector: map[string]string{labelInstance: cr.Name},
			Ports: []corev1.ServicePort{{
				Name:       httpPortName,
				Port:       port,
				TargetPort: intstr.FromInt32(port),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// statusURL 拼接待访问地址：有 baseDomain 用对外域名，否则用集群内 DNS 名。
func statusURL(cr *shortlinkv1alpha1.Shortlink) string {
	host := fmt.Sprintf("%s.%s.svc.cluster.local", cr.Name, cr.Namespace)
	if cr.Spec.BaseDomain != "" {
		host = cr.Spec.BaseDomain
	}
	if cr.Spec.Port != 80 {
		host = fmt.Sprintf("%s:%d", host, cr.Spec.Port)
	}
	return "http://" + host
}

// ---- 偏移检测（只比较「本 operator 管理的字段」，不是整体 DeepEqual）----
//
// 为什么不整体比较：对象经过 API Server 准入后会被补上大量默认值
// （ImagePullPolicy、TerminationMessagePath、Protocol=TCP，Service 还会被分配
// ClusterIP/NodePort…）。用 reflect.DeepEqual(actual, desired) 恒为不相等，
// 控制器会对「其实一致」的对象无限 Update——这是新手 operator 最常见的死循环。
// 正确姿势：只比我们声明并负责收敛的字段子集。

func deploymentNeedsUpdate(actual, desired *appsv1.Deployment) bool {
	if !reflect.DeepEqual(actual.Labels, desired.Labels) {
		return true
	}
	if !reflect.DeepEqual(actual.Spec.Replicas, desired.Spec.Replicas) {
		return true
	}
	if !reflect.DeepEqual(actual.Spec.Template.Labels, desired.Spec.Template.Labels) {
		return true
	}
	ac, dc := actual.Spec.Template.Spec.Containers, desired.Spec.Template.Spec.Containers
	if len(ac) != len(dc) {
		return true
	}
	for i := range dc {
		if ac[i].Name != dc[i].Name ||
			ac[i].Image != dc[i].Image ||
			!reflect.DeepEqual(ac[i].Env, dc[i].Env) ||
			!reflect.DeepEqual(ac[i].Resources, dc[i].Resources) ||
			!containerPortsEqual(ac[i].Ports, dc[i].Ports) {
			return true
		}
	}
	return false
}

func containerPortsEqual(a, b []corev1.ContainerPort) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].ContainerPort != b[i].ContainerPort {
			return false
		}
	}
	return true
}

func serviceNeedsUpdate(actual, desired *corev1.Service) bool {
	if !reflect.DeepEqual(actual.Labels, desired.Labels) {
		return true
	}
	if actual.Spec.Type != desired.Spec.Type {
		return true
	}
	if !reflect.DeepEqual(actual.Spec.Selector, desired.Spec.Selector) {
		return true
	}
	return !servicePortsEqual(actual.Spec.Ports, desired.Spec.Ports)
}

func servicePortsEqual(a, b []corev1.ServicePort) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name ||
			a[i].Port != b[i].Port ||
			!reflect.DeepEqual(a[i].TargetPort, b[i].TargetPort) ||
			a[i].Protocol != b[i].Protocol {
			return false
		}
	}
	return true
}
