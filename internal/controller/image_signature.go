package controller

import (
	"context"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	intsource "github.com/jakeschurch/argocd-tag-updater/internal/source"
)

const signatureCondition = "SignatureVerified"

// signedDigests remembers digests whose signed marker was seen, keyed by
// repository and digest. A digest is immutable and the marker is only ever
// added, so a positive answer stays true. Absence is never cached: the marker
// is expected to appear shortly after the tag.
type signedDigests struct{ seen sync.Map }

func (s *signedDigests) has(repository, digest string) bool {
	_, ok := s.seen.Load(repository + "@" + digest)
	return ok
}

func (s *signedDigests) add(repository, digest string) {
	s.seen.Store(repository+"@"+digest, struct{}{})
}

// validateRequireSigned rejects configurations whose signature repository is unknown.
func validateRequireSigned(spec v1alpha1.SourceSpec) error {
	switch spec.RequireSigned {
	case "":
		return nil
	case v1alpha1.SignatureModeMarker:
	default:
		return fmt.Errorf("unsupported source.requireSigned %q", spec.RequireSigned)
	}
	switch spec.Type {
	case v1alpha1.SourceTypeOCI:
		return nil
	case v1alpha1.SourceTypeGit:
		if spec.DigestRepo == "" {
			return fmt.Errorf("source.requireSigned on a git source requires source.digestRepo")
		}
		return nil
	}
	return fmt.Errorf("source.requireSigned is supported only for OCI sources and git sources with source.digestRepo")
}

// signatureRegistry returns the repository whose tags carry tag's signature, with credentials.
func (r *TagUpdaterReconciler) signatureRegistry(ctx context.Context, tu *v1alpha1.TagUpdater, ociBasicAuth string) (*intsource.OCI, error) {
	spec := tu.Spec.Source
	if spec.Type == v1alpha1.SourceTypeOCI {
		return &intsource.OCI{Repo: spec.Repo, BasicAuth: ociBasicAuth, AuthRealm: spec.DigestAuthRealm}, nil
	}
	authentication := ""
	if secret := spec.ImagePullSecretRef; secret != nil {
		var err error
		authentication, err = r.resolveDockerAuth(ctx, tu.Namespace, secret.Name, spec.DigestRepo)
		if err != nil {
			return nil, fmt.Errorf("resolve signature registry credentials: %w", err)
		}
	}
	return &intsource.OCI{Repo: spec.DigestRepo, BasicAuth: authentication, AuthRealm: spec.DigestAuthRealm}, nil
}

// verifySigned reports whether tag's image carries the signed marker. An error
// means the answer is unknown, and the caller must write nothing.
//
// The tag is resolved again on every call: tags move, digests do not. When
// digest resolution already ran for this tag its digest must agree, so the
// written digest is the one verified.
func (r *TagUpdaterReconciler) verifySigned(ctx context.Context, tu *v1alpha1.TagUpdater, ociBasicAuth, tag string, data map[string]string) (bool, string, error) {
	registry, err := r.signatureRegistry(ctx, tu, ociBasicAuth)
	if err != nil {
		return false, "", err
	}
	digest, err := registry.Digest(ctx, tag)
	if err != nil {
		return false, "", fmt.Errorf("resolve digest of tag %s for signature check: %w", tag, err)
	}
	if resolved := data["image_digest"]; resolved != "" && resolved != digest {
		return false, "", fmt.Errorf("tag %s moved during resolution (%s, then %s); refusing to write", tag, resolved, digest)
	}
	if r.signed.has(registry.Repo, digest) {
		return true, digest, nil
	}
	marker := intsource.SignedMarkerTag(digest)
	found, err := registry.HasTag(ctx, marker)
	if err != nil {
		return false, "", fmt.Errorf("check signed marker %s: %w", marker, err)
	}
	if found {
		r.signed.add(registry.Repo, digest)
	}
	return found, digest, nil
}

// awaitSignature records that tag is withheld for a missing signature. The
// event fires once per held tag, not on every poll.
func (r *TagUpdaterReconciler) awaitSignature(ctx context.Context, tu *v1alpha1.TagUpdater, tag, digest string) error {
	message := fmt.Sprintf("tag %s (%s) has no signed marker %s; not writing it until it is signed", tag, digest, intsource.SignedMarkerTag(digest))
	log.FromContext(ctx).Info("withholding unsigned tag", "tag", tag, "digest", digest)
	if tu.Status.AwaitingSignatureTag != tag && r.Recorder != nil {
		r.Recorder.Event(tu, corev1.EventTypeWarning, "AwaitingSignature", message)
	}
	tu.Status.AwaitingSignatureTag = tag
	meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: signatureCondition, Status: metav1.ConditionFalse, Reason: "AwaitingSignature", Message: message})
	meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "AwaitingSignature", Message: message})
	setWriteBackNotAttempted(tu, message)
	return r.Status().Update(ctx, tu)
}

func markSigned(tu *v1alpha1.TagUpdater, tag, digest string) {
	tu.Status.AwaitingSignatureTag = ""
	meta.SetStatusCondition(&tu.Status.Conditions, metav1.Condition{Type: signatureCondition, Status: metav1.ConditionTrue, Reason: "SignedMarkerFound", Message: fmt.Sprintf("tag %s (%s) carries signed marker %s", tag, digest, intsource.SignedMarkerTag(digest))})
}

// clearSignatureStatus drops signature state left by a requireSigned since unset.
func clearSignatureStatus(tu *v1alpha1.TagUpdater) {
	tu.Status.AwaitingSignatureTag = ""
	meta.RemoveStatusCondition(&tu.Status.Conditions, signatureCondition)
}
