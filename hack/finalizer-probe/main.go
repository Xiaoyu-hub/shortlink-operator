// finalizer-probe 回答一个问题：
//
//	Shortlink 控制器现在用 For(&Shortlink{}, builder.WithPredicates(
//	predicate.GenerationChangedPredicate{})) —— Day 6 加了 finalizer 之后，
//	控制器还能看到「删除开始」（deletionTimestamp 被设置）这个事件吗？
//
// 做法：对真 apiserver + 真 CRD 逐步操作，记录每步的 metadata.generation，
// 并把每一步的 old/new 对象直接喂给 GenerationChangedPredicate，看它放不放行。
//
// 用法：go run ./hack/finalizer-probe
package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	shortlinkv1alpha1 "github.com/Xiaoyu-hub/shortlink-operator/api/v1alpha1"
)

const (
	objName = "gen-probe"
	objNS   = "finalizer-probe"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	assets, err := filepath.Abs(filepath.Join("bin", "k8s", "1.37.0-windows-amd64"))
	must(err)
	env := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{filepath.Join("config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	must(err)
	defer func() { _ = env.Stop() }()

	must(shortlinkv1alpha1.AddToScheme(scheme.Scheme))
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	must(err)

	must(c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: objNS}}))

	key := types.NamespacedName{Namespace: objNS, Name: objName}
	pred := predicate.GenerationChangedPredicate{}

	get := func() *shortlinkv1alpha1.Shortlink {
		var sl shortlinkv1alpha1.Shortlink
		must(c.Get(ctx, key, &sl))
		return &sl
	}
	verdict := func(stage string, old, new *shortlinkv1alpha1.Shortlink) {
		allowed := pred.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: new})
		fmt.Printf("     [predicate] %-30s generation %d → %d ，放行=%v\n",
			stage, old.Generation, new.Generation, allowed)
	}

	fmt.Println("########## CRD 的 generation：加 finalizer / 删除时怎么走 ##########")

	// 步骤 1：创建
	sl := &shortlinkv1alpha1.Shortlink{
		ObjectMeta: metav1.ObjectMeta{Name: objName, Namespace: objNS},
		Spec:       shortlinkv1alpha1.ShortlinkSpec{Image: "ghcr.io/xiaoyu-hub/shortlink:v0.1.0"},
	}
	must(c.Create(ctx, sl))
	fmt.Printf("  1. 创建 CR                   → generation=%d  spec.port=%d（API Server 补的默认值）\n",
		sl.Generation, sl.Spec.Port)

	// 步骤 2：改 spec
	old2 := get()
	new2 := old2.DeepCopy()
	new2.Spec.Port = 9090
	must(c.Update(ctx, new2))
	got2 := get()
	fmt.Printf("  2. 改 spec.port              → generation=%d\n", got2.Generation)
	verdict("改 spec", old2, got2)

	// 步骤 3：只加 finalizer（纯 metadata 变化）
	old3 := get()
	new3 := old3.DeepCopy()
	new3.Finalizers = append(new3.Finalizers, "shortlink.dev/cleanup")
	must(c.Update(ctx, new3))
	got3 := get()
	fmt.Printf("  3. 只加 finalizer            → generation=%d  finalizers=%v\n",
		got3.Generation, got3.Finalizers)
	verdict("加 finalizer", old3, got3)

	// 步骤 4：delete（finalizer 在 → 只设置 deletionTimestamp，对象不消失）
	old4 := get()
	must(c.Delete(ctx, old4.DeepCopy()))
	got4 := get()
	fmt.Printf("  4. delete（finalizer 在）    → generation=%d  deletionTimestamp=%v\n",
		got4.Generation, got4.DeletionTimestamp != nil)
	verdict("删除开始（设置 deletionTimestamp）", old4, got4)

	// 步骤 5：移除 finalizer，对象才真正消失
	old5 := get()
	new5 := old5.DeepCopy()
	new5.Finalizers = nil
	must(c.Update(ctx, new5))
	var after shortlinkv1alpha1.Shortlink
	err = c.Get(ctx, key, &after)
	fmt.Printf("  5. 移除 finalizer            → Get err IsNotFound=%v\n", apierrors.IsNotFound(err))
}
