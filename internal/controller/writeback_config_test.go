package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
)

func TestValidateWriteBackRequiresDestination(t *testing.T) {
	err := validateWriteBack(v1alpha1.WriteBackSpec{})
	if err == nil {
		t.Fatal("empty write-back config was accepted")
	}
	for _, field := range []string{"spec.writeBack", "repo", "branch", "path"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error %q does not mention %q", err, field)
		}
	}
}

func TestResolveWriteBackCredentialsUsesMountedSSHKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	knownHostsPath := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(keyPath, []byte("private-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(knownHostsPath, []byte("github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSH_KEY_FILE", keyPath)
	t.Setenv("GIT_KNOWN_HOSTS_FILE", knownHostsPath)

	credentials, err := (&TagUpdaterReconciler{}).resolveWriteBackCredentials(context.Background(), &v1alpha1.TagUpdater{})
	if err != nil {
		t.Fatalf("resolve mounted credentials: %v", err)
	}
	if string(credentials.SSHPrivateKey) != "private-key" || string(credentials.KnownHosts) != "github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI" {
		t.Fatalf("mounted credentials not loaded: %#v", credentials)
	}
}

func TestValidateWriteBackAllowsMountedCredentials(t *testing.T) {
	err := validateWriteBack(v1alpha1.WriteBackSpec{
		Repo: "git@example.com:org/manifests.git", Branch: "main", Path: "apps/example.yaml",
	})
	if err != nil {
		t.Fatalf("write-back destination using mounted credentials rejected: %v", err)
	}
}

func TestValidateWriteBackAcceptsCompleteDestination(t *testing.T) {
	err := validateWriteBack(v1alpha1.WriteBackSpec{
		Repo: "git@example.com:org/manifests.git", Branch: "main", Path: "apps/example.yaml",
		CredentialsSecretRef: v1alpha1.LocalObjectReference{Name: "git-credentials"},
	})
	if err != nil {
		t.Fatalf("complete write-back config rejected: %v", err)
	}
}

func TestWriteBackCurrentRequiresTagAndRemoteHead(t *testing.T) {
	status := v1alpha1.TagUpdaterStatus{LastTag: "v2", LastWriteCommit: "abc"}
	if !writeBackCurrent("v2", status, "abc") {
		t.Fatal("matching tag and remote head were not current")
	}
	if writeBackCurrent("v3", status, "abc") || writeBackCurrent("v2", status, "def") {
		t.Fatal("stale tag or remote head was treated as current")
	}
}

func TestValidateFieldOwnershipRejectsDuplicateWriter(t *testing.T) {
	updaters := []v1alpha1.TagUpdater{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "argocd", Name: "first"}, Spec: ownershipSpec("apps/foundry.yaml", "spec.sources.0.helm.valuesObject.nixMount.storePath")},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "argocd", Name: "second"}, Spec: ownershipSpec("apps/foundry.yaml", "spec.sources.0.helm.valuesObject.nixMount.storePath")},
	}
	if err := validateFieldOwnership(updaters); err == nil || !strings.Contains(err.Error(), "both argocd/first and argocd/second") {
		t.Fatalf("duplicate writer was accepted: %v", err)
	}
}

func TestValidateFieldOwnershipAllowsSeparateFields(t *testing.T) {
	updaters := []v1alpha1.TagUpdater{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "argocd", Name: "store-path"}, Spec: ownershipSpec("apps/foundry.yaml", "spec.sources.0.helm.valuesObject.nixMount.storePath")},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "argocd", Name: "flake-ref"}, Spec: ownershipSpec("apps/foundry.yaml", "spec.sources.0.helm.valuesObject.nixMount.flakeRef")},
	}
	if err := validateFieldOwnership(updaters); err != nil {
		t.Fatalf("separate fields rejected: %v", err)
	}
}

func ownershipSpec(path, field string) v1alpha1.TagUpdaterSpec {
	return v1alpha1.TagUpdaterSpec{
		WriteBack: v1alpha1.WriteBackSpec{Repo: "git@example.com:org/manifests.git", Branch: "main", Path: path},
		Targets:   []v1alpha1.TargetSpec{{APIVersion: "argoproj.io/v1alpha1", Kind: "Application", Name: "foundry", Namespace: "argocd", Patches: []v1alpha1.PatchSpec{{Field: field}}}},
	}
}
