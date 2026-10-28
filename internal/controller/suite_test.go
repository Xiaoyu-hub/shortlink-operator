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
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	shortlinkv1alpha1 "github.com/Xiaoyu-hub/shortlink-operator/api/v1alpha1"
	// +kubebuilder:scaffold:imports
)

// 这些测试用 Ginkgo（BDD 风格的 Go 测试框架）。想了解 Ginkgo 用法可看
// http://onsi.github.io/ginkgo/。
//
// 与 Day 5 前的骨架不同：这里真正启动了 manager（控制循环），所以测试是
// 「创建 CR → Deployment/Service 被 watch 驱动自动调和」，而不是手调 Reconcile。
// 这就是 envtest 真控制循环：etcd + kube-apiserver 都是真实的，只是没有
// kube-controller-manager / scheduler / GC 这些集群组件（其影响见 day05 文档）。

var (
	ctx        context.Context
	cancel     context.CancelFunc
	testEnv    *envtest.Environment
	cfg        *rest.Config
	k8sManager ctrl.Manager
	k8sClient  client.Client
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	var err error
	err = shortlinkv1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	// +kubebuilder:scaffold:scheme

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	// 取出找到的第一个二进制目录，这样在 IDE 里直接跑测试也能跑起来
	if getFirstFoundEnvTestBinaryDir() != "" {
		testEnv.BinaryAssetsDirectory = getFirstFoundEnvTestBinaryDir()
	}

	// cfg 是本文件里的全局变量，后面的用例直接复用。
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	By("starting the manager with the Shortlink controller (real control loop)")
	k8sManager, err = ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme,
		// 测试里关掉 metrics 端口，避免端口冲突；健康探针默认不启用
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	Expect(err).NotTo(HaveOccurred())

	err = (&ShortlinkReconciler{
		Client: k8sManager.GetClient(),
		Scheme: k8sManager.GetScheme(),
	}).SetupWithManager(k8sManager)
	Expect(err).NotTo(HaveOccurred())

	go func() {
		defer GinkgoRecover()
		Expect(k8sManager.Start(ctx)).NotTo(HaveOccurred())
	}()

	// 等 informer 缓存同步完成，否则立刻写 CR 可能撞上「还没开始 watch」的窗口
	By("waiting for informer caches to sync")
	Expect(k8sManager.GetCache().WaitForCacheSync(ctx)).To(BeTrue())

	// 测试用的直连客户端（不走缓存，读到的就是 API Server 的最新状态）
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})

// getFirstFoundEnvTestBinaryDir 在给定目录下找出第一个二进制目录。
// 基于 envtest 的测试依赖一组特定二进制（kube-apiserver、etcd、kubectl），
// 这些二进制通常放在 controller-runtime 约定的路径下。若不走 Makefile 目标、
// 而是直接跑测试（例如在 IDE 里点运行），就必须显式配置 BinaryAssetsDirectory。
//
// 这个函数把这件事简化成「自己找个二进制目录」，效果等价于设置
// KUBEBUILDER_ASSETS 环境变量。要保证二进制真的就位，事先跑一次 'make setup-envtest'。
func getFirstFoundEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		logf.Log.Error(err, "Failed to read directory", "path", basePath)
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
