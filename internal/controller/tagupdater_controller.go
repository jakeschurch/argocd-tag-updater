package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
	"github.com/jakeschurch/argocd-tag-updater/internal/gate"
	"github.com/jakeschurch/argocd-tag-updater/internal/matcher"
	intsource "github.com/jakeschurch/argocd-tag-updater/internal/source"
	"github.com/jakeschurch/argocd-tag-updater/internal/writeback"
)

const defaultInterval = 2 * time.Minute
const revStaleAfter = 5 * time.Minute

type TagUpdaterReconciler struct {
	client.Client
	APIReader client.Reader
	Recorder  record.EventRecorder
	Dynamic   dynamic.Interface
	Gates     *gate.Registry

	revFailingSince atomic.Int64
	revPending      sync.Map // per-call start times; completed calls never age while idle
	progress        progressTracker
	fastRetry       fastRetryTracker
	signed          signedDigests

	StaleMultiplier int
	StaleFloor      time.Duration

	// now is the clock for fast-retry windows; nil means time.Now.
	now func() time.Time
}

const defaultRollbackTimeout = 20 * time.Minute

func (r *TagUpdaterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	var tu v1alpha1.TagUpdater
	if err := r.Get(ctx, req.NamespacedName, &tu); err != nil {
		if apierrors.IsNotFound(err) {
			pendingTooLong.DeleteLabelValues(req.Namespace, req.Name)
			if r.Gates != nil {
				r.Gates.Unsubscribe(req.NamespacedName)
			}
			r.progress.forget(req.String())
			r.fastRetry.reset(req.String())
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !tu.DeletionTimestamp.IsZero() {
		pendingTooLong.DeleteLabelValues(tu.Namespace, tu.Name)
		if r.Gates != nil {
			r.Gates.Unsubscribe(req.NamespacedName)
		}
		if err := r.releaseApp(ctx, &tu); err != nil {
			return ctrl.Result{}, err
		}
		removeGateFinalizer(&tu)
		return ctrl.Result{}, r.Update(ctx, &tu)
	}
	if tu.Status.PendingWriteStatus != "" {
		writer, err := r.preparedWriter(ctx, &tu)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.settleWrite(ctx, &tu, writer, nil); err != nil {
			return ctrl.Result{}, err
		}
	}
	// 2026-11-11: drain old ownership before observing a changed spec. Search
	// leases by holder, since removed targets are absent from the current spec.
	if tu.Status.WatchingTag != "" && (!gated(&tu) || (tu.Status.WatchingSpecHash != "" && tu.Status.WatchingSpecHash != specHash(&tu)) || len(tu.Status.RollbackFields) == 0) {
		if err := r.releaseApp(ctx, &tu); err != nil {
			return ctrl.Result{}, err
		}
		clearWatch(&tu)
		meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "RollbackReady", Status: metav1.ConditionFalse, Reason: "WatchDrained", Message: "spec changed or legacy watch lacks a verifiable baseline; drained old leases without changing Git"})
		return ctrl.Result{Requeue: true}, r.Status().Update(ctx, &tu)
	}
	if r.Gates != nil {
		if err := r.Gates.Subscribe(req.NamespacedName, tu.Generation, r.gateSubscriptions(&tu)); err != nil {
			return ctrl.Result{RequeueAfter: defaultInterval}, r.setFailedBeforeWriteBack(ctx, &tu, fmt.Errorf("invalid gate: %w", err))
		}
		if gated(&tu) && addGateFinalizer(&tu) {
			return ctrl.Result{Requeue: true}, r.Update(ctx, &tu)
		}
		if !r.Gates.Synced(req.NamespacedName) {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
	}

	if r.Gates == nil && gated(&tu) && addGateFinalizer(&tu) {
		if err := r.Update(ctx, &tu); err != nil {
			return ctrl.Result{}, err
		}
	}

	interval := defaultInterval
	if tu.Spec.Interval.Duration > 0 {
		interval = tu.Spec.Interval.Duration
	}
	progressKey := req.String()
	r.progress.attempt(progressKey, interval, time.Now())
	if tu.Spec.Source.ResolveDigest && tu.Spec.Source.Type != v1alpha1.SourceTypeGit {
		return ctrl.Result{RequeueAfter: interval}, r.setFailedBeforeWriteBack(ctx, &tu, fmt.Errorf("resolveDigest is supported only for Git sources"))
	}
	if err := validateRequireSigned(tu.Spec.Source); err != nil {
		return ctrl.Result{RequeueAfter: interval}, r.setFailedBeforeWriteBack(ctx, &tu, err)
	}
	if tu.Spec.Rollback != nil && tu.Spec.Rollback.Enabled && !gated(&tu) {
		message := "spec.rollback.enabled requires spec.argoCDApp for read-only sync revision and health observation"
		meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "RollbackReady", Status: metav1.ConditionFalse, Reason: "MissingArgoCDApp", Message: message})
		meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "MissingArgoCDApp", Message: message})
		_ = r.Status().Update(ctx, &tu)
		if r.Recorder != nil {
			r.Recorder.Event(&tu, corev1.EventTypeWarning, "MissingArgoCDApp", message)
		}
		return ctrl.Result{RequeueAfter: interval}, nil
	}
	ambiguous, err := r.appAmbiguous(ctx, &tu)
	if err != nil {
		return ctrl.Result{}, err
	}
	if ambiguous {
		return ctrl.Result{RequeueAfter: interval}, nil
	}
	if tu.Status.WatchingTag != "" {
		acquired, err := r.acquireApp(ctx, &tu)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !acquired {
			return ctrl.Result{RequeueAfter: interval}, nil
		}
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
		return ctrl.Result{RequeueAfter: interval}, nil
	}
	if strings.EqualFold(tu.Annotations[holdAnnotation], "true") {
		condition := metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Held", Message: "selection paused by " + holdAnnotation}
		if meta.SetStatusCondition(&tu.Status.Conditions, condition) {
			return ctrl.Result{RequeueAfter: interval}, r.Status().Update(ctx, &tu)
		}
		return ctrl.Result{RequeueAfter: interval}, nil
	}

	if err := validateWriteBack(tu.Spec.WriteBack, tu.Spec.Targets); err != nil {
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
	var updaters v1alpha1.TagUpdaterList
	if err := r.List(ctx, &updaters); err != nil {
		return ctrl.Result{}, fmt.Errorf("list TagUpdaters to validate field ownership: %w", err)
	}
	if err := validateFieldOwnership(updaters.Items); err != nil {
		message := err.Error()
		meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "WriteBackReady", Status: metav1.ConditionFalse, Reason: "ConflictingFieldOwner", Message: message})
		meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "ConflictingFieldOwner", Message: message})
		_ = r.Status().Update(ctx, &tu)
		if r.Recorder != nil {
			r.Recorder.Event(&tu, corev1.EventTypeWarning, "ConflictingFieldOwner", message)
		}
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
		return ctrl.Result{}, r.setFailedBeforeWriteBack(ctx, &tu, err)
	}
	tags, err := src.Tags(ctx)
	if err != nil {
		return r.transientFailure(ctx, &tu, progressKey, "", interval, err)
	}
	m, err := matcher.New(tu.Spec.Source.TagPattern)
	if err != nil {
		return ctrl.Result{}, r.setFailedBeforeWriteBack(ctx, &tu, err)
	}
	_ = m
	latest, componentData, ok, err := r.selectRelease(ctx, &tu, tags, src)
	if err != nil {
		return r.transientFailure(ctx, &tu, progressKey, "", interval, err)
	}
	if !ok {
		r.fastRetry.reset(progressKey)
		// Refresh BOTH conditions before returning. This path used to return
		// with only Stalled touched, so Ready/WriteBackReady kept whatever the
		// last reconcile that got past here left behind — a source or
		// write-back error from days or months ago that no longer describes
		// anything. An updater whose tag stream has gone empty (tags pushed to
		// a different remote than spec.source.repo, say) then reports a
		// long-dead failure forever.
		message := fmt.Sprintf("no tag in %s matched pattern %s", tu.Spec.Source.Repo, tu.Spec.Source.TagPattern)
		logger.Info("no tags matched pattern", "pattern", tu.Spec.Source.TagPattern)
		meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "NoMatchingTags", Message: message})
		setWriteBackNotAttempted(&tu, message)
		if err := r.Status().Update(ctx, &tu); err != nil {
			return ctrl.Result{}, err
		}
		r.markReconcileSucceeded(ctx, &tu, progressKey)
		return ctrl.Result{RequeueAfter: interval}, nil
	}

	data := latest.Captures
	data["tag"] = latest.Tag
	for key, value := range parseRepo(tu.Spec.Source.Repo) {
		data[key] = value
	}
	if resolver, ok := src.(intsource.TagResolver); ok {
		extra, resolveErr := resolver.Resolve(ctx, latest.Tag)
		if resolveErr != nil {
			return r.transientFailure(ctx, &tu, progressKey, latest.Tag, interval, fmt.Errorf("resolve release record for tag %s: %w", latest.Tag, resolveErr))
		}
		for key, value := range extra {
			data[key] = value
		}
	}
	if err := r.addRev(ctx, src, latest.Tag, data); err != nil {
		return r.transientFailure(ctx, &tu, progressKey, latest.Tag, interval, err)
	}

	if err := r.addImageDigest(ctx, &tu, latest.Tag, data); err != nil {
		return r.transientFailure(ctx, &tu, progressKey, latest.Tag, interval, err)
	}

	if tu.Spec.Source.RequireSigned != "" {
		signed, digest, signErr := r.verifySigned(ctx, &tu, ociBasicAuth, latest.Tag, data)
		if signErr != nil {
			meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: signatureCondition, Status: metav1.ConditionUnknown, Reason: "SignatureCheckFailed", Message: signErr.Error()})
			if r.Recorder != nil {
				r.Recorder.Event(&tu, corev1.EventTypeWarning, "SignatureCheckFailed", signErr.Error())
			}
			return r.transientFailure(ctx, &tu, progressKey, latest.Tag, interval, signErr)
		}
		if !signed {
			// Holding is a wait, not a failure: the tag is not skipped, so the
			// next poll writes it once the pipeline adds its signed marker.
			if err := r.awaitSignature(ctx, &tu, latest.Tag, digest); err != nil {
				return ctrl.Result{}, err
			}
			r.markReconcileSucceeded(ctx, &tu, progressKey)
			// The marker is expected moments after the tag: poll quickly for a
			// bounded time instead of idling a whole interval.
			return ctrl.Result{RequeueAfter: r.fastRetry.next(progressKey, latest.Tag, r.clock(), interval)}, nil
		}
		markSigned(&tu, latest.Tag, digest)
		data["signed_digest"] = digest
	} else {
		clearSignatureStatus(&tu)
	}

	// Past every transient gate: whatever follows is write-back, which keeps its
	// own error handling, and steady/WrittenToGit must wait the full interval.
	r.fastRetry.reset(progressKey)

	if tu.Spec.Source.ResolveDigest && latest.Tag == tu.Status.LastTag && intsource.ValidDigest(tu.Status.LastImageDigest) && tu.Status.LastImageDigest != data["image_digest"] {
		message := "registry digest changed for the recorded release tag; refusing to replace the known-good image"
		meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "RegistryDigestChanged", Message: message})
		meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "WriteBackReady", Status: metav1.ConditionFalse, Reason: "RegistryDigestChanged", Message: message})
		if err := r.Status().Update(ctx, &tu); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: interval}, nil
	}

	acquired, err := r.acquireApp(ctx, &tu)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !acquired {
		condition := metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "AppQueued", Message: "another release holds this application's health gate"}
		if meta.SetStatusCondition(&tu.Status.Conditions, condition) {
			return ctrl.Result{RequeueAfter: interval}, r.Status().Update(ctx, &tu)
		}
		return ctrl.Result{RequeueAfter: interval}, nil
	}
	// No pending watch means no Git mutation is in flight. Release on all
	// pre-push failures and unchanged paths; durable watches retain the lease.
	defer func() {
		if tu.Status.WatchingTag == "" {
			if err := r.releaseApp(ctx, &tu); err != nil {
				logger.Error(err, "release app lease")
			}
		}
	}()

	previousTag, previousDigest, previousSigned := tu.Status.LastTag, tu.Status.LastImageDigest, tu.Status.LastSignedDigest
	var writeResult writeback.Result
	credentials, err := r.resolveWriteBackCredentials(ctx, &tu)
	if err == nil {
		writer := writeback.Writer{Spec: tu.Spec.WriteBack, Credentials: credentials, Targets: tu.Spec.Targets, Data: data, TargetData: componentData}
		if latest.Tag == tu.Status.LastTag && tu.Status.LastWriteCommit != "" && tu.Status.ObservedGeneration == tu.Generation && (!tu.Spec.Source.ResolveDigest || tu.Status.LastImageDigest == data["image_digest"]) && tu.Status.LastSignedDigest == data["signed_digest"] {
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
		if gated(&tu) {
			writer.CaptureRollback = true
			writer.BeforePush = func(result writeback.Result) error {
				if !result.Committed {
					return nil
				}
				// Validate intent before arming status: a refused push must never
				// persist a watch without its baseline and complete field snapshot.
				ambiguous, err := r.appAmbiguousForIntent(ctx, &tu, true)
				if err != nil {
					return err
				}
				if ambiguous {
					return fmt.Errorf("ambiguous pending changes; refusing push")
				}
				prepareWrite(&tu)
				now := metav1.NewTime(r.clock())
				tu.Status.PreviousTag, tu.Status.PreviousImageDigest, tu.Status.PreviousSignedDigest = previousTag, previousDigest, previousSigned
				tu.Status.LastTag = latest.Tag
				tu.Status.LastImageDigest, tu.Status.LastSignedDigest = data["image_digest"], data["signed_digest"]
				tu.Status.LastWriteCommit, tu.Status.LastUpdated = result.CommitSHA, &now
				tu.Status.WatchingTag, tu.Status.WatchingCommit = latest.Tag, result.CommitSHA
				tu.Status.WatchingArmedAt, tu.Status.WatchingSince = &now, nil
				tu.Status.WatchingGate = tu.Spec.Gate.DeepCopy()
				tu.Status.WatchingSpecHash = specHash(&tu)
				tu.Status.WatchingDeployed = false
				baseline := r.baselineHealthy(ctx, &tu)
				tu.Status.BaselineHealthy = baseline
				tu.Status.RollbackFields = result.RollbackFields
				tu.Status.WatchingRollback = false
				if !baseline {
					r.suppressed(ctx, &tu, "UnhealthyBaseline", "application was not Healthy before write-back; this release cannot be blamed automatically")
				}
				return r.Status().Update(ctx, &tu)
			}
		}
		writeResult, err = writer.Apply(ctx)
		if tu.Status.PendingWriteStatus != "" {
			err = r.settleWrite(ctx, &tu, writer, err)
			if err == nil && writeResult.CommitSHA == "" {
				writeResult = writeback.Result{Committed: true, CommitSHA: tu.Status.WatchingCommit}
			}
		}
		if err == nil {
			reason, message := "UpToDate", "manifest already contains the rendered values; no commit created"
			if writeResult.Committed {
				reason = "Committed"
				message = fmt.Sprintf("committed tag %s to %s on branch %s", latest.Tag, strings.Join(writeBackPaths(tu.Spec), ", "), tu.Spec.WriteBack.Branch)
				if r.Recorder != nil {
					r.Recorder.Event(&tu, corev1.EventTypeNormal, "WriteBackCommitted", message)
				}
				r.refreshAfterWriteBack(ctx, &tu)
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
		tu.Status.PreviousImageDigest = tu.Status.LastImageDigest
		tu.Status.PreviousSignedDigest = tu.Status.LastSignedDigest
		tu.Status.LastTag = latest.Tag
		tu.Status.LastUpdated = &now

	}
	tu.Status.LastImageDigest = data["image_digest"]
	tu.Status.LastSignedDigest = data["signed_digest"]
	tu.Status.ObservedGeneration = tu.Generation
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
	ambiguous, err := r.appAmbiguous(ctx, tu)
	if err != nil {
		return 0, err
	}
	if ambiguous {
		return 0, nil
	}
	if tu.Status.WatchingSince == nil {
		observes := revisionObserver(tu) == nil
		if revisionObserver(tu) != nil {
			revisionState, err := r.argoCDAppState(ctx, revisionObserver(tu))
			if err != nil {
				return r.blockedWatch(ctx, tu, "HealthReadFailed", err.Error())
			}
			observes = revisionState.observes(tu.Status.WatchingCommit, tu.Status.WatchingArmedAt)
		}
		if !observes {
			if r.observationDeadlineExceeded(tu) {
				return r.blockedWatch(ctx, tu, "CommitNeverObserved", "ArgoCD has not observed the pin commit; retaining the app lease and excluding no tags")
			}
			return r.watchDelay(tu), nil
		}
		now := metav1.NewTime(r.clock())
		tu.Status.WatchingSince = &now
		if err := r.Status().Update(ctx, tu); err != nil {
			return 0, err
		}
	}
	state, err := r.releaseHealth(ctx, tu)
	if err != nil {
		return r.blockedWatch(ctx, tu, "HealthReadFailed", err.Error())
	}
	if state.Sync == "Unknown" {
		if r.clock().Sub(tu.Status.WatchingSince.Time) >= rollbackTimeout(tu) {
			pendingTooLong.WithLabelValues(tu.Namespace, tu.Name).Set(1)
		}
		return r.blockedWatch(ctx, tu, "ComparisonUnavailable", "Argo comparison is unavailable; this is not evidence of a release failure")
	}
	// An app-of-apps can acknowledge the pin before the child has compared its
	// new spec. Require a fresh child comparison before accepting its health.
	fresh := tu.Status.WatchingArmedAt == nil || !state.ReconciledAt.Before(tu.Status.WatchingArmedAt.Time)
	if tu.Spec.ManagingApp != nil {
		fresh = fresh && !state.ReconciledAt.Before(tu.Status.WatchingSince.Time)
	}
	if len(healthApps(tu)) == 0 {
		fresh = true
	}
	deployed := fresh && state.Sync == "Synced" && r.applicationPinsApplied(ctx, tu)
	healthy := deployed && state.Health == "Healthy" && !state.syncInFlight()
	failed := deployed && state.Health == "Degraded" && !state.syncInFlight()
	analysisUnavailable := false
	if explicit := activeGate(tu); explicit != nil {
		if r.Gates == nil {
			return r.blockedWatch(ctx, tu, "GateUnavailable", "gate registry is unavailable")
		}
		tag := tu.Status.WatchingTag
		if tu.Status.WatchingRollback {
			tag = tu.Status.PreviousTag
		}
		d, h, f, why := r.Gates.Evaluate(explicit.Sources, tag)
		analysisUnavailable = why == gate.AnalysisInfrastructureError
		if analysisUnavailable {
			failed = failed && r.failureOutsideAnalysis(ctx, tu)
		}
		// The explicit source fences stale aggregate Application failures. Once
		// it acknowledges this release, failures elsewhere in the same unit
		// (for example its operator Deployment) must still roll back the unit.
		deployed, healthy, failed = deployed && d, healthy && h, f || (d && failed)
	}
	if deployed && !tu.Status.WatchingDeployed {
		tu.Status.WatchingDeployed = true
		if err := r.Status().Update(ctx, tu); err != nil {
			return 0, err
		}
	}
	if healthy {
		pendingTooLong.WithLabelValues(tu.Namespace, tu.Name).Set(0)
		clearWatch(tu)
		meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "RollbackReady", Status: metav1.ConditionTrue, Reason: "DeploymentHealthy", Message: "ArgoCD reports the written release Synced and Healthy"})
		if err := r.Status().Update(ctx, tu); err != nil {
			return 0, err
		}
		return 0, r.releaseApp(ctx, tu)
	}
	if analysisUnavailable && !failed {
		if r.clock().Sub(tu.Status.WatchingSince.Time) >= rollbackTimeout(tu) {
			pendingTooLong.WithLabelValues(tu.Namespace, tu.Name).Set(1)
		}
		return r.blockedWatch(ctx, tu, "AnalysisUnavailable", "analysis infrastructure errored; awaiting recovery without blaming the release")
	}
	// 2026-11-11: after deployment only a terminal source failure can blame a
	// release. Long drains and low-traffic analysis must not race a wall clock.
	expired := !tu.Status.WatchingDeployed && r.clock().Sub(tu.Status.WatchingSince.Time) >= rollbackTimeout(tu)
	if failed || expired {
		if tu.Status.WatchingRollback {
			return r.blockedWatch(ctx, tu, "RollbackUnhealthy", "rollback has not reached Healthy; retaining the app lease")
		}
		if !tu.Status.BaselineHealthy {
			return r.blockedWatch(ctx, tu, "UnhealthyBaseline", "app was unhealthy before this write; rollback suppressed and app lease retained until Healthy")
		}
		if tu.Spec.Rollback == nil || !tu.Spec.Rollback.Enabled {
			return r.blockedWatch(ctx, tu, "RollbackDisabled", "release has not reached Healthy; automatic rollback is disabled")
		}
		return r.watchDelay(tu), r.rollbackWithStatus(ctx, tu)
	}
	if tu.Status.WatchingDeployed && r.clock().Sub(tu.Status.WatchingSince.Time) >= rollbackTimeout(tu) {
		pendingTooLong.WithLabelValues(tu.Namespace, tu.Name).Set(1)
		return r.blockedWatch(ctx, tu, "PendingTooLong", "deployed release is still waiting for health; inspect workload rotation or analysis; no failure has been attributed")
	}
	return r.watchDelay(tu), nil
}

func clearWatch(tu *v1alpha1.TagUpdater) {
	pendingTooLong.WithLabelValues(tu.Namespace, tu.Name).Set(0)
	tu.Status.PendingWriteStatus, tu.Status.WatchingSpecHash = "", ""
	tu.Status.WatchingTag, tu.Status.WatchingCommit = "", ""
	tu.Status.WatchingSince, tu.Status.WatchingArmedAt = nil, nil
	tu.Status.WatchingRollback = false
	tu.Status.RollbackFields = nil
	tu.Status.WatchingGate = nil
	tu.Status.WatchingDeployed = false
}

func (r *TagUpdaterReconciler) watchDelay(tu *v1alpha1.TagUpdater) time.Duration {
	anchor := tu.Status.WatchingArmedAt
	if tu.Status.WatchingSince != nil {
		anchor = tu.Status.WatchingSince
	}
	if anchor != nil {
		if left := rollbackTimeout(tu) - r.clock().Sub(anchor.Time); left > 0 {
			return left
		}
	}
	return defaultInterval // slow recovery backstop; informer events drive progress
}

func (r *TagUpdaterReconciler) blockedWatch(ctx context.Context, tu *v1alpha1.TagUpdater, reason, message string) (time.Duration, error) {
	condition := metav1.Condition{Type: "RollbackReady", Status: metav1.ConditionFalse, Reason: reason, Message: message}
	if meta.SetStatusCondition(&tu.Status.Conditions, condition) {
		r.suppressed(ctx, tu, reason, message)
		if err := r.Status().Update(ctx, tu); err != nil {
			return 0, err
		}
	}
	return defaultInterval, nil
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
	return r.clock().Sub(tu.Status.WatchingArmedAt.Time) >= rollbackTimeout(tu)
}

// refreshAfterWriteBack asks ArgoCD to read the commit just pushed, instead of it finding the
// commit when its cached branch heads expire, about three minutes later. The annotation only
// compares: it starts no sync and touches no operation, so the Application's own sync policy still
// decides everything. ArgoCD removes it once read. Failing costs only those minutes, so it is
// logged and nothing else.
func (r *TagUpdaterReconciler) refreshAfterWriteBack(ctx context.Context, tu *v1alpha1.TagUpdater) {
	ref := revisionObserver(tu)
	if ref == nil || r.Dynamic == nil {
		return
	}

	namespace := ref.Namespace
	if namespace == "" {
		namespace = "argocd"
	}

	gvr := schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
	patch := []byte(`{"metadata":{"annotations":{"argocd.argoproj.io/refresh":"normal"}}}`)

	if _, err := r.Dynamic.Resource(gvr).Namespace(namespace).Patch(ctx, ref.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		log.FromContext(ctx).Info("could not ask ArgoCD to refresh; it will find the commit on its own", "application", ref.Name, "err", err)
	}
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

// argoCDDeployment is one rollout ArgoCD actually performed. Revisions is a
// slice because a multi-source Application resolves one revision per source.
// StartedAt is what orders a rollout against a write-back commit: a rollout
// that began after the commit was pushed resolved the branch to a head at or
// after it.
type argoCDDeployment struct {
	Revisions []string
	StartedAt time.Time
}

type argoCDState struct {
	Sync         string
	ReconciledAt time.Time
	Revision     string
	Revisions    []string
	// Deployments are the rollouts ArgoCD still retains, oldest first: its
	// history plus the current operation once it has succeeded.
	Deployments []argoCDDeployment
	Health      string
	Operation   string
}

// observes reports whether ArgoCD has caught up to commit, which was pushed to
// the write-back branch at pushedAt.
//
// Equality against the instantaneous status.sync.revision is not a usable test.
// ArgoCD resolves the branch to whatever head exists when it next refreshes, so
// that one field only ever names the newest revision it has looked at — it does
// not retain what was actually rolled out. Commits written by this controller
// were reported CommitNeverObserved while sitting in the very same
// Application's deployment history, so a watch could never open for a change
// that had already shipped, and abandonWatch then blamed the delivery path.
//
// So: match every revision ArgoCD still names — the compared head, the sync
// revisions, and each retained rollout. The write-back branch is also shared by
// every TagUpdater and by humans, so a commit can be superseded before ArgoCD
// next resolves the branch; it is then coalesced into a later rollout and never
// named anywhere. Because the branch only fast-forwards, any rollout that
// started after the commit was pushed necessarily carries it, so accept that
// too. ArgoCD resolves a target revision at comparison time rather than at
// rollout time, so this can accept a rollout up to one refresh interval stale;
// opening the gate marginally early is the deliberate trade against a gate that
// never opens at all.
//
// A watch armed by an older build records no push time. Fall back to exact
// matching there rather than guessing, as observationDeadlineExceeded does.
func (s argoCDState) observes(commit string, pushedAt *metav1.Time) bool {
	if commit == "" {
		return false
	}
	if s.Revision == commit {
		return true
	}
	for _, revision := range s.Revisions {
		if revision == commit {
			return true
		}
	}
	for _, deployment := range s.Deployments {
		for _, revision := range deployment.Revisions {
			if revision == commit {
				return true
			}
		}
	}
	if pushedAt == nil {
		return false
	}
	for _, deployment := range s.Deployments {
		if deployment.StartedAt.After(pushedAt.Time) {
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
	obj, err := r.gateObject(ctx, gvr.GroupVersion().WithKind("Application"), "applications", types.NamespacedName{Namespace: namespace, Name: ref.Name})
	if err != nil {
		return argoCDState{}, fmt.Errorf("get application %s/%s: %w", namespace, ref.Name, err)
	}
	state := argoCDState{}
	state.Sync, _, _ = unstructured.NestedString(obj.Object, "status", "sync", "status")
	reconciled, _, _ := unstructured.NestedString(obj.Object, "status", "reconciledAt")
	state.ReconciledAt = parseTimestamp(reconciled)
	state.Revision, _, _ = unstructured.NestedString(obj.Object, "status", "sync", "revision")
	state.Revisions, _, _ = unstructured.NestedStringSlice(obj.Object, "status", "sync", "revisions")
	state.Health, _, _ = unstructured.NestedString(obj.Object, "status", "health", "status")
	state.Operation, _, _ = unstructured.NestedString(obj.Object, "status", "operationState", "phase")
	state.Deployments = deploymentsFrom(obj.Object)
	// 2026-11-11: OnDelete pod rotation is operator-owned. Exclude its revision
	// convergence from Application gating; Argo health still checks ready pods.
	resources, _, _ := unstructured.NestedSlice(obj.Object, "status", "resources")
	for _, resource := range resources {
		item, ok := resource.(map[string]interface{})
		if !ok || item["group"] != "apps" || item["kind"] != "StatefulSet" {
			continue
		}
		name, _ := item["name"].(string)
		ns, _ := item["namespace"].(string)
		sts, err := r.gateObject(ctx, schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"}, "statefulsets", types.NamespacedName{Namespace: ns, Name: name})
		if err != nil {
			return state, fmt.Errorf("read StatefulSet rollout %s/%s: %w", ns, name, err)
		}
		strategy, _, _ := unstructured.NestedString(sts.Object, "spec", "updateStrategy", "type")
		if strategy != "OnDelete" {
			continue
		}
		// No updatedReplicas requirement for OnDelete.
	}
	return state, nil
}

// deploymentsFrom collects the rollouts ArgoCD retains. status.history holds
// only completed deployments and is capped by spec.revisionHistoryLimit, so the
// in-flight operation is appended once it succeeds — history has not caught up
// to it yet, and that newest rollout is the one most likely to carry a commit
// still under watch.
func deploymentsFrom(app map[string]interface{}) []argoCDDeployment {
	history, _, _ := unstructured.NestedSlice(app, "status", "history")
	deployments := make([]argoCDDeployment, 0, len(history)+1)
	for _, item := range history {
		entry, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		revisions := revisionsOf(entry)
		if len(revisions) == 0 {
			continue
		}
		// deployStartedAt is when the rollout began; deployedAt is when it
		// finished. Only the start orders the rollout against a push, because a
		// rollout already running when the commit landed cannot contain it.
		started, _, _ := unstructured.NestedString(entry, "deployStartedAt")
		if started == "" {
			started, _, _ = unstructured.NestedString(entry, "deployedAt")
		}
		deployments = append(deployments, argoCDDeployment{Revisions: revisions, StartedAt: parseTimestamp(started)})
	}
	if phase, _, _ := unstructured.NestedString(app, "status", "operationState", "phase"); phase == "Succeeded" {
		result, _, _ := unstructured.NestedMap(app, "status", "operationState", "syncResult")
		if revisions := revisionsOf(result); len(revisions) > 0 {
			started, _, _ := unstructured.NestedString(app, "status", "operationState", "startedAt")
			deployments = append(deployments, argoCDDeployment{Revisions: revisions, StartedAt: parseTimestamp(started)})
		}
	}
	return deployments
}

// revisionsOf reads the revisions a history entry or sync result names. A
// single-source Application fills revision; a multi-source one fills revisions
// instead and leaves revision empty, so both have to be read or exact matching
// silently degrades to ordering alone on multi-source apps.
func revisionsOf(entry map[string]interface{}) []string {
	revisions, _, _ := unstructured.NestedStringSlice(entry, "revisions")
	if revision, _, _ := unstructured.NestedString(entry, "revision"); revision != "" {
		revisions = append(revisions, revision)
	}
	return revisions
}

// parseTimestamp reads an RFC3339 instant, yielding the zero time when it is
// absent or malformed. A zero start never satisfies the ordering test, so an
// unparseable timestamp can only make observes stricter, never looser.
func parseTimestamp(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func (r *TagUpdaterReconciler) doRollback(ctx context.Context, tu *v1alpha1.TagUpdater) error {
	badTag, previousTag := tu.Status.WatchingTag, tu.Status.PreviousTag
	if len(tu.Status.RollbackFields) > 0 {
		return r.restoreRelease(ctx, tu)
	}
	if !tu.Status.BaselineHealthy && tu.Spec.ArgoCDApp != nil {
		return fmt.Errorf("rollback requires a recorded Healthy baseline and manifest snapshot")
	}

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
		extra, resolveErr := resolver.Resolve(ctx, previousTag)
		if resolveErr != nil {
			return fmt.Errorf("resolve release record for rollback tag %s: %w", previousTag, resolveErr)
		}
		for key, value := range extra {
			data[key] = value
		}
	}
	if err := r.addRev(ctx, src, previousTag, data); err != nil {
		return err
	}
	if tu.Spec.Source.ResolveDigest {
		if !intsource.ValidDigest(tu.Status.PreviousImageDigest) {
			return fmt.Errorf("rollback requires recorded previous image digest")
		}
		data["image_digest"] = tu.Status.PreviousImageDigest
	}
	if err := addRollbackSignedDigest(tu, data); err != nil {
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
	tu.Status.LastImageDigest = data["image_digest"]
	tu.Status.LastSignedDigest = data["signed_digest"]
	tu.Status.ObservedGeneration = tu.Generation
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

// validateWriteBack requires a destination repository and branch, and exactly
// one way of naming manifests: spec.writeBack.path, or a path on every target.
func validateWriteBack(spec v1alpha1.WriteBackSpec, targets []v1alpha1.TargetSpec) error {
	var missing []string
	if strings.TrimSpace(spec.Repo) == "" {
		missing = append(missing, "repo")
	}
	if strings.TrimSpace(spec.Branch) == "" {
		missing = append(missing, "branch")
	}
	anyTargetPath := false
	for _, target := range targets {
		if strings.TrimSpace(target.Path) != "" {
			anyTargetPath = true
		}
	}
	if strings.TrimSpace(spec.Path) == "" && !anyTargetPath {
		missing = append(missing, "path")
	}
	if len(missing) > 0 {
		return fmt.Errorf("spec.writeBack is required with non-empty repo, branch, and path (or a path on every target); add missing field(s): %s", strings.Join(missing, ", "))
	}
	return writeback.ValidatePaths(spec, targets)
}

// writeBackPaths lists the distinct manifest files the spec writes, sorted.
func writeBackPaths(spec v1alpha1.TagUpdaterSpec) []string {
	seen := map[string]struct{}{}
	var paths []string
	for _, target := range spec.Targets {
		path := filepath.ToSlash(filepath.Clean(writeback.TargetPath(spec.WriteBack, target)))
		if _, found := seen[path]; !found {
			seen[path] = struct{}{}
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		paths = append(paths, spec.WriteBack.Path)
	}
	sort.Strings(paths)
	return paths
}

// validateFieldOwnership ensures one updater is the sole writer for a scalar
// in a manifest. Separate updaters advancing a store path, flake ref, or image
// field independently can produce a mixed release, even if each individual git
// commit is valid.
func validateFieldOwnership(updaters []v1alpha1.TagUpdater) error {
	owners := make(map[string]string)
	for _, updater := range updaters {
		for _, target := range updater.Spec.Targets {
			selector, err := json.Marshal(target.Selector)
			if err != nil {
				return fmt.Errorf("serialize selector for %s/%s: %w", updater.Namespace, updater.Name, err)
			}
			for _, patch := range target.Patches {
				key := strings.Join([]string{
					updater.Spec.WriteBack.Repo, updater.Spec.WriteBack.Branch,
					filepath.ToSlash(filepath.Clean(writeback.TargetPath(updater.Spec.WriteBack, target))),
					target.APIVersion, target.Kind, target.Namespace, target.Name, string(selector), patch.Field,
				}, "\x00")
				owner := updater.Namespace + "/" + updater.Name
				if previous, exists := owners[key]; exists && previous != owner {
					return fmt.Errorf("manifest field %s on %s/%s is owned by both %s and %s; consolidate it into one TagUpdater", patch.Field, target.Kind, target.Name, previous, owner)
				}
				owners[key] = owner
			}
		}
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
	started := r.clock()
	r.revPending.Store(&started, started)
	defer r.revPending.Delete(&started)
	revs, err := resolver.TagRevs(ctx)
	if err != nil {
		r.revFailingSince.CompareAndSwap(0, r.clock().UnixNano())
		return fmt.Errorf("resolve tag %q to commit sha: %w", tag, err)
	}
	sha := revs[tag]
	if sha == "" {
		r.revFailingSince.CompareAndSwap(0, r.clock().UnixNano())
		return fmt.Errorf("tag %q resolved to no commit sha; refusing impure tag-only flakeRef", tag)
	}
	data["rev"] = sha
	r.revFailingSince.Store(0)
	return nil
}

func (r *TagUpdaterReconciler) RevResolutionHealthz() healthz.Checker {
	return func(*http.Request) error {
		return r.revResolutionHealth(time.Now())
	}
}

func (r *TagUpdaterReconciler) revResolutionHealth(now time.Time) error {
	// 2026-11-11: idle git streams can poll less often than the watchdog.
	// Only sustained resolution failures start its deadline; success clears it.
	last := r.revFailingSince.Load()
	anchor := "failing"
	r.revPending.Range(func(_, value any) bool {
		if started := value.(time.Time).UnixNano(); last == 0 || started < last {
			last = started
			anchor = "pending"
		}
		return true
	})
	if last == 0 {
		return nil
	}
	if age := now.Sub(time.Unix(0, last)); age > revStaleAfter {
		return fmt.Errorf("git tag->rev resolution %s for %s (>%s)", anchor, age.Round(time.Second), revStaleAfter)
	}
	return nil
}

func (r *TagUpdaterReconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// transientFailure records cause like setFailedBeforeWriteBack but, instead of
// returning it to controller-runtime (whose rate limiter ignores spec.interval),
// requeues on the bounded fast-retry schedule and then on spec.interval. The
// error is logged and shown in Ready=False/Error rather than returned.
func (r *TagUpdaterReconciler) transientFailure(ctx context.Context, tu *v1alpha1.TagUpdater, key, tag string, interval time.Duration, cause error) (ctrl.Result, error) {
	_ = r.setFailedBeforeWriteBack(ctx, tu, cause)
	after := r.fastRetry.next(key, tag, r.clock(), interval)
	log.FromContext(ctx).Error(cause, "transient source error; will retry", "tag", tag, "after", after.String())
	return ctrl.Result{RequeueAfter: after}, nil
}

// setWriteBackNotAttempted records that this reconcile ended before the
// write-back leg ran, so WriteBackReady describes THIS pass instead of
// silently retaining the verdict of the last pass that reached the writer.
// Unknown (not False) is the honest status: nothing was tried, so nothing
// failed.
func setWriteBackNotAttempted(tu *v1alpha1.TagUpdater, cause string) {
	meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{
		Type:    "WriteBackReady",
		Status:  metav1.ConditionUnknown,
		Reason:  "NotAttempted",
		Message: "write-back not attempted this reconcile: " + cause,
	})
}

// setFailedBeforeWriteBack is setFailed for the error paths that abort ahead of
// the write-back leg (source construction, tag listing, pattern compilation,
// release-record resolution, tag->rev resolution). Those must clear
// WriteBackReady too; setFailed alone touches only Ready, which is how a CR can
// sit at Ready=False/Error while WriteBackReady still reports a stale failure
// from a spec or credential that was fixed long ago.
func (r *TagUpdaterReconciler) setFailedBeforeWriteBack(ctx context.Context, tu *v1alpha1.TagUpdater, cause error) error {
	setWriteBackNotAttempted(tu, cause.Error())
	return r.setFailed(ctx, tu, cause)
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
	metrics.Registry.MustRegister(progressCollector{tracker: &r.progress}, rollbackCount, suppressedCount, ambiguousGate, pendingTooLong)
	return r.controllerBuilder(mgr)
}

func (r *TagUpdaterReconciler) releaseHealth(ctx context.Context, tu *v1alpha1.TagUpdater) (argoCDState, error) {
	aggregate := argoCDState{Health: "Healthy", Sync: "Synced"}
	for _, ref := range healthApps(tu) {
		state, err := r.argoCDAppState(ctx, &ref)
		if err != nil {
			return argoCDState{}, err
		}
		if aggregate.ReconciledAt.IsZero() || state.ReconciledAt.Before(aggregate.ReconciledAt) {
			aggregate.ReconciledAt = state.ReconciledAt
		}
		if state.Health != "Healthy" {
			if aggregate.Health != "Degraded" {
				aggregate.Health = state.Health
			}
		}
		if state.Sync != "Synced" {
			aggregate.Sync = state.Sync
		}
		if state.syncInFlight() {
			aggregate.Operation = "Running"
		}
	}
	return aggregate, nil
}
