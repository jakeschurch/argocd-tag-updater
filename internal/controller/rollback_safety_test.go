package controller

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	v1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var appResource = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

func safetyApp(health, rev string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application", "metadata": map[string]interface{}{"name": "workload", "namespace": "argocd"},
		"spec":   map[string]interface{}{"image": "v1"},
		"status": map[string]interface{}{"health": map[string]interface{}{"status": health}, "sync": map[string]interface{}{"revision": rev, "status": "Synced"}, "reconciledAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
	}}
}
func safetyHarness(t *testing.T, health string) (*TagUpdaterReconciler, *v1.TagUpdater, string) {
	t.Helper()
	remote, _, initial := multiPathRemote(t)
	tu := multiPathUpdater(remote, initial, "")
	tu.UID = types.UID("test-uid")
	tu.Spec.ArgoCDApp = &v1.ArgoCDAppRef{Name: "workload"}
	tu.Spec.Rollback = &v1.RollbackSpec{Enabled: true, Timeout: metav1.Duration{Duration: time.Minute}}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = coordinationv1.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1.TagUpdater{}).WithObjects(tu, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "test-credentials", Namespace: "argocd"}, Data: map[string][]byte{"token": []byte("test-token")}}).Build()
	apps := []runtime.Object{safetyApp(health, initial)}
	for _, name := range []string{"first", "second"} {
		app := safetyApp(health, initial)
		app.SetName(name)
		apps = append(apps, app)
	}
	r := &TagUpdaterReconciler{Client: c, APIReader: c, Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), apps...), Recorder: record.NewFakeRecorder(30)}
	return r, tu, remote
}
func loadSafety(t *testing.T, r *TagUpdaterReconciler, tu *v1.TagUpdater) {
	t.Helper()
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(tu), tu); err != nil {
		t.Fatal(err)
	}
}
func reconcileSafety(t *testing.T, r *TagUpdaterReconciler, tu *v1.TagUpdater) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tu)}); err != nil {
		t.Fatal(err)
	}
	loadSafety(t, r, tu)
}
func changeSafetyApp(t *testing.T, r *TagUpdaterReconciler, health, rev string) {
	t.Helper()
	var updater v1.TagUpdater
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "argocd", Name: "test"}, &updater); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"workload", "first", "second"} {
		app := safetyApp(health, rev)
		app.SetName(name)
		for _, field := range updater.Status.RollbackFields {
			if field.Name == name && field.Field == "spec.image" {
				value := field.After
				if updater.Status.WatchingRollback {
					value = field.Before
				}
				if value != nil {
					_ = unstructured.SetNestedField(app.Object, *value, "spec", "image")
				}
			}
		}
		if _, err := r.Dynamic.Resource(appResource).Namespace("argocd").Update(context.Background(), app, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUnhealthyBaselineNeverBlamesNewRelease(t *testing.T) {
	for _, health := range []string{"Degraded", "Missing"} {
		t.Run(health, func(t *testing.T) {
			r, tu, _ := safetyHarness(t, health)
			reconcileSafety(t, r, tu)
			if tu.Status.BaselineHealthy || tu.Status.WatchingTag != "v2" || len(tu.Status.RollbackFields) != 2 {
				t.Fatalf("baseline/snapshot not recorded: %+v", tu.Status)
			}
			changeSafetyApp(t, r, "Degraded", tu.Status.WatchingCommit)
			reconcileSafety(t, r, tu)
			r.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
			reconcileSafety(t, r, tu)
			if tu.Status.WatchingRollback || len(tu.Status.SkippedTags) > 0 || tu.Status.LastTag != "v2" {
				t.Fatalf("preexisting failure blamed on new release: %+v", tu.Status)
			}
			if c := meta.FindStatusCondition(tu.Status.Conditions, "RollbackReady"); c == nil || c.Reason != "UnhealthyBaseline" {
				t.Fatalf("missing suppression: %+v", c)
			}
			changeSafetyApp(t, r, "Healthy", tu.Status.WatchingCommit)
			reconcileSafety(t, r, tu)
			if tu.Status.WatchingTag != "" {
				t.Fatal("healthy release did not open gate")
			}
		})
	}
}

func TestRollbackRestoresWholeUnitAndWaitsForHealthy(t *testing.T) {
	r, tu, remote := safetyHarness(t, "Healthy")
	before := metricValue(rollbackCount.WithLabelValues(tu.Namespace, tu.Name))
	reconcileSafety(t, r, tu)
	if !tu.Status.BaselineHealthy {
		t.Fatal("Healthy baseline lost")
	}
	// Changes to templates after promotion cannot alter the recorded Healthy set.
	tu.Spec.Targets[0].Patches[0].Template = "changed-template"
	if err := r.Update(context.Background(), tu); err != nil {
		t.Fatal(err)
	}
	changeSafetyApp(t, r, "Degraded", tu.Status.WatchingCommit)
	reconcileSafety(t, r, tu)
	if !tu.Status.WatchingRollback || tu.Status.LastTag != "v1" || len(tu.Status.SkippedTags) != 1 || tu.Status.SkippedTags[0] != "v2" {
		t.Fatalf("rollback state %+v", tu.Status)
	}
	if metricValue(rollbackCount.WithLabelValues(tu.Namespace, tu.Name)) != before+1 {
		t.Fatal("rollback metric missing")
	}
	// Read the repository, proving both manifest files reverted, not just status.
	for _, path := range []string{"apps/first.yaml", "apps/second.yaml"} {
		if contents := safetyFile(t, remote, path); !strings.Contains(contents, "image: v1") {
			t.Fatalf("%s: %s", path, contents)
		}
	}
	lease := &coordinationv1.Lease{}
	if err := r.Get(context.Background(), leaseKey(tu), lease); err != nil {
		t.Fatal(err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		t.Fatal("rollback released lease before Healthy")
	}
	changeSafetyApp(t, r, "Healthy", tu.Status.WatchingCommit)
	reconcileSafety(t, r, tu)
	if tu.Status.WatchingTag != "" || len(tu.Status.SkippedTags) != 1 {
		t.Fatal("Healthy rollback reset the skip guard")
	}
	if err := r.Get(context.Background(), leaseKey(tu), lease); err != nil {
		t.Fatal(err)
	}
	if *lease.Spec.HolderIdentity != "" {
		t.Fatal("Healthy rollback did not release lease")
	}
	events := r.Recorder.(*record.FakeRecorder).Events
	found := false
	for len(events) > 0 {
		if strings.Contains(<-events, "RolledBack") {
			found = true
		}
	}
	if !found {
		t.Fatal("rollback event missing")
	}
}

func TestAppLeaseSerializesDifferentSourcesAndSurvivesRestart(t *testing.T) {
	r, first, _ := safetyHarness(t, "Healthy")
	second := first.DeepCopy()
	second.Name = "second"
	second.UID = "second-uid"
	second.ResourceVersion = ""
	second.Spec.Source.Repo = "different-source"
	second.Status = v1.TagUpdaterStatus{}
	if err := r.Create(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	// Distinct fields avoid the independent field-ownership rejection.
	second.Spec.Targets = nil
	if err := r.Update(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	reconcileSafety(t, r, first)
	restart := &TagUpdaterReconciler{Client: r.Client, APIReader: r.APIReader, Dynamic: r.Dynamic}
	acquired, err := restart.acquireApp(context.Background(), second)
	if err != nil || acquired {
		t.Fatalf("second writer admitted: %v %v", acquired, err)
	}
	requests := restart.enqueuePeers(context.Background(), first)
	if len(requests) != 2 {
		t.Fatalf("updater completion did not wake app peers: %v", requests)
	}
	changeSafetyApp(t, r, "Healthy", first.Status.WatchingCommit)
	reconcileSafety(t, r, first)
	acquired, err = restart.acquireApp(context.Background(), second)
	if err != nil || !acquired {
		t.Fatalf("queued updater not admitted after Healthy: %v %v", acquired, err)
	}
}

func TestConcurrentAppLeaseHasSingleWinner(t *testing.T) {
	r, first, _ := safetyHarness(t, "Healthy")
	second := first.DeepCopy()
	second.Name = "other"
	second.UID = "other"
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	errs := make(chan error, 2)
	for _, tu := range []*v1.TagUpdater{first, second} {
		wg.Add(1)
		go func(tu *v1.TagUpdater) {
			defer wg.Done()
			won, err := r.acquireApp(context.Background(), tu)
			results <- won
			errs <- err
		}(tu)
	}
	wg.Wait()
	close(results)
	close(errs)
	winners := 0
	for won := range results {
		if won {
			winners++
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d winners", winners)
	}
}

func TestAmbiguousPendingChangesNeverRollbackOrSkip(t *testing.T) {
	r, tu, remote := safetyHarness(t, "Healthy")
	reconcileSafety(t, r, tu)
	pin := tu.Status.LastWriteCommit
	other := tu.DeepCopy()
	other.Name = "bypass"
	other.ResourceVersion = ""
	other.UID = "bypass"
	if err := r.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	changeSafetyApp(t, r, "Degraded", pin)
	reconcileSafety(t, r, tu)
	if tu.Status.WatchingRollback || len(tu.Status.SkippedTags) > 0 || tu.Status.LastWriteCommit != pin {
		t.Fatal("ambiguous change was blamed")
	}
	if !meta.IsStatusConditionTrue(tu.Status.Conditions, "RollbackAmbiguous") {
		t.Fatal("ambiguity condition missing")
	}
	if metricValue(ambiguousGate.WithLabelValues(tu.Namespace, tu.Name)) != 1 {
		t.Fatal("ambiguity metric missing")
	}
	if !strings.Contains(safetyFile(t, remote, "apps/first.yaml"), "image: v2") {
		t.Fatal("ambiguous rollback changed git")
	}
}

func TestHoldPausesSelectionButAllowsPendingHealth(t *testing.T) {
	r, tu, remote := safetyHarness(t, "Healthy")
	tu.Annotations = map[string]string{holdAnnotation: "true"}
	if err := r.Update(context.Background(), tu); err != nil {
		t.Fatal(err)
	}
	reconcileSafety(t, r, tu)
	if tu.Status.LastTag != "v1" || !strings.Contains(safetyFile(t, remote, "apps/first.yaml"), "image: v1") {
		t.Fatal("held updater wrote")
	}
	delete(tu.Annotations, holdAnnotation)
	if err := r.Update(context.Background(), tu); err != nil {
		t.Fatal(err)
	}
	reconcileSafety(t, r, tu)
	tu.Annotations = map[string]string{holdAnnotation: "true"}
	if err := r.Update(context.Background(), tu); err != nil {
		t.Fatal(err)
	}
	changeSafetyApp(t, r, "Healthy", tu.Status.WatchingCommit)
	reconcileSafety(t, r, tu)
	if tu.Status.WatchingTag != "" {
		t.Fatal("hold blocked pending health observation")
	}
}

func safetyFile(t *testing.T, remote, path string) string {
	t.Helper()
	repo, err := git.PlainOpen(remote)
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	file, err := commit.File(path)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := file.Contents()
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func metricValue(metric prometheus.Metric) float64 {
	value := &dto.Metric{}
	if err := metric.Write(value); err != nil {
		panic(err)
	}
	if value.Counter != nil {
		return value.Counter.GetValue()
	}
	return value.Gauge.GetValue()
}

func TestHealthyBaselineTimeoutAndNewerBuilds(t *testing.T) {
	r, tu, remote := safetyHarness(t, "Healthy")
	reconcileSafety(t, r, tu)
	changeSafetyApp(t, r, "Progressing", tu.Status.WatchingCommit)
	reconcileSafety(t, r, tu)
	r.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	reconcileSafety(t, r, tu)
	if tu.Status.WatchingRollback || !tu.Status.WatchingDeployed {
		t.Fatal("deployed progressing release must survive the timeout")
	}
	r.now = nil
	changeSafetyApp(t, r, "Degraded", tu.Status.WatchingCommit)
	reconcileSafety(t, r, tu)
	if !tu.Status.WatchingRollback {
		t.Fatal("explicit failure did not roll back")
	}
	changeSafetyApp(t, r, "Healthy", tu.Status.WatchingCommit)
	reconcileSafety(t, r, tu)
	repo, err := git.PlainOpen(remote)
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	addReleaseTag(t, repo, "v3", head.Hash())
	reconcileSafety(t, r, tu)
	if tu.Status.LastTag != "v3" || tu.Status.WatchingTag != "v3" {
		t.Fatal("newer build was not attempted after rollback")
	}
	changeSafetyApp(t, r, "Degraded", tu.Status.WatchingCommit)
	reconcileSafety(t, r, tu)
	if len(tu.Status.SkippedTags) != 2 || !tu.Status.WatchingRollback {
		t.Fatalf("second bad tag not skipped: %+v", tu.Status)
	}
	changeSafetyApp(t, r, "Healthy", tu.Status.WatchingCommit)
	reconcileSafety(t, r, tu)
	head, _ = repo.Head()
	addReleaseTag(t, repo, "v4", head.Hash())
	reconcileSafety(t, r, tu)
	if tu.Status.LastTag != "v4" || len(tu.Status.SkippedTags) != 2 {
		t.Fatal("new build or permanent skip guard lost")
	}
}

func TestManagedAppRequiresFreshChildComparison(t *testing.T) {
	r, tu, _ := safetyHarness(t, "Healthy")
	tu.Spec.ManagingApp = &v1.ArgoCDAppRef{Name: "workload"}
	if err := r.Update(context.Background(), tu); err != nil {
		t.Fatal(err)
	}
	reconcileSafety(t, r, tu)
	for _, name := range []string{"workload", "first", "second"} {
		app := safetyApp("Healthy", tu.Status.WatchingCommit)
		app.SetName(name)
		_ = unstructured.SetNestedField(app.Object, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), "status", "reconciledAt")
		if _, err := r.Dynamic.Resource(appResource).Namespace("argocd").Update(context.Background(), app, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	reconcileSafety(t, r, tu)
	if tu.Status.WatchingTag == "" {
		t.Fatal("old child health prematurely opened gate")
	}
	changeSafetyApp(t, r, "Healthy", tu.Status.WatchingCommit)
	reconcileSafety(t, r, tu)
	if tu.Status.WatchingTag != "" {
		t.Fatal("fresh child health did not open gate")
	}
}

func TestOnDeleteMustUpdatePodsBeforeHealthy(t *testing.T) {
	r, tu, _ := safetyHarness(t, "Healthy")
	app := safetyApp("Healthy", "pin")
	_ = unstructured.SetNestedSlice(app.Object, []interface{}{map[string]interface{}{"group": "apps", "kind": "StatefulSet", "name": "worker", "namespace": "nix-csi"}}, "status", "resources")
	if _, err := r.Dynamic.Resource(appResource).Namespace("argocd").Update(context.Background(), app, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	sts := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "metadata": map[string]interface{}{"name": "worker", "namespace": "nix-csi"}, "spec": map[string]interface{}{"replicas": int64(2), "updateStrategy": map[string]interface{}{"type": "OnDelete"}}, "status": map[string]interface{}{"readyReplicas": int64(2), "updatedReplicas": int64(0)}}}
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	if _, err := r.Dynamic.Resource(gvr).Namespace("nix-csi").Create(context.Background(), sts, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	state, err := r.argoCDAppState(context.Background(), tu.Spec.ArgoCDApp)
	if err != nil || state.Health == "Healthy" {
		t.Fatalf("old OnDelete pods certified release: %+v %v", state, err)
	}
	_ = unstructured.SetNestedField(sts.Object, int64(2), "status", "updatedReplicas")
	if _, err := r.Dynamic.Resource(gvr).Namespace("nix-csi").Update(context.Background(), sts, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	state, err = r.argoCDAppState(context.Background(), tu.Spec.ArgoCDApp)
	if err != nil || state.Health != "Healthy" {
		t.Fatalf("updated OnDelete pods were rejected: %+v %v", state, err)
	}
}

func TestLeaseIncludesSecondaryApplication(t *testing.T) {
	r, tu, _ := safetyHarness(t, "Healthy")
	reconcileSafety(t, r, tu)
	other := tu.DeepCopy()
	other.Name = "other-source"
	other.UID = "other"
	other.Spec.ArgoCDApp = &v1.ArgoCDAppRef{Name: "first"}
	other.Spec.Targets = nil
	other.Status = v1.TagUpdaterStatus{}
	acquired, err := r.acquireApp(context.Background(), other)
	if err != nil || acquired {
		t.Fatalf("secondary app admitted another writer: %v %v", acquired, err)
	}
}
