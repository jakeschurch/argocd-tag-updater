package controller

import (
	"strings"
	"testing"

	v1alpha1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
)

func TestValidateWriteBackRequiresDestination(t *testing.T) {
	err := validateWriteBack(v1alpha1.WriteBackSpec{})
	if err == nil {
		t.Fatal("empty write-back config was accepted")
	}
	for _, field := range []string{"spec.writeBack", "repo", "branch", "path", "credentialsSecretRef.name"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error %q does not mention %q", err, field)
		}
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
