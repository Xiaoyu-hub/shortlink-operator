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
	autoscalingv2 "k8s.io/api/autoscaling/v2"
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
	"sigs.k8s.io/controller-runtime/pkg/recorder"

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

// 事件 Reason：面向运维的「发生过什么动作」。与 status 条件的 Reason 分工不同——
// 事件描述**动作**（Created/Updated/Scaled），条件描述**状态**（Ready/NotReady）。
// 事件会随时间失效（默认 1 小时 TTL），条件才是长期可读的状态；两者互补，不能互相替代。
const (
	eventReasonCreated = "Created"
	eventReasonUpdated = "Updated"
	eventReasonScaled  = "Scaled"
	eventReasonDeleted = "Deleted"

	// events.k8s.io/v1 的 Event 除了 reason 还有 action 字段：reason 说"结果是什么"，
	// action 说"控制器尝试做了什么"。两者刻意分开，便于按动作聚合/告警。
	eventActionCreate    = "Create"
	eventActionUpdate    = "Update"
	eventActionScale     = "Scale"
	eventActionDelete    = "Delete"
	eventActionReconcile = "Reconcile"
)

// ShortlinkReconciler reconciles a Shortlink object
type ShortlinkReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Recorder 写 Kubernetes Event（kubectl describe 里能看到的那部分）。
	// 由 SetupWithManager 兜底注入：调用方没显式给时用 manager 的记录器，
	// 这样 main.go、测试套件、以及将来任何构造路径都不需要各自记得注入。
	//
	// 这里用的是 **events.k8s.io/v1** 的新事件 API（`mgr.GetEventRecorder`）；
	// 旧版 `GetEventRecorderFor` 写 core/v1 Event 且已被标记 deprecated——staticcheck 会报，
	// 迁过来顺带把 RBAC 的组也换成了 events.k8s.io（见下方 marker）。
	//
	// 写哪个 API 组**由代码路径唯一决定，不由环境决定**（读源码可证）：
	//
	//	GetEventRecorderFor → deprecatedRecorder  → corev1.EventSource{Component}      → core/v1
	//	GetEventRecorder    → lazyRecorder       → events.Broadcaster.NewRecorder(name)
	//	                                          → EventSinkImpl.Create(eventsv1.Event) → events.k8s.io/v1
	//
	// 所以这里必然写 events.k8s.io/v1；reportingController = "shortlink"，
	// reportingInstance = "shortlink-<hostname>"。
	Recorder recorder.EventRecorder
}

// +kubebuilder:rbac:groups=shortlink.dev,resources=shortlinks,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=shortlink.dev,resources=shortlinks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=shortlink.dev,resources=shortlinks/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments/status,verbs=get
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services/status,verbs=get
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// 同时授权 core/v1 events 属**防御性冗余**，不是因为我们真的写它：
// Day 7 曾在真集群看到 `kubectl get events`（默认读 core/v1）里有事件，据此以为记录器落到了 core/v1——
// 那是误归因：Deployment 扩缩容时 kube-controller-manager 必写 core/v1 事件
// （ScalingReplicaSet / SuccessfulCreate，source.component=deployment-controller|replicaset-controller）。
// 判定归属只看一个字段：core/v1 的 .source.component、events.k8s.io/v1 的 .reportingController 是否为 "shortlink"。
// 两组都保留：多授一份几乎无风险的权限（审计面略大），换掉"事件被 403 丢光而无人发现"这一类故障。
// 注意：403 不是完全静默——client-go 在后台 goroutine 里 klog 打 "Unable to write event"，
// 重试上限后才丢弃；不影响功能，只影响可观测性，所以没人看日志就等于没有。
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete

// Reconcile 把集群中的 Deployment/Service 收敛到 Shortlink 声明的期望状态。
//
// 三条路径：
//  1. 不存在 → 创建（撞 AlreadyExists 视为并发窗口内他方已建，按已存在收敛）；
//  2. 存在但偏移 → 更新（只覆盖本 operator 管理的字段，见 deploymentNeedsUpdate）；
//  3. 无偏移 → 不写任何 API（幂等：连续 reconcile 不产生写入）。
//
// paused=true 是排障冻结开关：跳过子资源调和，只刷新 status 的可观测量，
// observedGeneration 停在冻结前版本——客户端看到 obsGen < gen 就知道「尚未收敛」。
//
// 调和失败（子资源 Create/Update 被拒等）不是"静默重试"：会写 Warning 事件 + Ready=False 条件
// （见 reportFailure），并把原始错误往上抛给 workqueue 做指数退避。失败时**不推进**
// observedGeneration——它代表「最新 spec 已被完整调和」，而这一轮并没有完成。
func (r *ShortlinkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling Shortlink", "name", req.Name, "namespace", req.Namespace)

	var cr shortlinkv1alpha1.Shortlink
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			// 对象已删除：子资源由 ownerReference 级联回收，控制器无事可做。
			logger.Info("Shortlink not found; children are owned and will be garbage-collected",
				"name", req.Name, "namespace", req.Namespace)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if cr.Spec.Paused {
		// 冻结：不再推进底层资源，status 也只刷新可观测量（readyReplicas/url），
		// conditions/observedGeneration 保持冻结前快照，避免谎报「已收敛最新 spec」。
		logger.Info("Shortlink paused; skipping child reconciliation", "name", cr.Name, "namespace", cr.Namespace)
		return ctrl.Result{}, r.updateStatus(ctx, &cr, false)
	}

	if err := r.reconcileDeployment(ctx, &cr); err != nil {
		logger.Error(err, "Failed to reconcile Deployment", "name", cr.Name, "namespace", cr.Namespace)
		return ctrl.Result{}, r.reportFailure(ctx, &cr, shortlinkv1alpha1.ReasonDeploymentReconcileFailed, err)
	}
	if err := r.reconcileService(ctx, &cr); err != nil {
		logger.Error(err, "Failed to reconcile Service", "name", cr.Name, "namespace", cr.Namespace)
		return ctrl.Result{}, r.reportFailure(ctx, &cr, shortlinkv1alpha1.ReasonServiceReconcileFailed, err)
	}
	if err := r.reconcileHPA(ctx, &cr); err != nil {
		logger.Error(err, "Failed to reconcile HorizontalPodAutoscaler", "name", cr.Name, "namespace", cr.Namespace)
		return ctrl.Result{}, r.reportFailure(ctx, &cr, shortlinkv1alpha1.ReasonHPAReconcileFailed, err)
	}
	return ctrl.Result{}, r.updateStatus(ctx, &cr, true)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ShortlinkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// 事件记录器兜底注入：没显式配就用 manager 的（见字段注释）。
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("shortlink")
	}
	return ctrl.NewControllerManagedBy(mgr).
		// For：主资源变化入队。加 GenerationChangedPredicate 只放行 spec 变化
		// （generation 递增），滤掉 status-only 更新——我们自己写 status 会再次触发
		// Shortlink 的 watch，不滤掉就形成「写 status → 再调和 → 再写」的自循环噪音。
		For(&shortlinkv1alpha1.Shortlink{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// Owns：子资源任何变化（含 status 的 readyReplicas 走动）反查 owner 入队。
		// 这是「删 Pod 后自愈」「滚动后 status 刷新」的事件来源，不需要轮询。
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		// HPA 也纳入 watch：它被删/被改同样要自愈（第三件套）。
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Named("shortlink").
		Complete(r)
}

// ---- 调和子资源 ----

func (r *ShortlinkReconciler) reconcileDeployment(ctx context.Context, cr *shortlinkv1alpha1.Shortlink) error {
	logger := log.FromContext(ctx)

	desired := desiredDeployment(cr)
	if err := controllerutil.SetControllerReference(cr, desired, r.Scheme); err != nil {
		return err
	}

	var actual appsv1.Deployment
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &actual)
	if err == nil {
		if deploymentNeedsUpdate(&actual, desired) {
			// 副本数变化单独记一条事件：排障时「谁在什么时候缩/扩容」一眼能看到。
			if actual.Spec.Replicas != nil && desired.Spec.Replicas != nil &&
				*actual.Spec.Replicas != *desired.Spec.Replicas {
				r.Recorder.Eventf(cr, nil, corev1.EventTypeNormal, eventReasonScaled, eventActionScale,
					"Scaling Deployment %s from %d to %d replicas",
					actual.Name, *actual.Spec.Replicas, *desired.Spec.Replicas)
			}
			logger.Info("Updating Deployment", "name", actual.Name, "namespace", actual.Namespace)
			// 搬回"不归我们"的字段再整体覆盖——与 reconcileService 搬 clusterIP 是同一个模式，
			// 区别在动机：那边是"分配值不可变"，这边是"所有权在 HPA"。
			currentReplicas := actual.Spec.Replicas
			actual.Spec = desired.Spec
			actual.Labels = desired.Labels
			if !ownsReplicas(cr) {
				actual.Spec.Replicas = currentReplicas
			}
			if err := r.Update(ctx, &actual); err != nil {
				return err
			}
			r.Recorder.Eventf(cr, nil, corev1.EventTypeNormal, eventReasonUpdated, eventActionUpdate,
				"Updated Deployment %s", actual.Name)
			return nil
		}
		return nil // 无偏移：零写入
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	logger.Info("Creating Deployment", "name", desired.Name, "namespace", desired.Namespace)
	if err := r.Create(ctx, desired); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		// AlreadyExists：并发窗口内别的 reconcile 先建好了；事件驱动的 Owns 会再触发一次，
		// 下一次走 diff 分支统一收敛。这里直接返回即可。
		return nil
	}
	r.Recorder.Eventf(cr, nil, corev1.EventTypeNormal, eventReasonCreated, eventActionCreate,
		"Created Deployment %s", desired.Name)
	return nil
}

func (r *ShortlinkReconciler) reconcileService(ctx context.Context, cr *shortlinkv1alpha1.Shortlink) error {
	logger := log.FromContext(ctx)

	desired := desiredService(cr)
	if err := controllerutil.SetControllerReference(cr, desired, r.Scheme); err != nil {
		return err
	}

	var actual corev1.Service
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &actual)
	if err == nil {
		if serviceNeedsUpdate(&actual, desired) {
			logger.Info("Updating Service", "name", actual.Name, "namespace", actual.Namespace)
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
			if err := r.Update(ctx, &actual); err != nil {
				return err
			}
			r.Recorder.Eventf(cr, nil, corev1.EventTypeNormal, eventReasonUpdated, eventActionUpdate,
				"Updated Service %s", actual.Name)
			return nil
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	logger.Info("Creating Service", "name", desired.Name, "namespace", desired.Namespace)
	if err := r.Create(ctx, desired); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		return nil
	}
	r.Recorder.Eventf(cr, nil, corev1.EventTypeNormal, eventReasonCreated, eventActionCreate,
		"Created Service %s", desired.Name)
	return nil
}

// ---- HPA ----

// desiredHPA 从 CR 构造期望的 HPA（纯函数，不读集群）。
// 只声明我们负责的部分：scaleTargetRef 指向同名 Deployment，CPU 目标利用率来自 spec.autoscaling。
func desiredHPA(cr *shortlinkv1alpha1.Shortlink) *autoscalingv2.HorizontalPodAutoscaler {
	as := cr.Spec.Autoscaling
	minReplicas := int32(1)
	if as.MinReplicas != nil {
		minReplicas = *as.MinReplicas
	}
	target := as.TargetCPUUtilizationPercentage
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.Name,
			Namespace: cr.Namespace,
			Labels:    appLabels(cr),
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       cr.Name,
			},
			MinReplicas: &minReplicas,
			MaxReplicas: as.MaxReplicas,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: &target,
					},
				},
			}},
		},
	}
}

// reconcileHPA 让集群里的 HPA 与 CR 的声明一致：
//
//	声明了 → 存在则比对着更新、不存在则创建（撞 AlreadyExists 交给事件驱动重试）；
//	没声明 → 若还残留由我们创建的 HPA，删掉（"按需 create/delete"）。
//
// 注意 Controller Reference：HPA 挂在本 CR 上，删 CR 时由 GC 级联回收——
// 但这也意味着"关闭 autoscaling"时必须显式删除（GC 只在 CR 被删时动手）。
func (r *ShortlinkReconciler) reconcileHPA(ctx context.Context, cr *shortlinkv1alpha1.Shortlink) error {
	logger := log.FromContext(ctx)
	key := client.ObjectKeyFromObject(cr)

	if cr.Spec.Autoscaling == nil {
		var existing autoscalingv2.HorizontalPodAutoscaler
		switch err := r.Get(ctx, key, &existing); {
		case apierrors.IsNotFound(err):
			return nil // 幂等：没有就什么都不做
		case err != nil:
			return err
		}
		if err := r.Delete(ctx, &existing); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		r.Recorder.Eventf(cr, nil, corev1.EventTypeNormal, eventReasonDeleted, eventActionDelete,
			"Deleted HorizontalPodAutoscaler %s (autoscaling disabled)", existing.Name)
		return nil
	}

	desired := desiredHPA(cr)
	if err := controllerutil.SetControllerReference(cr, desired, r.Scheme); err != nil {
		return err
	}

	var actual autoscalingv2.HorizontalPodAutoscaler
	err := r.Get(ctx, key, &actual)
	if err == nil {
		if hpaNeedsUpdate(&actual, desired) {
			logger.Info("Updating HorizontalPodAutoscaler", "name", actual.Name, "namespace", actual.Namespace)
			actual.Spec = desired.Spec
			actual.Labels = desired.Labels
			if err := r.Update(ctx, &actual); err != nil {
				return err
			}
			r.Recorder.Eventf(cr, nil, corev1.EventTypeNormal, eventReasonUpdated, eventActionUpdate,
				"Updated HorizontalPodAutoscaler %s", actual.Name)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	logger.Info("Creating HorizontalPodAutoscaler", "name", desired.Name, "namespace", desired.Namespace)
	if err := r.Create(ctx, desired); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		return nil
	}
	r.Recorder.Eventf(cr, nil, corev1.EventTypeNormal, eventReasonCreated, eventActionCreate,
		"Created HorizontalPodAutoscaler %s", desired.Name)
	return nil
}

// hpaNeedsUpdate 只比对本 operator 声明的字段。
// 指标只比 CPU 利用率目标：别的指标类型我们不声明、也不拥有——但要注意整体覆盖
// （actual.Spec = desired.Spec）仍会把它们抹掉，这是与 Deployment 完全同款的已知取舍。
func hpaNeedsUpdate(actual, desired *autoscalingv2.HorizontalPodAutoscaler) bool {
	if !reflect.DeepEqual(actual.Labels, desired.Labels) {
		return true
	}
	if actual.Spec.ScaleTargetRef != desired.Spec.ScaleTargetRef {
		return true
	}
	if !reflect.DeepEqual(actual.Spec.MinReplicas, desired.Spec.MinReplicas) {
		return true
	}
	if actual.Spec.MaxReplicas != desired.Spec.MaxReplicas {
		return true
	}
	at, aok := cpuUtilizationTarget(actual.Spec.Metrics)
	dt, dok := cpuUtilizationTarget(desired.Spec.Metrics)
	return !aok || !dok || at != dt
}

// cpuUtilizationTarget 从指标列表里取出 CPU 利用率目标（不存在时 ok=false）。
func cpuUtilizationTarget(metrics []autoscalingv2.MetricSpec) (int32, bool) {
	for _, m := range metrics {
		if m.Type != autoscalingv2.ResourceMetricSourceType || m.Resource == nil {
			continue
		}
		if m.Resource.Name != corev1.ResourceCPU || m.Resource.Target.AverageUtilization == nil {
			continue
		}
		return *m.Resource.Target.AverageUtilization, true
	}
	return 0, false
}

// ---- status 写回 ----

// updateStatus 把「对期望状态的最新观察」写回 status。
// advanceGeneration=true 时同时推进 observedGeneration 并刷新 Ready 条件；
// 冻结（paused）时只刷新 readyReplicas / url——那条因果链代表「最新 spec 已被调和」，
// 冻结状态下我们刻意没有调和，所以不推进它。
func (r *ShortlinkReconciler) updateStatus(ctx context.Context, cr *shortlinkv1alpha1.Shortlink, advanceGeneration bool) error {
	logger := log.FromContext(ctx)

	st := r.observeDeployment(ctx, cr)

	changed, err := r.patchStatus(ctx, cr, func(s *shortlinkv1alpha1.ShortlinkStatus) {
		// 可观测量：每次观测都刷新，无论这一轮成功与否。
		s.ReadyReplicas = st.ready
		s.URL = statusURL(cr)
		if advanceGeneration {
			// 结论：只有「最新 spec 这一轮被完整走完」才更新。
			s.ObservedGeneration = cr.Generation
			meta.SetStatusCondition(&s.Conditions, readyCondition(cr, expectedReplicas(cr, st), st))
		}
	})
	if err != nil || !changed {
		return err
	}
	logger.Info("Updated Shortlink status",
		"name", cr.Name, "namespace", cr.Namespace,
		"readyReplicas", st.ready, "observedGeneration", cr.Status.ObservedGeneration)
	return nil
}

// reportFailure 把「这一轮调和失败」变成外部可观测的两件事：Warning 事件 + Ready=False 条件。
//
// 两条刻意的设计：
//  1. **不推进 status.observedGeneration**——它代表「最新 spec 已被完整调和」，而这一轮没有完成，
//     推进它就等于谎报收敛（客户端见 obsGen < gen 才知道要等）。
//  2. **条件自身的 ObservedGeneration 仍记当前 generation**——这条 False 说的正是当前这版 spec，
//     两者语义不同，所以不能一起省略。
//
// 返回原始 cause（而不是写 status 时可能的错误），让 workqueue 按真正的失败原因退避重试；
// status 写失败只记日志，避免它掩盖原始故障。
func (r *ShortlinkReconciler) reportFailure(
	ctx context.Context,
	cr *shortlinkv1alpha1.Shortlink,
	reason string,
	cause error,
) error {
	r.Recorder.Eventf(cr, nil, corev1.EventTypeWarning, reason, eventActionReconcile, "%s", cause.Error())

	st := r.observeDeployment(ctx, cr)
	_, err := r.patchStatus(ctx, cr, func(s *shortlinkv1alpha1.ShortlinkStatus) {
		s.ReadyReplicas = st.ready
		s.URL = statusURL(cr)
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{
			Type:               shortlinkv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            cause.Error(),
			ObservedGeneration: cr.Generation,
		})
	})
	if err != nil {
		log.FromContext(ctx).Error(err, "Failed to write failure status",
			"name", cr.Name, "namespace", cr.Namespace)
	}
	return cause
}

// patchStatus 是 status 的唯一写出口：mutate 里只改该改的字段，无变化则一次 API 都不发。
// 用 Status().Patch(MergeFrom(old)) 而非 Update：并发下别的副本改了 status 时，
// merge patch 不要求 resourceVersion 一致，比 Update 撞 409 更宽容。
func (r *ShortlinkReconciler) patchStatus(
	ctx context.Context,
	cr *shortlinkv1alpha1.Shortlink,
	mutate func(*shortlinkv1alpha1.ShortlinkStatus),
) (bool, error) {
	old := cr.DeepCopy()
	mutate(&cr.Status)
	if equality.Semantic.DeepEqual(old.Status, cr.Status) {
		return false, nil // 无变化不写：避免 status 写放大与自触发
	}
	if err := r.Status().Patch(ctx, cr, client.MergeFrom(old)); err != nil {
		return false, err
	}
	return true, nil
}

// deploymentState 是对子资源 Deployment 的一次观测快照。
// found 与 available 分开，才能把「还没建出来」和「建出来了但不可用」报成不同的 Reason——
// 折叠成一种的话，message 会自相矛盾（见 readyCondition）。
type deploymentState struct {
	found     bool
	available bool
	ready     int32
	// specReplicas 是 Deployment **自己**声明的副本数。autoscaling 模式下它是 HPA 写的值，
	// 也就是"这一层此刻的真实期望"——Ready 的判据必须用它，而不是 CR 里的 replicas。
	specReplicas *int32
}

// observeDeployment 从 Deployment.status 读真实观测。
// 数据源是 API Server（informer 缓存），绝不使用控制器本地计数——本地计数会在
// 多副本 manager / 重启下漂移。
//
// 注意缓存是最终一致的：刚 Create 完的 Deployment 可能在这里短暂读到 NotFound，
// 于是 status 会如实写出 DeploymentNotFound 而不是硬猜一个值——下一轮事件会把它纠正成真实观测。
func (r *ShortlinkReconciler) observeDeployment(ctx context.Context, cr *shortlinkv1alpha1.Shortlink) deploymentState {
	var deploy appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKeyFromObject(cr), &deploy); err != nil {
		return deploymentState{}
	}
	st := deploymentState{found: true, ready: deploy.Status.ReadyReplicas, specReplicas: deploy.Spec.Replicas}
	for _, c := range deploy.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable && c.Status == corev1.ConditionTrue {
			st.available = true
			break
		}
	}
	return st
}

// expectedReplicas 是 Ready 判据里的"分母"：
//
//	副本数归本控制器 → 用 CR 的期望；
//	归 HPA          → 用 Deployment.spec.replicas（HPA 写的），因为那才是此刻的真实期望。
//
// 这是"所有权变更会连带改变判据"的一个具体例子：同一个 Ready 条件，在两种模式下算的东西不同。
func expectedReplicas(cr *shortlinkv1alpha1.Shortlink, st deploymentState) int32 {
	if !ownsReplicas(cr) && st.specReplicas != nil {
		return *st.specReplicas
	}
	return desiredReplicas(cr)
}

// readyCondition 把观测快照翻译成 Ready 条件。三种未就绪各有独立 Reason。
func readyCondition(cr *shortlinkv1alpha1.Shortlink, desired int32, st deploymentState) metav1.Condition {
	cond := metav1.Condition{
		Type:               shortlinkv1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: cr.Generation,
	}
	switch {
	case !st.found:
		cond.Reason = shortlinkv1alpha1.ReasonDeploymentNotFound
		cond.Message = fmt.Sprintf("Deployment %s not found yet", cr.Name)
	case st.ready != desired:
		cond.Reason = shortlinkv1alpha1.ReasonReplicasNotReady
		cond.Message = fmt.Sprintf("%d/%d replicas ready", st.ready, desired)
	case !st.available:
		// 副本数达标但 Deployment 还没 Available：仍在滚动，或进度超时。
		cond.Reason = shortlinkv1alpha1.ReasonDeploymentNotAvailable
		cond.Message = fmt.Sprintf("%d/%d replicas ready, but Deployment is not available yet", st.ready, desired)
	default:
		cond.Status = metav1.ConditionTrue
		cond.Reason = shortlinkv1alpha1.ReasonReplicasReady
		cond.Message = fmt.Sprintf("%d/%d replicas ready", st.ready, desired)
	}
	return cond
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

// desiredDeployment 从 CR 构造期望的 Deployment（纯函数：只读 cr，不碰集群）。
//
// 一个必须记住的边界：本函数**只设置**下面这些字段，但写入路径是整体覆盖
// （actual.Spec = desired.Spec），于是 spec 里"本函数不设置、也不比较"的字段会被抹回默认值
// （例如别人手工设的 minReadySeconds、strategy 里没声明的项）——实测见 docs/day06 第 7.2 节。
// 要管一个字段，就必须**同时**加进本函数与 deploymentNeedsUpdate。
func desiredDeployment(cr *shortlinkv1alpha1.Shortlink) *appsv1.Deployment {
	labels := appLabels(cr)
	replicas := desiredReplicas(cr)
	port := cr.Spec.Port
	dep := &appsv1.Deployment{
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
			Strategy: desiredStrategy(cr),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:      containerName,
						Image:     cr.Spec.Image,
						Ports:     []corev1.ContainerPort{{Name: httpPortName, ContainerPort: port, Protocol: corev1.ProtocolTCP}},
						Env:       cr.Spec.Env,
						Resources: cr.Spec.Resources,
						// 就绪探针决定 Pod 何时算「就绪」。没有它时 readyReplicas 只代表
						// "容器进程起来了"，滚动更新也就失去意义（新旧 Pod 无法区分就绪与否）。
						//
						// 这里用 TCP 探针而不是 HTTPGet：TCP 只验证端口可连，对任何镜像都成立，
						// 适合作为 operator 的默认值。真实项目应改成 HTTPGet（如 /healthz）——
						// 只有 HTTP 探针才能证明「业务可用」，TCP 只能证明「端口开着」。
						// 所有字段都显式写出，避免与 API Server 的探针默认值产生比较噪音。
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)},
							},
							InitialDelaySeconds: 2,
							PeriodSeconds:       5,
							TimeoutSeconds:      2,
							SuccessThreshold:    1,
							FailureThreshold:    3,
						},
					}},
				},
			},
		},
	}
	// 让位：声明了 autoscaling 时副本数归 HPA，我们不声明它（nil = 不拥有）。
	// 注意整体覆盖仍然会写 Spec —— 搬运在 reconcileDeployment 里做。
	if !ownsReplicas(cr) {
		dep.Spec.Replicas = nil
	}
	return dep
}

// ownsReplicas 表示 Deployment 的 spec.replicas 是否归本控制器所有。
// 声明了 autoscaling 时归 HPA：我们既不写、也不比较它，并在覆写前把 HPA 写的值原样搬回，
// 否则整体覆盖会把它抹回默认值（Deployment 的 replicas 默认 1），变成"控制器和 HPA 抢副本数"。
func ownsReplicas(cr *shortlinkv1alpha1.Shortlink) bool {
	return cr.Spec.Autoscaling == nil
}

// desiredStrategy 把 CR 的滚动参数翻译成 Deployment 的滚动策略。
// 两个字段都没声明时返回零值——让 API Server 自己的默认（RollingUpdate 25%/25%）生效，
// 而不是把 25% 这个数字抄进我们的代码里（抄了就要跟着 K8s 版本走）。
func desiredStrategy(cr *shortlinkv1alpha1.Shortlink) appsv1.DeploymentStrategy {
	if cr.Spec.MaxSurge == nil && cr.Spec.MaxUnavailable == nil {
		return appsv1.DeploymentStrategy{}
	}
	return appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxSurge:       cr.Spec.MaxSurge,
			MaxUnavailable: cr.Spec.MaxUnavailable,
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
	// 条件性所有权：desired 里 replicas 为 nil ⇒ 我们不拥有它（HPA 拥有）⇒ 不比较。
	// 与 rollingUpdateDiffers 里 nil 的语义同构：写它才比它，不写它就别比它。
	if desired.Spec.Replicas != nil && !reflect.DeepEqual(actual.Spec.Replicas, desired.Spec.Replicas) {
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
			!containerPortsEqual(ac[i].Ports, dc[i].Ports) ||
			!readinessProbeEqual(ac[i].ReadinessProbe, dc[i].ReadinessProbe) {
			return true
		}
	}
	return rollingUpdateDiffers(actual.Spec.Strategy, desired.Spec.Strategy)
}

// readinessProbeEqual 只比对本 operator 显式声明的探针字段。
// 为什么不整体 DeepEqual：Probe 在准入时会被补默认值（TimeoutSeconds/PeriodSeconds/
// SuccessThreshold/FailureThreshold…），整体比对恒为不相等 —— 与 Day 5 学的 defaulting 陷阱同源。
func readinessProbeEqual(actual, desired *corev1.Probe) bool {
	if actual == nil || desired == nil {
		return actual == desired
	}
	at, dt := actual.TCPSocket, desired.TCPSocket
	if (at == nil) != (dt == nil) {
		return false
	}
	if at != nil && at.Port.String() != dt.Port.String() {
		return false
	}
	return actual.InitialDelaySeconds == desired.InitialDelaySeconds &&
		actual.PeriodSeconds == desired.PeriodSeconds &&
		actual.TimeoutSeconds == desired.TimeoutSeconds &&
		actual.SuccessThreshold == desired.SuccessThreshold &&
		actual.FailureThreshold == desired.FailureThreshold
}

// rollingUpdateDiffers 处理「条件性所有权」：CR 没声明滚动参数时，期望是零值，
// 此时**不比较**——nil 的语义是「我不管这个字段」，让 Deployment 的默认值（25%）生效。
// 一旦声明了，对应字段就进入比较集合，偏移会被拉回。
//
// 比较集合可以是条件性的，但条件必须与写入意图一致：写它才比它，不写它就别比它。
func rollingUpdateDiffers(actual, desired appsv1.DeploymentStrategy) bool {
	if desired.Type != "" && actual.Type != desired.Type {
		return true
	}
	ar, dr := actual.RollingUpdate, desired.RollingUpdate
	if dr == nil {
		return false
	}
	if ar == nil {
		return true
	}
	if dr.MaxSurge != nil && !intOrStringEqual(ar.MaxSurge, dr.MaxSurge) {
		return true
	}
	if dr.MaxUnavailable != nil && !intOrStringEqual(ar.MaxUnavailable, dr.MaxUnavailable) {
		return true
	}
	return false
}

// intOrStringEqual 按字符串形态比较 IntOrString。
// 不用 reflect.DeepEqual：两种表示（Type=Int 的 1 与 Type=String 的 "1"）语义相同却判为不等。
// 这里不假设 API Server 会归一化表示形式，取保守写法。
func intOrStringEqual(a, b *intstr.IntOrString) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.String() == b.String()
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
