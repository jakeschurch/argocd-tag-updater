package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type SourceType string

const (
	SourceTypeGit SourceType = "git"
	SourceTypeOCI SourceType = "oci"
)

type SourceSpec struct {
	Type SourceType `json:"type"`
	// Repo is a git remote URL (git source) or OCI repository reference (oci source).
	Repo string `json:"repo"`
	// TagPattern is a named-group regex. Captures are available in Patch templates.
	// The capture named "n" is used as the sort key to select the latest tag.
	// e.g.: platform\.(?P<branch>[^.]+)\.build-(?P<n>\d+)\.(?P<sha>[0-9a-f]{6,})
	TagPattern string `json:"tagPattern"`
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
}

type TagUpdaterStatus struct {
	LastTag     string             `json:"lastTag,omitempty"`
	LastUpdated *metav1.Time       `json:"lastUpdated,omitempty"`
	Conditions  []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type TagUpdaterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TagUpdater `json:"items"`
}
