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

// Package v1alpha1 定义 shortlink v1alpha1 这一 API 组的 API 模式（Schema）。
// +kubebuilder:object:generate=true
// +groupName=shortlink.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// SchemeGroupVersion 是注册这些对象时使用的组版本。
	// applyconfiguration 生成器（例如 controller-gen）会引用这个名字。
	SchemeGroupVersion = schema.GroupVersion{Group: "shortlink.dev", Version: "v1alpha1"}

	// GroupVersion 是 SchemeGroupVersion 的别名，保留只是为了向后兼容。
	GroupVersion = SchemeGroupVersion

	// SchemeBuilder 负责把本包的 Go 类型登记进 GroupVersionKind 对应的 scheme。
	SchemeBuilder = runtime.NewSchemeBuilder(func(scheme *runtime.Scheme) error {
		metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
		return nil
	})

	// AddToScheme 把本组版本的类型加入调用方给出的 scheme。
	AddToScheme = SchemeBuilder.AddToScheme
)
