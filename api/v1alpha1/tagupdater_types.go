package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type SourceType string

const (
	SourceTypeGit SourceType = "git"
	SourceTypeOCI SourceType = "oci"
	SourceTypeNix SourceType = "nix"
)

// LocalObjectReference names a secret in the same namespace as the TagUpdater.
type LocalObjectReference struct {
	Name string `json:"name"`
}

type SourceSpec struct {
	Type SourceType `json:"type"`
	// Repo is a git remote URL (git source), OCI repository reference (oci source),
	// or nix cache tag namespace "<host>/<name>" (nix source).
	Repo string `json:"repo"`
	// TagPattern is a named-group regex. Captures are available in Patch templates.
	// The capture named "n" is used as the sort key to select the latest tag.
	// e.g.: platform\.(?P<branch>[^.]+)\.build-(?P<n>\d+)\.(?P<sha>[0-9a-f]{6,})
	TagPattern string `json:"tagPattern"`
	// ImagePullSecretRef names a kubernetes.io/dockerconfigjson Secret in the same
	// namespace as the TagUpdater. When set on an oci source, the controller reads
	// the secret and uses its credentials to authenticate against the registry.
	ImagePullSecretRef *LocalObjectReference `json:"imagePullSecretRef,omitempty"`
}

type PatchSpec struct {
	// Field is a dot-notation path into the target CR. e.g. "spec.flakeRef" or
	// "spec.source.helm.valuesObject.image.tag".
	Field string `json:"field"`
	// Template is a Go template rendered with named captures from TagPattern plus
	// repo-derived fields (owner, repo, host, repoURL) and "tag" (full tag string).
	Template string `json:"template"`
}

type TargetSpec struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	// Name selects a specific CR by name. Mutually exclusive with Selector.
	Name string `json:"name,omitempty"`
	// Namespace scopes the lookup. Required for namespaced resources.
	Namespace string `json:"namespace,omitempty"`
	// Selector dynamically selects CRs by label. All matching CRs receive every patch.
	// Mutually exclusive with Name.
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
	// Patches is the list of field+template pairs to apply to each matched CR.
	Patches []PatchSpec `json:"patches"`
}

type ArgoCDAppRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"` // defaults to "argocd"
}

// RollbackSpec configures automatic rollback when a tag deployment is unhealthy.
type RollbackSpec struct {
	Enabled bool `json:"enabled,omitempty"`
	// Timeout is how long to wait for ArgoCD health before rolling back.
	// Defaults to 10m. Should be >= the target Deployment's progressDeadlineSeconds.
	Timeout metav1.Duration `json:"timeout,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Repo",type=string,JSONPath=`.spec.source.repo`
// +kubebuilder:printcolumn:name="Last Tag",type=string,JSONPath=`.status.lastTag`
// +kubebuilder:printcolumn:name="Updated",type=date,JSONPath=`.status.lastUpdated`
type TagUpdater struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TagUpdaterSpec   `json:"spec,omitempty"`
	Status TagUpdaterStatus `json:"status,omitempty"`
}

type TagUpdaterSpec struct {
	Source SourceSpec `json:"source"`
	// Targets is the list of CR groups to patch when a new tag matches.
	Targets []TargetSpec `json:"targets"`
	// Interval between tag polls. Defaults to 2m.
	Interval metav1.Duration `json:"interval,omitempty"`
	// ArgoCDApp triggers a sync on the named Application after all patches are applied.
	ArgoCDApp *ArgoCDAppRef `json:"argoCDApp,omitempty"`
	// ManagingApp is the app-of-apps that syncs the target Applications from git.
	// The controller ensures RespectIgnoreDifferences=true is in its syncOptions
	// so TagUpdater patches on child Applications survive selfHeal cycles.
	ManagingApp *ArgoCDAppRef `json:"managingApp,omitempty"`
	// Rollback configures automatic rollback when a newly-applied tag fails to
	// deploy successfully. Requires ArgoCDApp to be set.
	Rollback *RollbackSpec `json:"rollback,omitempty"`
}

type TagUpdaterStatus struct {
	LastTag     string             `json:"lastTag,omitempty"`
	LastUpdated *metav1.Time       `json:"lastUpdated,omitempty"`
	Conditions  []metav1.Condition `json:"conditions,omitempty"`
	// PreviousTag is the tag that was applied before LastTag.
	PreviousTag string `json:"previousTag,omitempty"`
	// SkippedTags is the list of tags that failed to deploy and were rolled back.
	// These tags are excluded from future Latest() selection until a newer tag
	// deploys successfully, at which point the list is cleared.
	SkippedTags []string `json:"skippedTags,omitempty"`
	// WatchingTag is the tag whose ArgoCD health is currently being monitored.
	// Set after a patch is applied; cleared once the app is healthy or rolled back.
	WatchingTag string `json:"watchingTag,omitempty"`
	// WatchingSince is when health monitoring for WatchingTag started.
	WatchingSince *metav1.Time `json:"watchingSince,omitempty"`
}

// +kubebuilder:object:root=true
type TagUpdaterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TagUpdater `json:"items"`
}
