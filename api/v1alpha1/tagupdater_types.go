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
	// TagPattern is a named-group regex. Captures are available as
	// .CaptureNames in Template. e.g.:
	//   platform\.(?P<branch>[^.]+)\.build-(?P<n>\d+)\.(?P<sha>[0-9a-f]{6,})
	// The capture named "n" is used as the sort key to select the latest tag.
	TagPattern string `json:"tagPattern"`
}

type TargetSpec struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace,omitempty"`
	// Field is a dot-notation path to the field to patch. e.g. "spec.nixExpr"
	Field string `json:"field"`
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
	Source   SourceSpec      `json:"source"`
	Target   TargetSpec      `json:"target"`
	// Template is a Go template rendered with named captures from TagPattern.
	// Use {{ .sha }}, {{ .branch }}, {{ .n }}, {{ .tag }} etc.
	Template string          `json:"template"`
	// Interval between tag polls. Defaults to 2m.
	Interval metav1.Duration `json:"interval,omitempty"`
	// ArgoCDApp triggers a sync on the named Application after patching.
	ArgoCDApp *ArgoCDAppRef `json:"argoCDApp,omitempty"`
}

type TagUpdaterStatus struct {
	// LastTag is the most recent matched tag.
	LastTag     string             `json:"lastTag,omitempty"`
	// LastApplied is the rendered template value last written to the target.
	LastApplied string             `json:"lastApplied,omitempty"`
	LastUpdated *metav1.Time       `json:"lastUpdated,omitempty"`
	Conditions  []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type TagUpdaterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TagUpdater `json:"items"`
}
