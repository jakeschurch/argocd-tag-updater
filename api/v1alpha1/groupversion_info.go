// +groupName=updater.argocd.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var GroupVersion = schema.GroupVersion{Group: "updater.argocd.io", Version: "v1alpha1"}

func AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&TagUpdater{},
		&TagUpdaterList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
