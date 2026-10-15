// defaulting-probe 用 envtest 里的真实 kube-apiserver 做三个对照实验：
//
//	实验 1：我提交的最小 Deployment，API Server 存下来的变成了什么（defaulting）
//	实验 2：我提交的最小 Service，多了哪些我没写过的字段（allocation）
//	实验 3：把分配值清空后再 Update，API Server 会怎么回我
//
// 用法：go run ./hack/defaulting-probe
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func show(title string, obj any) {
	b, err := yaml.Marshal(obj)
	must(err)
	fmt.Printf("\n--- %s ---\n%s", title, string(b))
}

// jsonFieldCount 数一个结构体序列化后有多少个字段，用来对比「我提交了几个 / 存下来几个」。
func jsonFieldCount(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return -1
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return -1
	}
	return len(m)
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	assets, err := filepath.Abs(filepath.Join("bin", "k8s", "1.37.0-windows-amd64"))
	must(err)
	env := &envtest.Environment{BinaryAssetsDirectory: assets}
	cfg, err := env.Start()
	must(err)
	defer func() { _ = env.Stop() }()

	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	must(err)

	must(c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "probe"}}))
	fmt.Println("envtest apiserver 已启动（service-cluster-ip-range=10.0.0.0/24）")

	// =====================================================================
	// 实验 1：Deployment —— 我提交 2 个字段，拿回来 n 个
	// =====================================================================
	sent := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "probe"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "probe"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "probe"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Image: "nginx:1.27"}},
				},
			},
		},
	}
	// Create 成功后 client 会把服务端返回写回 sent，所以先留一份提交前快照
	sentSpec, err := yaml.Marshal(sent.Spec)
	must(err)
	sentContainer := sent.Spec.Template.Spec.Containers[0]

	must(c.Create(ctx, sent))

	var got appsv1.Deployment
	must(c.Get(ctx, types.NamespacedName{Namespace: "probe", Name: "probe"}, &got))

	fmt.Println("\n########## 实验 1：Deployment ##########")
	fmt.Printf("【我提交的 spec】\n%s", string(sentSpec))
	show("【API Server 返回的 spec】", got.Spec)
	fmt.Printf("\n【作者对比：容器字段】\n")
	fmt.Printf("  我提交的容器：%d 个字段\n", jsonFieldCount(sentContainer))
	show("  我提交的容器原文", sentContainer)
	fmt.Printf("  API Server 存下来的容器：%d 个字段\n", jsonFieldCount(got.Spec.Template.Spec.Containers[0]))
	show("  存下来的容器原文", got.Spec.Template.Spec.Containers[0])

	fmt.Println("\n【metadata.managedFields：谁声称拥有哪些字段】")
	show("  managedFields", got.ManagedFields)

	// =====================================================================
	// 实验 2：Service —— clusterIP / nodePort 是「分配」来的，不是默认值
	// =====================================================================
	sentSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "probe"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "probe"},
			Ports:    []corev1.ServicePort{{Port: 8080, TargetPort: intstr.FromInt32(8080)}},
		},
	}
	sentSvcSpec, err := yaml.Marshal(sentSvc.Spec)
	must(err)
	sentSvcPort := sentSvc.Spec.Ports[0]

	must(c.Create(ctx, sentSvc))

	var gotSvc corev1.Service
	must(c.Get(ctx, types.NamespacedName{Namespace: "probe", Name: "probe"}, &gotSvc))

	fmt.Println("\n########## 实验 2：Service ##########")
	fmt.Printf("【我提交的 spec】\n%s", string(sentSvcSpec))
	gotSvcSpec, err := yaml.Marshal(gotSvc.Spec)
	must(err)
	fmt.Printf("【API Server 返回的 spec】\n%s", string(gotSvcSpec))
	fmt.Printf("\n【作者对比：ServicePort】\n")
	fmt.Printf("  我提交的 port：%d 个字段\n", jsonFieldCount(sentSvcPort))
	show("  我提交的 port 原文", sentSvcPort)
	fmt.Printf("  存下来的 port：%d 个字段\n", jsonFieldCount(gotSvc.Spec.Ports[0]))
	show("  存下来的 port 原文", gotSvc.Spec.Ports[0])
	fmt.Printf("  clusterIP（我没写过）= %q\n", gotSvc.Spec.ClusterIP)

	// =====================================================================
	// 实验 3a：想「整体覆盖」但没带上 clusterIP —— API Server 的反应
	// =====================================================================
	fmt.Println("\n########## 实验 3a：Update 时清空 clusterIP ##########")
	fmt.Printf("  Update 前 clusterIP = %q\n", gotSvc.Spec.ClusterIP)
	evil := gotSvc.DeepCopy()
	evil.Labels = map[string]string{"touched-by": "operator"} // 模拟：改了 label，于是整体覆盖 Spec
	evil.Spec.ClusterIP = ""
	evil.Spec.ClusterIPs = nil
	err = c.Update(ctx, evil)
	fmt.Printf("  Update(带空 clusterIP) → err=%v\n", err)

	var afterSvc corev1.Service
	must(c.Get(ctx, types.NamespacedName{Namespace: "probe", Name: "probe"}, &afterSvc))
	fmt.Printf("  Update 后 clusterIP = %q （变化：%v）\n",
		afterSvc.Spec.ClusterIP, afterSvc.Spec.ClusterIP != gotSvc.Spec.ClusterIP)
	afterSvcSpec, err := yaml.Marshal(afterSvc.Spec)
	must(err)
	fmt.Printf("【Update 后的完整 spec】\n%s", string(afterSvcSpec))
	fmt.Printf("  Labels 是否被改成了 touched-by=operator：%v\n", afterSvc.Labels["touched-by"] == "operator")

	// =====================================================================
	// 实验 3a-2：把 clusterIP 改成一个「别的具体值」
	// =====================================================================
	fmt.Println("\n########## 实验 3a-2：显式改 clusterIP = 10.0.0.222 ##########")
	evil2 := afterSvc.DeepCopy()
	evil2.Spec.ClusterIP = "10.0.0.222"
	evil2.Spec.ClusterIPs = []string{"10.0.0.222"}
	err = c.Update(ctx, evil2)
	fmt.Printf("  Update(clusterIP=10.0.0.222) → err=%v\n", err)

	// =====================================================================
	// 实验 3b：NodePort 服务，把 nodePort 清成 0 再 Update
	// =====================================================================
	fmt.Println("\n########## 实验 3b：NodePort 清空后重新分配 ##########")
	sentNP := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "probe-np", Namespace: "probe"},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeNodePort,
			Selector: map[string]string{"app": "probe"},
			Ports:    []corev1.ServicePort{{Port: 8080, TargetPort: intstr.FromInt32(8080)}},
		},
	}
	must(c.Create(ctx, sentNP))

	var gotNP corev1.Service
	must(c.Get(ctx, types.NamespacedName{Namespace: "probe", Name: "probe-np"}, &gotNP))
	first := gotNP.Spec.Ports[0].NodePort
	fmt.Printf("  创建后分配到的 nodePort = %d\n", first)

	again := gotNP.DeepCopy()
	again.Spec.Ports[0].NodePort = 0
	again.Labels = map[string]string{"touched-by": "operator"}
	err = c.Update(ctx, again)
	fmt.Printf("  Update(nodePort=0) → %v\n", err)

	var after corev1.Service
	must(c.Get(ctx, types.NamespacedName{Namespace: "probe", Name: "probe-np"}, &after))
	fmt.Printf("  Update 之后的 nodePort = %d （变化：%v）\n",
		after.Spec.Ports[0].NodePort, after.Spec.Ports[0].NodePort != first)

	// 对照：另建一个全新的 NodePort 服务，看分配器给什么值
	//（用来判断上面那个 31672 是「被保留」还是「重新分配恰好撞回同一个值」）
	secondNP := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "probe-np2", Namespace: "probe"},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeNodePort,
			Selector: map[string]string{"app": "probe"},
			Ports:    []corev1.ServicePort{{Port: 8080, TargetPort: intstr.FromInt32(8080)}},
		},
	}
	must(c.Create(ctx, secondNP))
	var gotNP2 corev1.Service
	must(c.Get(ctx, types.NamespacedName{Namespace: "probe", Name: "probe-np2"}, &gotNP2))
	fmt.Printf("  对照：全新创建的 probe-np2 分配到的 nodePort = %d\n", gotNP2.Spec.Ports[0].NodePort)

	// =====================================================================
	// 实验 3c：把已有对象 DeepCopy 一下改名当成"新对象"用 —— 分配值会跟着走
	// =====================================================================
	fmt.Println("\n########## 实验 3c：DeepCopy 改名当新对象 ##########")
	clone := gotNP.DeepCopy()
	clone.Name = "probe-np3"
	clone.ResourceVersion = ""
	clone.UID = ""
	clone.OwnerReferences = nil
	err = c.Create(ctx, clone)
	fmt.Printf("  Create(DeepCopy 改名) → err=%v\n", err)

	// =====================================================================
	// 实验 4：内容一字不差的写操作，resourceVersion 会不会变？
	//（用来判定「无偏移也写」会不会形成 写→事件→调和→写 的热循环）
	// =====================================================================
	fmt.Println("\n########## 实验 4：无变化的写操作 ##########")
	var d1 appsv1.Deployment
	must(c.Get(ctx, types.NamespacedName{Namespace: "probe", Name: "probe"}, &d1))

	rvBefore := d1.ResourceVersion
	must(c.Update(ctx, d1.DeepCopy())) // 内容与当前完全一致
	var d2 appsv1.Deployment
	must(c.Get(ctx, types.NamespacedName{Namespace: "probe", Name: "probe"}, &d2))
	fmt.Printf("  4a 内容不变的 Update：%s → %s （变化：%v）\n",
		rvBefore, d2.ResourceVersion, rvBefore != d2.ResourceVersion)

	rvBefore2 := d2.ResourceVersion
	must(c.Patch(ctx, d2.DeepCopy(), client.MergeFrom(d2.DeepCopy()))) // 空 merge patch
	var d3 appsv1.Deployment
	must(c.Get(ctx, types.NamespacedName{Namespace: "probe", Name: "probe"}, &d3))
	fmt.Printf("  4b 空 merge patch：%s → %s （变化：%v）\n",
		rvBefore2, d3.ResourceVersion, rvBefore2 != d3.ResourceVersion)

	// =====================================================================
	// 实验 5：如果控制器把「不属于自己的字段」纳入判据会怎样
	//   —— 模拟一个 needsUpdate 里比较 nodePort 的控制器：它每次都想把 nodePort 写成 0，
	//      而 API Server 每次都把旧值保留回来（实验 3b），于是「期望永远追不上现实」。
	//	     连做 3 轮，看 resourceVersion 会不会一路涨。
	// =====================================================================
	fmt.Println("\n########## 实验 5：把不归自己的字段纳入判据 ##########")
	var npSvc corev1.Service
	must(c.Get(ctx, types.NamespacedName{Namespace: "probe", Name: "probe-np"}, &npSvc))
	fmt.Printf("  起始：nodePort=%d resourceVersion=%s\n",
		npSvc.Spec.Ports[0].NodePort, npSvc.ResourceVersion)

	for i := 1; i <= 3; i++ {
		var cur corev1.Service
		must(c.Get(ctx, types.NamespacedName{Namespace: "probe", Name: "probe-np"}, &cur))
		rvBefore := cur.ResourceVersion
		// 控制器的「期望」：nodePort 归零（它认为这是自己管理的字段）
		cur.Spec.Ports[0].NodePort = 0
		err = c.Update(ctx, &cur)
		var after corev1.Service
		must(c.Get(ctx, types.NamespacedName{Namespace: "probe", Name: "probe-np"}, &after))
		fmt.Printf("  第 %d 轮：Update err=%v，nodePort=%d，resourceVersion %s → %s（变化：%v）\n",
			i, err, after.Spec.Ports[0].NodePort, rvBefore, after.ResourceVersion,
			rvBefore != after.ResourceVersion)
	}

	// =====================================================================
	// 实验 6：对照组 —— 真的改变了存储内容时，resourceVersion 会不会变
	//（用来确认实验 4/5 的「不变」不是探针自身的问题）
	// =====================================================================
	fmt.Println("\n########## 实验 6：对照 —— 真的有变化 ##########")
	var ctl corev1.Service
	must(c.Get(ctx, types.NamespacedName{Namespace: "probe", Name: "probe-np"}, &ctl))
	rvB := ctl.ResourceVersion
	ctl.Labels = map[string]string{"probe-rv-control": "yes"}
	must(c.Update(ctx, &ctl))
	var ctlAfter corev1.Service
	must(c.Get(ctx, types.NamespacedName{Namespace: "probe", Name: "probe-np"}, &ctlAfter))
	fmt.Printf("  加一个 label：resourceVersion %s → %s（变化：%v）\n",
		rvB, ctlAfter.ResourceVersion, rvB != ctlAfter.ResourceVersion)

	// =====================================================================
	// 实验 7：字段所有权 —— 两个 field manager 抢同一个字段
	//   SSA（Server-Side Apply）= 你只提交「你拥有的字段」，API Server 做字段级合并，
	//   冲突时显式报 409，而不是静默覆盖。
	// =====================================================================
	fmt.Println("\n########## 实验 7：字段所有权与 SSA 冲突 ##########")

	teamA := &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: "probe-ssa", Namespace: "probe"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "probe"},
			Ports:    []corev1.ServicePort{{Port: 8080, TargetPort: intstr.FromInt32(8080)}},
		},
	}
	err = c.Patch(ctx, teamA, client.Apply, client.FieldOwner("team-a-operator"))
	fmt.Printf("  7a team-a-operator 用 Apply 创建 → err=%v\n", err)

	var ssaSvc corev1.Service
	must(c.Get(ctx, types.NamespacedName{Namespace: "probe", Name: "probe-ssa"}, &ssaSvc))
	fmt.Printf("     分配到的 clusterIP = %q\n", ssaSvc.Spec.ClusterIP)
	show("     创建后的 managedFields", ssaSvc.ManagedFields)

	// 7b：另一个 manager 想改同一个字段（ports），不带 force
	teamB := &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: "probe-ssa", Namespace: "probe"},
		Spec: corev1.ServiceSpec{ // 只提交它想拥有的字段
			Selector: map[string]string{"app": "probe", "team": "b"},
			Ports:    []corev1.ServicePort{{Port: 9090, TargetPort: intstr.FromInt32(9090)}},
		},
	}
	err = c.Patch(ctx, teamB, client.Apply, client.FieldOwner("team-b-operator"))
	fmt.Printf("  7b team-b-operator 抢 ports（不带 force）→ err=%v\n", err)

	// 7c：带 force 抢
	err = c.Patch(ctx, teamB, client.Apply, client.FieldOwner("team-b-operator"), client.ForceOwnership)
	fmt.Printf("  7c team-b-operator 带 ForceOwnership 再抢 → err=%v\n", err)

	var ssaAfter corev1.Service
	must(c.Get(ctx, types.NamespacedName{Namespace: "probe", Name: "probe-ssa"}, &ssaAfter))
	fmt.Printf("     抢失败之后的 ports 项数 = %d（第一项 = %+v）\n",
		len(ssaAfter.Spec.Ports), ssaAfter.Spec.Ports[0])

	// 7d：只抢 map 字段（selector），演示所有权转移
	teamC := &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: "probe-ssa", Namespace: "probe"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "probe", "team": "b"},
		},
	}
	err = c.Patch(ctx, teamC, client.Apply, client.FieldOwner("team-b-operator"), client.ForceOwnership)
	fmt.Printf("  7d team-b-operator 抢 selector（map 字段）+ ForceOwnership → err=%v\n", err)

	var ssaFinal corev1.Service
	must(c.Get(ctx, types.NamespacedName{Namespace: "probe", Name: "probe-ssa"}, &ssaFinal))
	fmt.Printf("     selector = %v，ports 项数 = %d\n", ssaFinal.Spec.Selector, len(ssaFinal.Spec.Ports))
	show("     抢成功之后的 managedFields（两个 manager 各拥有哪些字段）", ssaFinal.ManagedFields)

	// =====================================================================
	// 实验 8：同一个 manager 改「列表项的键」，旧项会被 prune 掉吗？
	//   用来判定 SSA 的删除语义来自「替换」还是来自「这个 manager 不再提交它」。
	// =====================================================================
	fmt.Println("\n########## 实验 8：SSA 的 prune 语义 ##########")

	ssaPorts := func(port int32) *corev1.Service {
		return &corev1.Service{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
			ObjectMeta: metav1.ObjectMeta{Name: "probe-ssa2", Namespace: "probe"},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{"app": "probe"},
				Ports: []corev1.ServicePort{{
					Name: "http", Port: port, TargetPort: intstr.FromInt32(port),
				}},
			},
		}
	}
	report := func(tag string) {
		var s corev1.Service
		must(c.Get(ctx, types.NamespacedName{Namespace: "probe", Name: "probe-ssa2"}, &s))
		ports := make([]int32, 0, len(s.Spec.Ports))
		for _, p := range s.Spec.Ports {
			ports = append(ports, p.Port)
		}
		fmt.Printf("     %s → ports=%v，manager 数=%d\n", tag, ports, len(s.ManagedFields))
	}

	err = c.Patch(ctx, ssaPorts(8080), client.Apply, client.FieldOwner("team-a-operator"))
	fmt.Printf("  8a 同一个 manager 首次 Apply（port 8080）→ err=%v\n", err)
	report("8a 之后")

	err = c.Patch(ctx, ssaPorts(9090), client.Apply, client.FieldOwner("team-a-operator"))
	fmt.Printf("  8b 同一个 manager 再 Apply（port 9090）→ err=%v\n", err)
	report("8b 之后")

	var ssa2 corev1.Service
	must(c.Get(ctx, types.NamespacedName{Namespace: "probe", Name: "probe-ssa2"}, &ssa2))
	show("     8b 之后的 managedFields", ssa2.ManagedFields)

	// =====================================================================
	// 实验 9：env 改名 —— 同一个 manager vs 不同 manager
	//   env 的 listMapKey 是 name（对应 Shortlink.spec.env 的 CRD 标记），
	//   所以「改名」等于换了一个实体。
	// =====================================================================
	fmt.Println("\n########## 实验 9：env 改名 ##########")

	envDep := func(env []corev1.EnvVar) *appsv1.Deployment {
		return &appsv1.Deployment{
			TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
			ObjectMeta: metav1.ObjectMeta{Name: "probe-env", Namespace: "probe"},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "probe"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "probe"}},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name: "c", Image: "nginx:1.27", Env: env,
						}},
					},
				},
			},
		}
	}
	reportEnv := func(tag string) {
		var d appsv1.Deployment
		must(c.Get(ctx, types.NamespacedName{Namespace: "probe", Name: "probe-env"}, &d))
		got := make([]string, 0, 2)
		for _, e := range d.Spec.Template.Spec.Containers[0].Env {
			got = append(got, e.Name+"="+e.Value)
		}
		fmt.Printf("     %s → env=%v（%d 项）\n", tag, got, len(got))
	}

	err = c.Patch(ctx, envDep([]corev1.EnvVar{{Name: "LOG_LEVEL", Value: "info"}}),
		client.Apply, client.FieldOwner("team-a-operator"))
	fmt.Printf("  9a team-a 首次 Apply（env: LOG_LEVEL）→ err=%v\n", err)
	reportEnv("9a 之后")

	err = c.Patch(ctx, envDep([]corev1.EnvVar{{Name: "LOG", Value: "debug"}}),
		client.Apply, client.FieldOwner("team-a-operator"))
	fmt.Printf("  9b team-a 自己改名（LOG_LEVEL → LOG）→ err=%v\n", err)
	reportEnv("9b 之后")

	err = c.Patch(ctx, envDep([]corev1.EnvVar{{Name: "LOG2", Value: "team-b"}}),
		client.Apply, client.FieldOwner("team-b-operator"))
	fmt.Printf("  9c team-b 加一项（LOG2）→ err=%v\n", err)
	reportEnv("9c 之后")

	fmt.Println("\n完成。")
}
