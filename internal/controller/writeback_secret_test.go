package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
)

type secretDataClient struct {
	client.Client
	data map[string][]byte
}

func (c secretDataClient) Get(_ context.Context, _ types.NamespacedName, out client.Object, _ ...client.GetOption) error {
	out.(*corev1.Secret).Data = c.data
	return nil
}

func secretUpdater() *v1alpha1.TagUpdater {
	updater := &v1alpha1.TagUpdater{}
	updater.Namespace = "argocd"
	updater.Spec.WriteBack.CredentialsSecretRef = v1alpha1.LocalObjectReference{Name: "writeback"}
	return updater
}

func TestResolveWriteBackCredentialsFromSecretTokenAndUsername(t *testing.T) {
	r := &TagUpdaterReconciler{Client: secretDataClient{data: map[string][]byte{
		"token": []byte("tok"), "username": []byte("margo-robot"),
	}}}
	credentials, err := r.resolveWriteBackCredentials(context.Background(), secretUpdater())
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Token != "tok" || credentials.Username != "margo-robot" || len(credentials.SSHPrivateKey) != 0 {
		t.Fatalf("token credentials not loaded: %#v", credentials)
	}
}

func TestResolveWriteBackCredentialsFromSecretRejectsUsernameOnly(t *testing.T) {
	r := &TagUpdaterReconciler{Client: secretDataClient{data: map[string][]byte{"username": []byte("margo-robot")}}}
	if _, err := r.resolveWriteBackCredentials(context.Background(), secretUpdater()); err == nil {
		t.Fatal("secret with only a username was accepted")
	}
}
