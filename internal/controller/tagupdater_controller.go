package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	v1alpha1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	"github.com/jakeschurch/argocd-tag-updater/internal/matcher"
	intsource "github.com/jakeschurch/argocd-tag-updater/internal/source"
	"github.com/jakeschurch/argocd-tag-updater/internal/writeback"
)

const defaultInterval = 2 * time.Minute
const revStaleAfter = 5 * time.Minute

type TagUpdaterReconciler struct {
	client.Client
	Recorder record.EventRecorder
	Dynamic  dynamic.Interface

	revAttempted   atomic.Bool
	lastRevSuccess atomic.Int64
	progress       progressTracker

	StaleMultiplier int
	StaleFloor      time.Duration
}

const defaultRollbackTimeout = 20 * time.Minute

func (r *TagUpdaterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	var tu v1alpha1.TagUpdater
	if err := r.Get(ctx, req.NamespacedName, &tu); err != nil {
		if apierrors.IsNotFound(err) {
			r.progress.forget(req.String())
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	interval := defaultInterval
	if tu.Spec.Interval.Duration > 0 {
		interval = tu.Spec.Interval.Duration
	}
	progressKey := req.String()
	r.progress.attempt(progressKey, interval, time.Now())
	if tu.Spec.Rollback != nil && tu.Spec.Rollback.Enabled && tu.Spec.ArgoCDApp == nil {
		message := "spec.rollback.enabled requires spec.argoCDApp for read-only sync revision and health observation"
		meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "RollbackReady", Status: metav1.ConditionFalse, Reason: "MissingArgoCDApp", Message: message})
		meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "MissingArgoCDApp", Message: message})
		_ = r.Status().Update(ctx, &tu)
		if r.Recorder != nil {
			r.Recorder.Event(&tu, corev1.EventTypeWarning, "MissingArgoCDApp", message)
		}
		return ctrl.Result{RequeueAfter: interval}, nil
	}
	if tu.Spec.Rollback != nil && tu.Spec.Rollback.Enabled && tu.Status.WatchingTag != "" {
		requeue, err := r.checkHealthAndMaybeRollback(ctx, &tu)
		if err != nil {
			return ctrl.Result{}, err
		}
		if requeue > 0 {
			// Only count waiting-for-health as progress. Waiting for ArgoCD to
			// even observe the commit (WatchingSince still nil) is a blocked
			// state, not a healthy one — marking it successful would hide the
			// freeze from the staleness detector, which is exactly the silent
			// reconcile freeze this controller already tracks elsewhere.
			if tu.Status.WatchingSince != nil {
				r.markReconcileSucceeded(ctx, &tu, progressKey)
			}
			return ctrl.Result{RequeueAfter: requeue}, nil
		}
	}

	if err := validateWriteBack(tu.Spec.WriteBack); err != nil {
		message := err.Error()
		meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "WriteBackReady", Status: metav1.ConditionFalse, Reason: "MissingWriteBackConfig", Message: message})
		meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "MissingWriteBackConfig", Message: message})
		meta.SetStatusCondition(&tu.Status.Conditions, r.stalledCondition(progressKey))
		_ = r.Status().Update(ctx, &tu)
		if r.Recorder != nil {
			r.Recorder.Event(&tu, corev1.EventTypeWarning, "MissingWriteBackConfig", message)
		}
		logger.Error(err, "required git write-back configuration is missing")
		return ctrl.Result{RequeueAfter: interval}, nil
	}

	var ociBasicAuth string
	if tu.Spec.Source.Type == v1alpha1.SourceTypeOCI && tu.Spec.Source.ImagePullSecretRef != nil {
		auth, err := r.resolveDockerAuth(ctx, tu.Namespace, tu.Spec.Source.ImagePullSecretRef.Name, tu.Spec.Source.Repo)
		if err != nil {
			logger.Info("could not resolve imagePullSecret for OCI source; proceeding unauthenticated", "err", err)
		} else {
			ociBasicAuth = auth
		}
	}
	src, err := sourceFor(tu.Spec.Source, ociBasicAuth)
	if err != nil {
		return ctrl.Result{}, r.setFailed(ctx, &tu, err)
	}
	tags, err := src.Tags(ctx)
	if err != nil {
		return ctrl.Result{}, r.setFailed(ctx, &tu, err)
	}
	m, err := matcher.New(tu.Spec.Source.TagPattern)
	if err != nil {
		return ctrl.Result{}, r.setFailed(ctx, &tu, err)
	}
	latest, ok := m.Latest(filterSkipped(tags, tu.Status.SkippedTags))
	if !ok {
		logger.Info("no tags matched pattern", "pattern", tu.Spec.Source.TagPattern)
		r.markReconcileSucceeded(ctx, &tu, progressKey)
		return ctrl.Result{RequeueAfter: interval}, nil
	}

	data := latest.Captures
	data["tag"] = latest.Tag
	for key, value := range parseRepo(tu.Spec.Source.Repo) {
		data[key] = value
	}
	if resolver, ok := src.(intsource.TagResolver); ok {
		if extra, resolveErr := resolver.Resolve(ctx); resolveErr != nil {
			logger.Info("tag resolver failed; templating with tag captures only", "err", resolveErr)
		} else {
			for key, value := range extra {
				data[key] = value
			}
		}
	}
	if err := r.addRev(ctx, src, latest.Tag, data); err != nil {
		return ctrl.Result{}, r.setFailed(ctx, &tu, err)
	}

	var writeResult writeback.Result
	credentials, err := r.resolveWriteBackCredentials(ctx, &tu)
	if err == nil {
		writer := writeback.Writer{Spec: tu.Spec.WriteBack, Credentials: credentials, Targets: tu.Spec.Targets, Data: data}
		if latest.Tag == tu.Status.LastTag && tu.Status.LastWriteCommit != "" {
			remoteHead, headErr := writer.RemoteHead(ctx)
			if headErr == nil && writeBackCurrent(latest.Tag, tu.Status, remoteHead) {
				meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "WriteBackReady", Status: metav1.ConditionTrue, Reason: "RemoteUnchanged", Message: "tag and manifest branch head are unchanged; clone skipped"})
				meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "WrittenToGit", Message: fmt.Sprintf("manifest values for tag %s are present in git", latest.Tag)})
				if err := r.Status().Update(ctx, &tu); err != nil {
					return ctrl.Result{}, err
				}
				r.markReconcileSucceeded(ctx, &tu, progressKey)
				return ctrl.Result{RequeueAfter: interval}, nil
			}
		}
		writeResult, err = writer.Apply(ctx)
		if err == nil {
			reason, message := "UpToDate", "manifest already contains the rendered values; no commit created"
			if writeResult.Committed {
				reason = "Committed"
				message = fmt.Sprintf("committed tag %s to %s on branch %s", latest.Tag, tu.Spec.WriteBack.Path, tu.Spec.WriteBack.Branch)
				if r.Recorder != nil {
					r.Recorder.Event(&tu, corev1.EventTypeNormal, "WriteBackCommitted", message)
				}
			}
			meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "WriteBackReady", Status: metav1.ConditionTrue, Reason: reason, Message: message})
		}
	}
	if err != nil {
		message := err.Error()
		meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "WriteBackReady", Status: metav1.ConditionFalse, Reason: "WriteBackFailed", Message: message})
		if r.Recorder != nil {
			r.Recorder.Event(&tu, corev1.EventTypeWarning, "WriteBackFailed", message)
		}
		return ctrl.Result{}, r.setFailed(ctx, &tu, err)
	}

	if latest.Tag != tu.Status.LastTag {
		now := metav1.Now()
		tu.Status.PreviousTag = tu.Status.LastTag
		tu.Status.LastTag = latest.Tag
		tu.Status.LastUpdated = &now
		if tu.Spec.Rollback != nil && tu.Spec.Rollback.Enabled && tu.Spec.ArgoCDApp != nil {
			tu.Status.WatchingTag = latest.Tag
			tu.Status.WatchingCommit = writeResult.CommitSHA
			tu.Status.WatchingSince = nil
			// Bounds the wait for ArgoCD to observe WatchingCommit; without it
			// that phase never times out. See WatchingArmedAt.
			tu.Status.WatchingArmedAt = &now
		}
	}
	tu.Status.LastWriteCommit = writeResult.CommitSHA
	meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "WrittenToGit", Message: fmt.Sprintf("manifest values for tag %s are present in git", latest.Tag)})
	if err := r.Status().Update(ctx, &tu); err != nil {
		return ctrl.Result{}, err
	}
	r.markReconcileSucceeded(ctx, &tu, progressKey)
	return ctrl.Result{RequeueAfter: interval}, nil
}

func writeBackCurrent(tag string, status v1alpha1.TagUpdaterStatus, remoteHead string) bool {
	return tag == status.LastTag && status.LastWriteCommit != "" && remoteHead == status.LastWriteCommit
}

func (r *TagUpdaterReconciler) checkHealthAndMaybeRollback(ctx context.Context, tu *v1alpha1.TagUpdater) (time.Duration, error) {
	revisionRef := revisionObserver(tu)
	revisionState, err := r.argoCDAppState(ctx, revisionRef)
	if err != nil {
		message := err.Error()
		meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "RollbackReady", Status: metav1.ConditionFalse, Reason: "HealthReadFailed", Message: message})
		_ = r.Status().Update(ctx, tu)
		if r.Recorder != nil {
			r.Recorder.Event(tu, corev1.EventTypeWarning, "HealthReadFailed", message)
		}
		return 15 * time.Second, nil
	}
	if tu.Status.WatchingSince == nil {
		if !revisionState.observes(tu.Status.WatchingCommit) {
			// This phase MUST be bounded. Reconcile short-circuits every tag
			// update while WatchingTag is set, so an ArgoCD that never observes
			// the commit (app renamed or deleted, repo credentials broken,
			// branch mismatch, controller down) would otherwise freeze the
			// updater permanently — and, because the caller marks progress
			// successful on this path, freeze it silently.
			if r.observationDeadlineExceeded(tu) {
				return 0, r.abandonWatch(ctx, tu)
			}
			return 15 * time.Second, nil
		}
		now := metav1.Now()
		tu.Status.WatchingSince = &now
		if err := r.Status().Update(ctx, tu); err != nil {
			return 0, err
		}
	}
	state, err := r.argoCDAppState(ctx, tu.Spec.ArgoCDApp)
	if err != nil {
		message := err.Error()
		meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "RollbackReady", Status: metav1.ConditionFalse, Reason: "HealthReadFailed", Message: message})
		_ = r.Status().Update(ctx, tu)
		return 15 * time.Second, nil
	}

	if state.Health == "Healthy" {
		tu.Status.WatchingTag = ""
		tu.Status.WatchingCommit = ""
		tu.Status.WatchingArmedAt = nil
		tu.Status.WatchingSince = nil
		tu.Status.SkippedTags = nil
		meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "RollbackReady", Status: metav1.ConditionTrue, Reason: "DeploymentHealthy", Message: "ArgoCD reports the written commit Healthy"})
		return 0, r.Status().Update(ctx, tu)
	}
	if state.Health == "Degraded" && !state.syncInFlight() {
		return 0, r.rollbackWithStatus(ctx, tu)
	}
	if time.Since(tu.Status.WatchingSince.Time) >= rollbackTimeout(tu) {
		return 0, r.rollbackWithStatus(ctx, tu)
	}
	return 15 * time.Second, nil
}

// rollbackTimeout is the configured health timeout, and doubles as the deadline
// for ArgoCD to observe the written commit in the first place.
func rollbackTimeout(tu *v1alpha1.TagUpdater) time.Duration {
	if tu.Spec.Rollback != nil && tu.Spec.Rollback.Timeout.Duration > 0 {
		return tu.Spec.Rollback.Timeout.Duration
	}
	return defaultRollbackTimeout
}

// observationDeadlineExceeded reports whether ArgoCD has had long enough to pick
// up WatchingCommit. A watch armed before WatchingArmedAt existed has no arm
// time recorded; treat that as not-yet-exceeded so an upgrade never rolls back
// or abandons a watch purely because the field was absent.
func (r *TagUpdaterReconciler) observationDeadlineExceeded(tu *v1alpha1.TagUpdater) bool {
	if tu.Status.WatchingArmedAt == nil {
		return false
	}
	return time.Since(tu.Status.WatchingArmedAt.Time) >= rollbackTimeout(tu)
}

// abandonWatch releases a watch ArgoCD never picked up, so tag updates resume.
//
// It deliberately does NOT roll back. Never being observed means the commit was
// never deployed, so there is no failed rollout to revert — the fault is in the
// delivery path (missing app, bad credentials, wrong branch), and rewriting git
// would churn the manifest without addressing it. The tag therefore stays in
// place and is NOT added to SkippedTags; it gets another chance once ArgoCD is
// working again. The failure is surfaced loudly instead.
func (r *TagUpdaterReconciler) abandonWatch(ctx context.Context, tu *v1alpha1.TagUpdater) error {
	observer := revisionObserver(tu)
	message := fmt.Sprintf(
		"ArgoCD application %s did not report commit %s within %s; abandoning health watch for tag %s so tag updates resume (check the application exists, its repo credentials, and that it tracks branch %s)",
		observer.Name, tu.Status.WatchingCommit, rollbackTimeout(tu), tu.Status.WatchingTag, tu.Spec.WriteBack.Branch)
	tu.Status.WatchingTag = ""
	tu.Status.WatchingCommit = ""
	tu.Status.WatchingArmedAt = nil
	tu.Status.WatchingSince = nil
	meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "RollbackReady", Status: metav1.ConditionFalse, Reason: "CommitNeverObserved", Message: message})
	meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "CommitNeverObserved", Message: message})
	if r.Recorder != nil {
		r.Recorder.Event(tu, corev1.EventTypeWarning, "CommitNeverObserved", message)
	}
	return r.Status().Update(ctx, tu)
}

func revisionObserver(tu *v1alpha1.TagUpdater) *v1alpha1.ArgoCDAppRef {
	if tu.Spec.ManagingApp != nil {
		return tu.Spec.ManagingApp
	}
	return tu.Spec.ArgoCDApp
}

func (r *TagUpdaterReconciler) rollbackWithStatus(ctx context.Context, tu *v1alpha1.TagUpdater) error {
	if err := r.doRollback(ctx, tu); err != nil {
		message := err.Error()
		meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "RollbackReady", Status: metav1.ConditionFalse, Reason: "RollbackWriteFailed", Message: message})
		_ = r.Status().Update(ctx, tu)
		if r.Recorder != nil {
			r.Recorder.Event(tu, corev1.EventTypeWarning, "RollbackWriteFailed", message)
		}
		return err
	}
	return nil
}

type argoCDState struct {
	Revision  string
	Revisions []string
	Health    string
	Operation string
}

func (s argoCDState) observes(commit string) bool {
	if commit != "" && s.Revision == commit {
		return true
	}
	for _, revision := range s.Revisions {
		if revision == commit && commit != "" {
			return true
		}
	}
	return false
}

func (s argoCDState) syncInFlight() bool {
	return s.Operation == "Running" || s.Operation == "Terminating"
}

func (r *TagUpdaterReconciler) argoCDAppState(ctx context.Context, ref *v1alpha1.ArgoCDAppRef) (argoCDState, error) {
	if ref == nil || r.Dynamic == nil {
		return argoCDState{}, fmt.Errorf("rollback requires spec.argoCDApp and a dynamic client")
	}
	namespace := ref.Namespace
	if namespace == "" {
		namespace = "argocd"
	}
	gvr := schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
	obj, err := r.Dynamic.Resource(gvr).Namespace(namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return argoCDState{}, fmt.Errorf("get application %s/%s: %w", namespace, ref.Name, err)
	}
	state := argoCDState{}
	state.Revision, _, _ = unstructured.NestedString(obj.Object, "status", "sync", "revision")
	state.Revisions, _, _ = unstructured.NestedStringSlice(obj.Object, "status", "sync", "revisions")
	state.Health, _, _ = unstructured.NestedString(obj.Object, "status", "health", "status")
	state.Operation, _, _ = unstructured.NestedString(obj.Object, "status", "operationState", "phase")
	return state, nil
}

func (r *TagUpdaterReconciler) doRollback(ctx context.Context, tu *v1alpha1.TagUpdater) error {
	badTag, previousTag := tu.Status.WatchingTag, tu.Status.PreviousTag
	if previousTag == "" || previousTag == badTag {
		tu.Status.WatchingTag = ""
		tu.Status.WatchingCommit = ""
		tu.Status.WatchingArmedAt = nil
		tu.Status.WatchingSince = nil
		tu.Status.SkippedTags = appendUnique(tu.Status.SkippedTags, badTag)
		message := fmt.Sprintf("tag %s failed but no previous tag is available to write back", badTag)
		meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "RollbackUnavailable", Message: message})
		meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "RollbackReady", Status: metav1.ConditionFalse, Reason: "NoPreviousTag", Message: message})
		return r.Status().Update(ctx, tu)
	}
	m, err := matcher.New(tu.Spec.Source.TagPattern)
	if err != nil {
		return err
	}
	previous, ok := m.Latest([]string{previousTag})
	if !ok {
		return fmt.Errorf("previous tag %s no longer matches spec.source.tagPattern", previousTag)
	}
	data := previous.Captures
	data["tag"] = previous.Tag
	for key, value := range parseRepo(tu.Spec.Source.Repo) {
		data[key] = value
	}
	src, err := sourceFor(tu.Spec.Source, "")
	if err != nil {
		return err
	}
	if resolver, ok := src.(intsource.TagResolver); ok {
		if extra, resolveErr := resolver.Resolve(ctx); resolveErr == nil {
			for key, value := range extra {
				data[key] = value
			}
		}
	}
	if err := r.addRev(ctx, src, previousTag, data); err != nil {
		return err
	}
	credentials, err := r.resolveWriteBackCredentials(ctx, tu)
	if err != nil {
		return err
	}
	result, err := (writeback.Writer{Spec: tu.Spec.WriteBack, Credentials: credentials, Targets: tu.Spec.Targets, Data: data}).Apply(ctx)
	if err != nil {
		return err
	}
	now := metav1.Now()
	tu.Status.LastTag = previousTag
	tu.Status.LastUpdated = &now
	tu.Status.LastWriteCommit = result.CommitSHA
	tu.Status.WatchingTag = ""
	tu.Status.WatchingCommit = ""
	tu.Status.WatchingArmedAt = nil
	tu.Status.WatchingSince = nil
	tu.Status.SkippedTags = appendUnique(tu.Status.SkippedTags, badTag)
	message := fmt.Sprintf("tag %s failed; wrote previous tag %s to git", badTag, previousTag)
	if !result.Committed {
		message += " (manifest was already rolled back; no commit created)"
	}
	meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "RolledBack", Message: message})
	meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "RollbackReady", Status: metav1.ConditionTrue, Reason: "RollbackWritten", Message: message})
	if r.Recorder != nil {
		r.Recorder.Event(tu, corev1.EventTypeWarning, "RolledBack", message)
	}
	return r.Status().Update(ctx, tu)
}

func filterSkipped(tags, skipped []string) []string {
	if len(skipped) == 0 {
		return tags
	}
	set := make(map[string]struct{}, len(skipped))
	for _, tag := range skipped {
		set[tag] = struct{}{}
	}
	filtered := tags[:0:0]
	for _, tag := range tags {
		if _, found := set[tag]; !found {
			filtered = append(filtered, tag)
		}
	}
	return filtered
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func validateWriteBack(spec v1alpha1.WriteBackSpec) error {
	var missing []string
	if strings.TrimSpace(spec.Repo) == "" {
		missing = append(missing, "repo")
	}
	if strings.TrimSpace(spec.Branch) == "" {
		missing = append(missing, "branch")
	}
	if strings.TrimSpace(spec.Path) == "" {
		missing = append(missing, "path")
	}
	if len(missing) > 0 {
		return fmt.Errorf("spec.writeBack is required with non-empty repo, branch, and path; add missing field(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

func (r *TagUpdaterReconciler) resolveWriteBackCredentials(ctx context.Context, tu *v1alpha1.TagUpdater) (writeback.Credentials, error) {
	ref := tu.Spec.WriteBack.CredentialsSecretRef
	if ref.Name != "" {
		var secret corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: tu.Namespace, Name: ref.Name}, &secret); err != nil {
			return writeback.Credentials{}, fmt.Errorf("get write-back credentials secret %s/%s: %w", tu.Namespace, ref.Name, err)
		}
		credentials := writeback.Credentials{
			Token: string(secret.Data["token"]), Username: string(secret.Data["username"]),
			Password: string(secret.Data["password"]), SSHPrivateKey: secret.Data["sshPrivateKey"],
			KnownHosts:            secret.Data["knownHosts"],
			InsecureIgnoreHostKey: strings.EqualFold(strings.TrimSpace(string(secret.Data["insecureIgnoreHostKey"])), "true"),
		}
		if credentials.Token == "" && len(credentials.SSHPrivateKey) == 0 && (credentials.Username == "" || credentials.Password == "") {
			return writeback.Credentials{}, fmt.Errorf("write-back credentials secret %s/%s must contain token, sshPrivateKey, or both username and password", tu.Namespace, ref.Name)
		}
		return credentials, nil
	}

	keyFile := strings.TrimSpace(os.Getenv("GIT_SSH_KEY_FILE"))
	knownHostsFile := strings.TrimSpace(os.Getenv("GIT_KNOWN_HOSTS_FILE"))
	if keyFile == "" || knownHostsFile == "" {
		return writeback.Credentials{}, fmt.Errorf("write-back requires credentialsSecretRef or both GIT_SSH_KEY_FILE and GIT_KNOWN_HOSTS_FILE")
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		return writeback.Credentials{}, fmt.Errorf("read mounted write-back SSH key %s: %w", keyFile, err)
	}
	knownHosts, err := os.ReadFile(knownHostsFile)
	if err != nil {
		return writeback.Credentials{}, fmt.Errorf("read mounted write-back known_hosts %s: %w", knownHostsFile, err)
	}
	return writeback.Credentials{SSHPrivateKey: key, KnownHosts: knownHosts}, nil
}

func (r *TagUpdaterReconciler) markReconcileSucceeded(ctx context.Context, tu *v1alpha1.TagUpdater, key string) {
	r.progress.success(key, time.Now())
	if meta.SetStatusCondition(&tu.Status.Conditions, r.stalledCondition(key)) {
		if err := r.Status().Update(ctx, tu); err != nil {
			log.FromContext(ctx).Error(err, "failed to update Stalled condition")
		}
	}
}

func (r *TagUpdaterReconciler) stalledCondition(key string) metav1.Condition {
	if r.progress.isStale(key, time.Now()) {
		return metav1.Condition{Type: "Stalled", Status: metav1.ConditionTrue, Reason: "ReconcileStale", Message: "no successful reconcile within the staleness window; deploys for this updater may be frozen"}
	}
	return metav1.Condition{Type: "Stalled", Status: metav1.ConditionFalse, Reason: "Progressing", Message: "reconciles are completing within the staleness window"}
}

func (r *TagUpdaterReconciler) addRev(ctx context.Context, src intsource.Source, tag string, data map[string]string) error {
	resolver, ok := src.(intsource.TagRevResolver)
	if !ok {
		return nil
	}
	r.revAttempted.Store(true)
	revs, err := resolver.TagRevs(ctx)
	if err != nil {
		return fmt.Errorf("resolve tag %q to commit sha: %w", tag, err)
	}
	sha := revs[tag]
	if sha == "" {
		return fmt.Errorf("tag %q resolved to no commit sha; refusing impure tag-only flakeRef", tag)
	}
	data["rev"] = sha
	r.lastRevSuccess.Store(time.Now().UnixNano())
	return nil
}

func (r *TagUpdaterReconciler) RevResolutionHealthz() healthz.Checker {
	return func(*http.Request) error {
		if !r.revAttempted.Load() {
			return nil
		}
		last := r.lastRevSuccess.Load()
		if last == 0 {
			return fmt.Errorf("git tag->rev resolution has never succeeded")
		}
		if age := time.Since(time.Unix(0, last)); age > revStaleAfter {
			return fmt.Errorf("git tag->rev resolution stale: last success %s ago (>%s)", age.Round(time.Second), revStaleAfter)
		}
		return nil
	}
}

func (r *TagUpdaterReconciler) setFailed(ctx context.Context, tu *v1alpha1.TagUpdater, cause error) error {
	meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Error", Message: cause.Error()})
	meta.SetStatusCondition(&tu.Status.Conditions, r.stalledCondition(client.ObjectKeyFromObject(tu).String()))
	_ = r.Status().Update(ctx, tu)
	return cause
}

func parseRepo(raw string) map[string]string {
	out := map[string]string{"repoURL": raw}
	raw = strings.TrimSuffix(raw, ".git")
	if strings.HasPrefix(raw, "git@") {
		raw = strings.TrimPrefix(raw, "git@")
		if host, path, ok := strings.Cut(raw, ":"); ok {
			out["host"] = host
			setOwnerRepo(out, path)
		}
		return out
	}
	if i := strings.Index(raw, ":"); i > 0 && !strings.Contains(raw[:i], "/") {
		out["host"] = raw[:i] + ".com"
		setOwnerRepo(out, raw[i+1:])
		return out
	}
	if strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "http://") {
		raw = strings.SplitN(raw, "://", 2)[1]
		if slash := strings.Index(raw, "/"); slash > 0 {
			out["host"] = raw[:slash]
			setOwnerRepo(out, raw[slash+1:])
		}
	}
	return out
}

func setOwnerRepo(out map[string]string, path string) {
	if owner, repo, ok := strings.Cut(path, "/"); ok {
		out["owner"], out["repo"] = owner, repo
	}
}

func sourceFor(spec v1alpha1.SourceSpec, ociBasicAuth string) (intsource.Source, error) {
	switch spec.Type {
	case v1alpha1.SourceTypeGit:
		return &intsource.Git{
			Repo: spec.Repo, SSHKeyFile: os.Getenv("GIT_SSH_KEY_FILE"), Token: os.Getenv("GIT_TOKEN"),
			KnownHostsFile:        os.Getenv("GIT_KNOWN_HOSTS_FILE"),
			InsecureIgnoreHostKey: strings.EqualFold(os.Getenv("GIT_INSECURE_IGNORE_HOST_KEY"), "true"),
		}, nil
	case v1alpha1.SourceTypeOCI:
		return &intsource.OCI{Repo: spec.Repo, BasicAuth: ociBasicAuth}, nil
	case v1alpha1.SourceTypeNix:
		return &intsource.Nix{Repo: spec.Repo, Token: os.Getenv("NIX_CACHE_TOKEN")}, nil
	default:
		return nil, fmt.Errorf("unknown source type %q", spec.Type)
	}
}

func (r *TagUpdaterReconciler) resolveDockerAuth(ctx context.Context, namespace, secretName, repo string) (string, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: secretName}, &secret); err != nil {
		return "", fmt.Errorf("get secret %s/%s: %w", namespace, secretName, err)
	}
	raw, ok := secret.Data[".dockerconfigjson"]
	if !ok {
		return "", fmt.Errorf("secret %s/%s missing .dockerconfigjson key", namespace, secretName)
	}
	var cfg struct {
		Auths map[string]struct {
			Auth     string `json:"auth"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", fmt.Errorf("parse dockerconfigjson in %s/%s: %w", namespace, secretName, err)
	}
	ref := strings.TrimPrefix(strings.TrimPrefix(repo, "https://"), "http://")
	host, _, _ := strings.Cut(ref, "/")
	for registryHost, entry := range cfg.Auths {
		if registryHost != host {
			continue
		}
		if entry.Auth != "" {
			return entry.Auth, nil
		}
		if entry.Username != "" && entry.Password != "" {
			return base64.StdEncoding.EncodeToString([]byte(entry.Username + ":" + entry.Password)), nil
		}
	}
	return "", fmt.Errorf("no auth entry for host %q in secret %s/%s", host, namespace, secretName)
}

func (r *TagUpdaterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.progress.multiplier = r.StaleMultiplier
	r.progress.floor = r.StaleFloor
	metrics.Registry.MustRegister(progressCollector{tracker: &r.progress})
	return ctrl.NewControllerManagedBy(mgr).For(&v1alpha1.TagUpdater{}).Complete(r)
}
