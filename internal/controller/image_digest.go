package controller

import (
	"context"
	"fmt"
	v1alpha1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
	intsource "github.com/jakeschurch/argocd-tag-updater/internal/source"
)

func (r *TagUpdaterReconciler) addImageDigest(ctx context.Context, updater *v1alpha1.TagUpdater, tag string, data map[string]string) error {
	if !updater.Spec.Source.ResolveDigest {
		return nil
	}
	repository := updater.Spec.Source.DigestRepo
	if updater.Spec.Source.Type != v1alpha1.SourceTypeGit {
		return fmt.Errorf("digest resolution requires a git release source")
	}
	if repository == "" {
		return fmt.Errorf("git digest resolution requires source.digestRepo")
	}
	authentication := ""
	if secret := updater.Spec.Source.ImagePullSecretRef; secret != nil {
		var err error
		authentication, err = r.resolveDockerAuth(ctx, updater.Namespace, secret.Name, repository)
		if err != nil {
			return fmt.Errorf("resolve digest registry credentials: %w", err)
		}
	}
	digest, err := (&intsource.OCI{Repo: repository, BasicAuth: authentication, AuthRealm: updater.Spec.Source.DigestAuthRealm}).Digest(ctx, tag)
	if err != nil {
		return err
	}
	data["image_digest"] = digest
	return nil
}
