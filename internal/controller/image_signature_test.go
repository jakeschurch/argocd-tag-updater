package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

// fakeRegistry serves tags/list and manifest HEADs for one repository.
type fakeRegistry struct {
	mu        sync.Mutex
	manifests map[string]string // tag -> digest
	failTag   string            // HEAD for this tag answers 500
	heads     map[string]int    // tag -> HEAD count
}

func (f *fakeRegistry) set(tag, digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.manifests[tag] = digest
}

func (f *fakeRegistry) headCount(tag string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.heads[tag]
}

func (f *fakeRegistry) totalHeads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, count := range f.heads {
		total += count
	}
	return total
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	const prefix = "/v2/foundry/app/"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == prefix+"tags/list":
		tags := []string{}
		for tag := range f.manifests {
			tags = append(tags, tag)
		}
		_ = json.NewEncoder(w).Encode(map[string][]string{"tags": tags})
	case r.Method == http.MethodHead && strings.HasPrefix(r.URL.Path, prefix+"manifests/"):
		tag := strings.TrimPrefix(r.URL.Path, prefix+"manifests/")
		f.heads[tag]++
		if tag == f.failTag {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		digest, ok := f.manifests[tag]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Docker-Content-Digest", digest)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func signedMarkerFor(digest string) string { return strings.ReplaceAll(digest, ":", "-") + ".signed" }

func TestReconcileRequireSignedMarker(t *testing.T) {
	for _, key := range []string{"GIT_TOKEN", "GIT_SSH_KEY_FILE", "GIT_USERNAME", "GIT_PASSWORD"} {
		t.Setenv(key, "")
	}
	digestOld := "sha256:" + strings.Repeat("a", 64)
	digestNew := "sha256:" + strings.Repeat("b", 64)
	const oldTag, newTag = "app.main.20261107140000.aaaaaa", "app.main.20261107141002.1dd98a"

	type step struct {
		before    func(*fakeRegistry)
		wantError bool
		wantTag   string // manifest image tag after the step
		wantHold  bool   // SignatureVerified False / AwaitingSignature
	}
	for _, tc := range []struct {
		name          string
		requireSigned v1.SignatureMode
		steps         []step
	}{
		{
			name:          "signed tag is written",
			requireSigned: v1.SignatureModeMarker,
			steps: []step{{
				before:  func(f *fakeRegistry) { f.set(signedMarkerFor(digestNew), digestNew) },
				wantTag: newTag,
			}},
		},
		{
			name:          "unsigned tag is held and never skipped",
			requireSigned: v1.SignatureModeMarker,
			steps: []step{
				{wantTag: oldTag, wantHold: true},
				{wantTag: oldTag, wantHold: true},
			},
		},
		{
			name:          "marker appearing later deploys the held tag",
			requireSigned: v1.SignatureModeMarker,
			steps: []step{
				{wantTag: oldTag, wantHold: true},
				{before: func(f *fakeRegistry) { f.set(signedMarkerFor(digestNew), digestNew) }, wantTag: newTag},
			},
		},
		{
			name:          "registry error on the candidate writes nothing",
			requireSigned: v1.SignatureModeMarker,
			steps: []step{{
				before:    func(f *fakeRegistry) { f.failTag = newTag; f.set(signedMarkerFor(digestNew), digestNew) },
				wantError: true, wantTag: oldTag,
			}},
		},
		{
			name:          "registry error on the marker writes nothing",
			requireSigned: v1.SignatureModeMarker,
			steps: []step{{
				before:    func(f *fakeRegistry) { f.failTag = signedMarkerFor(digestNew) },
				wantError: true, wantTag: oldTag,
			}},
		},
		{
			name: "unset keeps tag-only behavior",
			steps: []step{{
				wantTag: newTag,
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := &fakeRegistry{manifests: map[string]string{oldTag: digestOld, newTag: digestNew}, heads: map[string]int{}}
			server := httptest.NewServer(registry)
			defer server.Close()
			remote, repository, initial := signatureManifestRepo(t, oldTag)
			updater := signatureUpdater(server.URL+"/foundry/app", remote, tc.requireSigned, oldTag, initial)
			updater.Spec.Interval = metav1.Duration{Duration: 10 * time.Minute}
			storage := &digestTestClient{updater: updater}
			reconciler := &TagUpdaterReconciler{Client: storage}
			request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "app", Namespace: "argocd"}}
			for index, current := range tc.steps {
				if current.before != nil {
					current.before(registry) // no request is in flight between reconciles
				}
				result, err := reconciler.Reconcile(context.Background(), request)
				// Registry errors are transient: surfaced on Ready, retried fast, never returned.
				if err != nil {
					t.Fatalf("step %d: unexpected error %v", index, err)
				}
				if fast := result.RequeueAfter < updater.Spec.Interval.Duration; fast != (current.wantError || current.wantHold) {
					t.Fatalf("step %d: requeue %s with interval %s; fast=%v, want %v", index, result.RequeueAfter, updater.Spec.Interval.Duration, fast, current.wantError || current.wantHold)
				}
				if got := manifestTag(t, repository); got != current.wantTag {
					t.Fatalf("step %d: manifest tag %s, want %s", index, got, current.wantTag)
				}
				if wantDigest := map[string]string{oldTag: "", newTag: digestNew}[current.wantTag]; tc.requireSigned != "" && manifestDigest(t, repository) != wantDigest {
					t.Fatalf("step %d: manifest digest %q, want %q", index, manifestDigest(t, repository), wantDigest)
				}
				if updater.Status.LastTag != current.wantTag {
					t.Fatalf("step %d: status.lastTag %s, want %s", index, updater.Status.LastTag, current.wantTag)
				}
				if tc.requireSigned != "" && current.wantTag == newTag && updater.Status.LastSignedDigest != digestNew {
					t.Fatalf("step %d: status.lastSignedDigest %q, want %q", index, updater.Status.LastSignedDigest, digestNew)
				}
				if len(updater.Status.SkippedTags) != 0 {
					t.Fatalf("step %d: unsigned tag must not be skipped: %v", index, updater.Status.SkippedTags)
				}
				condition := meta.FindStatusCondition(updater.Status.Conditions, signatureCondition)
				if current.wantHold {
					if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "AwaitingSignature" || updater.Status.AwaitingSignatureTag != newTag {
						t.Fatalf("step %d: hold not surfaced: %+v awaiting=%q", index, condition, updater.Status.AwaitingSignatureTag)
					}
				} else if updater.Status.AwaitingSignatureTag != "" {
					t.Fatalf("step %d: awaitingSignatureTag not cleared", index)
				}
				if !current.wantError && !current.wantHold && tc.requireSigned != "" && (condition == nil || condition.Status != metav1.ConditionTrue) {
					t.Fatalf("step %d: SignatureVerified not True: %+v", index, condition)
				}
			}
			if tc.requireSigned == "" && registry.totalHeads() != 0 {
				t.Fatalf("unset requireSigned issued %d manifest requests", registry.totalHeads())
			}
		})
	}
}

func TestRequireSignedCachesSignedDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("d", 64)
	registry := &fakeRegistry{manifests: map[string]string{"app.1": digest, signedMarkerFor(digest): digest}, heads: map[string]int{}}
	server := httptest.NewServer(registry)
	defer server.Close()
	updater := &v1.TagUpdater{Spec: v1.TagUpdaterSpec{Source: v1.SourceSpec{Type: v1.SourceTypeOCI, Repo: server.URL + "/foundry/app", RequireSigned: v1.SignatureModeMarker}}}
	reconciler := &TagUpdaterReconciler{}
	for range 3 {
		signed, got, err := reconciler.verifySigned(context.Background(), updater, "", "app.1", map[string]string{})
		if err != nil || !signed || got != digest {
			t.Fatalf("signed=%v digest=%s err=%v", signed, got, err)
		}
	}
	if registry.headCount(signedMarkerFor(digest)) != 1 {
		t.Fatalf("marker checked %d times, want 1", registry.headCount(signedMarkerFor(digest)))
	}
	if registry.headCount("app.1") != 3 {
		t.Fatalf("mutable tag must be resolved every time, got %d", registry.headCount("app.1"))
	}
}

func TestRollbackRendersRecordedSignedDigest(t *testing.T) {
	for _, key := range []string{"GIT_TOKEN", "GIT_SSH_KEY_FILE", "GIT_USERNAME", "GIT_PASSWORD"} {
		t.Setenv(key, "")
	}
	digestOld := "sha256:" + strings.Repeat("a", 64)
	digestNew := "sha256:" + strings.Repeat("b", 64)
	const oldTag, newTag = "app.main.20261107140000.aaaaaa", "app.main.20261107141002.1dd98a"
	for _, tc := range []struct {
		name     string
		previous string
		wantErr  bool
	}{{"with recorded history", digestOld, false}, {"without history fails closed", "", true}} {
		t.Run(tc.name, func(t *testing.T) {
			// The registry must not be consulted: the tag may have moved since it deployed.
			registry := &fakeRegistry{manifests: map[string]string{}, heads: map[string]int{}}
			server := httptest.NewServer(registry)
			defer server.Close()
			remote, repository, initial := signatureManifestRepo(t, newTag)
			updater := signatureUpdater(server.URL+"/foundry/app", remote, v1.SignatureModeMarker, newTag, initial)
			updater.Spec.ArgoCDApp = &v1.ArgoCDAppRef{Name: "app"}
			updater.Spec.Rollback = &v1.RollbackSpec{Enabled: true}
			updater.Status.BaselineHealthy = true
			updater.Status.PreviousTag = oldTag
			updater.Status.WatchingTag = newTag
			updater.Status.LastSignedDigest = digestNew
			updater.Status.PreviousSignedDigest = tc.previous
			reconciler := &TagUpdaterReconciler{Client: &digestTestClient{updater: updater}}
			err := reconciler.rollbackWithStatus(context.Background(), updater)
			if (err != nil) != tc.wantErr {
				t.Fatalf("rollback error %v, want error %v", err, tc.wantErr)
			}
			if registry.totalHeads() != 0 {
				t.Fatalf("rollback queried the registry %d times", registry.totalHeads())
			}
			if tc.wantErr {
				if manifestTag(t, repository) != newTag {
					t.Fatal("failed rollback moved the manifest")
				}
				condition := meta.FindStatusCondition(updater.Status.Conditions, "RollbackReady")
				if condition == nil || condition.Reason != "RollbackWriteFailed" || !strings.Contains(condition.Message, "previousSignedDigest") {
					t.Fatalf("fail-closed rollback not surfaced: %+v", condition)
				}
				return
			}
			if manifestTag(t, repository) != oldTag || manifestDigest(t, repository) != digestOld {
				t.Fatalf("rollback wrote tag %s digest %s", manifestTag(t, repository), manifestDigest(t, repository))
			}
			if updater.Status.LastSignedDigest != digestOld || updater.Status.WatchingTag != "" {
				t.Fatalf("rollback status not restored: %+v", updater.Status)
			}
		})
	}
}

func TestReconcileRequireSignedGitSourceUsesDigestRepo(t *testing.T) {
	for _, key := range []string{"GIT_TOKEN", "GIT_SSH_KEY_FILE", "GIT_USERNAME", "GIT_PASSWORD"} {
		t.Setenv(key, "")
	}
	digestNew := "sha256:" + strings.Repeat("b", 64)
	const oldTag, newTag = "app.main.20261107140000.aaaaaa", "app.main.20261107141002.1dd98a"
	registry := &fakeRegistry{manifests: map[string]string{newTag: digestNew, signedMarkerFor(digestNew): digestNew}, heads: map[string]int{}}
	server := httptest.NewServer(registry)
	defer server.Close()
	remote, repository, initial := signatureManifestRepo(t, oldTag)
	for _, tag := range []string{oldTag, newTag} {
		if _, err := repository.CreateTag(tag, initial, nil); err != nil {
			t.Fatal(err)
		}
	}
	updater := signatureUpdater("", remote, v1.SignatureModeMarker, oldTag, initial)
	updater.Spec.Source.Type = v1.SourceTypeGit
	updater.Spec.Source.Repo = remote
	updater.Spec.Source.DigestRepo = server.URL + "/foundry/app"
	updater.Spec.Source.ResolveDigest = true
	reconciler := &TagUpdaterReconciler{Client: &digestTestClient{updater: updater}}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "app", Namespace: "argocd"}}); err != nil {
		t.Fatal(err)
	}
	if manifestTag(t, repository) != newTag || manifestDigest(t, repository) != digestNew {
		t.Fatalf("git source wrote tag %s digest %s", manifestTag(t, repository), manifestDigest(t, repository))
	}
	if registry.headCount(signedMarkerFor(digestNew)) != 1 {
		t.Fatalf("marker not checked in digestRepo")
	}
	if updater.Status.LastImageDigest != digestNew || updater.Status.LastSignedDigest != digestNew {
		t.Fatalf("digests not recorded: %+v", updater.Status)
	}
}

func TestVerifySignedRejectsTagMovedSinceDigestResolution(t *testing.T) {
	digest := "sha256:" + strings.Repeat("d", 64)
	registry := &fakeRegistry{manifests: map[string]string{"app.1": digest, signedMarkerFor(digest): digest}, heads: map[string]int{}}
	server := httptest.NewServer(registry)
	defer server.Close()
	updater := &v1.TagUpdater{Spec: v1.TagUpdaterSpec{Source: v1.SourceSpec{Type: v1.SourceTypeGit, DigestRepo: server.URL + "/foundry/app", RequireSigned: v1.SignatureModeMarker}}}
	data := map[string]string{"image_digest": "sha256:" + strings.Repeat("e", 64)}
	signed, _, err := (&TagUpdaterReconciler{}).verifySigned(context.Background(), updater, "", "app.1", data)
	if err == nil || signed || !strings.Contains(err.Error(), "moved") {
		t.Fatalf("signed=%v err=%v, want tag-moved error", signed, err)
	}
	if registry.headCount(signedMarkerFor(digest)) != 0 {
		t.Fatal("marker checked for a digest that will not be written")
	}
}

func TestValidateRequireSigned(t *testing.T) {
	for _, tc := range []struct {
		spec v1.SourceSpec
		ok   bool
	}{
		{v1.SourceSpec{Type: v1.SourceTypeNix}, true},
		{v1.SourceSpec{Type: v1.SourceTypeOCI, RequireSigned: v1.SignatureModeMarker}, true},
		{v1.SourceSpec{Type: v1.SourceTypeGit, RequireSigned: v1.SignatureModeMarker, DigestRepo: "harbor/foundry/app"}, true},
		{v1.SourceSpec{Type: v1.SourceTypeGit, RequireSigned: v1.SignatureModeMarker}, false},
		{v1.SourceSpec{Type: v1.SourceTypeNix, RequireSigned: v1.SignatureModeMarker}, false},
		{v1.SourceSpec{Type: v1.SourceTypeOCI, RequireSigned: "cosign"}, false},
	} {
		if err := validateRequireSigned(tc.spec); (err == nil) != tc.ok {
			t.Errorf("%+v: err %v, want ok=%v", tc.spec, err, tc.ok)
		}
	}
}

func signatureManifestRepo(t *testing.T, tag string) (string, *git.Repository, plumbing.Hash) {
	t.Helper()
	source := t.TempDir()
	repository, err := git.PlainInit(source, false)
	if err != nil {
		t.Fatal(err)
	}
	manifest := "apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: app\n  namespace: argocd\nspec:\n  image:\n    tag: " + tag + "\n"
	if err = os.WriteFile(filepath.Join(source, "app.yaml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	tree, _ := repository.Worktree()
	_, _ = tree.Add("app.yaml")
	commit, err := tree.Commit("initial", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	bare, err := git.PlainClone(remote, true, &git.CloneOptions{URL: source})
	if err != nil {
		t.Fatal(err)
	}
	return remote, bare, commit
}

func signatureUpdater(registry, remote string, mode v1.SignatureMode, lastTag string, commit plumbing.Hash) *v1.TagUpdater {
	patches := []v1.PatchSpec{{Field: "spec.image.tag", Template: "{{ .tag }}"}}
	if mode != "" {
		patches = append(patches, v1.PatchSpec{Field: "spec.image.digest", Template: "{{ .signed_digest }}"})
	}
	return &v1.TagUpdater{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "argocd", Generation: 1},
		Spec: v1.TagUpdaterSpec{
			Source:    v1.SourceSpec{Type: v1.SourceTypeOCI, Repo: registry, TagPattern: `^app\.(?P<branch>main)\.(?P<n>\d{14})\.(?P<sha>[0-9a-f]{6})$`, RequireSigned: mode},
			WriteBack: v1.WriteBackSpec{Repo: remote, Branch: "master", Path: "app.yaml", CredentialsSecretRef: v1.LocalObjectReference{Name: "test-credentials"}},
			Targets:   []v1.TargetSpec{{APIVersion: "argoproj.io/v1alpha1", Kind: "Application", Name: "app", Namespace: "argocd", Patches: patches}},
		},
		Status: v1.TagUpdaterStatus{LastTag: lastTag, LastWriteCommit: commit.String(), ObservedGeneration: 1},
	}
}

func manifestTag(t *testing.T, repository *git.Repository) string {
	t.Helper()
	return manifestValue(t, repository, "tag: ")
}

func manifestDigest(t *testing.T, repository *git.Repository) string {
	t.Helper()
	return manifestValue(t, repository, "digest: ")
}

func manifestValue(t *testing.T, repository *git.Repository, key string) string {
	t.Helper()
	head, err := repository.Head()
	if err != nil {
		t.Fatal(err)
	}
	commit, _ := repository.CommitObject(head.Hash())
	file, _ := commit.File("app.yaml")
	text, _ := file.Contents()
	for _, line := range strings.Split(text, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), key); ok {
			return value
		}
	}
	return ""
}
