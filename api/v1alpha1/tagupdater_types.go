package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type SourceType string

const (
	SourceTypeGit SourceType = "git"
	SourceTypeOCI SourceType = "oci"
	SourceTypeNix SourceType = "nix"
)

// SignatureMode selects the evidence a candidate release must carry before it
// is written. See SourceSpec.RequireSigned.
type SignatureMode string

const (
	// SignatureModeMarker requires the release pipeline's signed-marker tag:
	// the candidate's manifest digest with ':' replaced by '-', plus ".signed",
	// in the same repository (sha256:abc... -> sha256-abc....signed).
	SignatureModeMarker SignatureMode = "marker"
	// TODO: a "cosign" mode verifying the signature against a public key Secret.
	// It needs sigstore dependencies this module does not vendor yet.
)

// LocalObjectReference names a secret in the same namespace as the TagUpdater.
type LocalObjectReference struct {
	Name string `json:"name"`
}

type SourceSpec struct {
	// DigestAuthRealm explicitly trusts a cross-origin HTTPS Bearer token endpoint.
	DigestAuthRealm string `json:"digestAuthRealm,omitempty"`
	// ResolveDigest enables exact-tag OCI manifest digest lookup for a git release source.
	// +kubebuilder:default=false
	ResolveDigest bool `json:"resolveDigest,omitempty"`
	// DigestRepo is the OCI repository whose exact git release tag is resolved.
	// It is also the repository RequireSigned checks for a git source.
	DigestRepo string `json:"digestRepo,omitempty"`
	// RequireSigned withholds a new tag until its image is signed. With "marker",
	// the newest matching tag is resolved to its manifest digest and written only
	// when the repository also holds the tag <digest with ':' replaced by '-'>.signed.
	// Until then nothing is written, the SignatureVerified condition is False with
	// reason AwaitingSignature, and the tag is checked again every interval, so it
	// deploys once signed. A registry error writes nothing. The repository is
	// source.repo for an OCI source and source.digestRepo for a git source; nix
	// sources are rejected. The verified digest is available to templates as
	// "signed_digest". Unset keeps tag-only behavior.
	// +kubebuilder:validation:Enum=marker
	RequireSigned SignatureMode `json:"requireSigned,omitempty"`
	Type          SourceType    `json:"type"`
	// Repo is a git remote URL (git source), OCI repository reference (oci source),
	// or nix cache tag namespace "<host>/<name>" (nix source).
	Repo string `json:"repo"`
	// TagPattern is a named-group regex. Captures are available in Patch templates.
	// The capture named "n" is used as the sort key to select the latest tag.
	// e.g.: platform\.(?P<branch>[^.]+)\.build-(?P<n>\d+)\.(?P<sha>[0-9a-f]{6,})
	TagPattern string `json:"tagPattern"`
	// ImagePullSecretRef names a kubernetes.io/dockerconfigjson Secret in the same
	// namespace as the TagUpdater. For OCI sources or opted-in digest resolution, it reads
	// the secret and uses its credentials to authenticate against the registry.
	ImagePullSecretRef *LocalObjectReference `json:"imagePullSecretRef,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.operation != 'remove' || has(self.expectedValue)",message="remove requires expectedValue"
// +kubebuilder:validation:XValidation:rule="self.operation != 'remove' || !has(self.template) || size(self.template) == 0",message="remove cannot render a template"
// +kubebuilder:validation:XValidation:rule="self.operation == 'remove' || !has(self.expectedValue)",message="expectedValue is only supported for remove"
type PatchSpec struct {
	// Field is a dot-notation path into the target manifest. e.g. "spec.flakeRef" or
	// "spec.source.helm.valuesObject.image.tag".
	Field string `json:"field"`
	// Operation defaults to set. Remove deletes a block mapping's single-line
	// string scalar only when it equals ExpectedValue; absent fields are a no-op.
	// +kubebuilder:validation:Enum=set;remove
	// +kubebuilder:default=set
	Operation string `json:"operation,omitempty"`
	// ExpectedValue is required for remove, including an explicitly empty string.
	ExpectedValue *string `json:"expectedValue,omitempty"`
	// Template is a Go template rendered with named captures from TagPattern plus
	// repo-derived fields (owner, repo, host, repoURL), "tag" (full tag string),
	// and — for the git source — "rev", the matched tag's immutable full commit
	// sha, enabling an immutable flake ref like
	// "github:{{ .owner }}/{{ .repo }}/{{ .tag }}?rev={{ .rev }}#attr".
	// An absent key renders empty and the whole field is skipped (never blanked).
	Template string `json:"template,omitempty"`
}

type TargetSpec struct {
	// Path is the repository-relative path, inside writeBack.repo, of the YAML
	// manifest holding this target. Set it on every target, and leave
	// writeBack.path empty, to update several files from one tag: all files
	// are edited in a single commit and pushed once, and if any target fails
	// nothing is written. Leave it empty on every target to use writeBack.path.
	// Setting both writeBack.path and a target path is rejected.
	Path       string `json:"path,omitempty"`
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

// RollbackSpec configures a git write-back rollback when a deployed tag is unhealthy.
type RollbackSpec struct {
	Enabled bool `json:"enabled,omitempty"`
	// Timeout is how long to wait for the workload to become Healthy after ArgoCD
	// observes the pushed manifest commit. It does not include repository polling
	// latency. Defaults to 20m.
	Timeout metav1.Duration `json:"timeout,omitempty"`
}

// WriteBackSpec identifies the git repository and manifest file(s) that are the
// sole update target.
type WriteBackSpec struct {
	// Repo is the git repository containing the target manifest. It is independent
	// of source.repo, which is only the tag source.
	Repo string `json:"repo"`
	// Branch is the branch cloned and pushed by the controller.
	Branch string `json:"branch"`
	// Path is the repository-relative path to a YAML manifest. Multi-document YAML
	// is supported; target identity selects the document to edit. Exactly one of
	// path and per-target spec.targets[].path is required: leave this empty
	// when every target names its own file.
	Path string `json:"path,omitempty"`
	// CredentialsSecretRef optionally names a Secret in the TagUpdater namespace.
	// It takes precedence over the controller's mounted Git credentials. Supported
	// authentication keys are token (optionally with username), username/password,
	// and sshPrivateKey. SSH additionally requires knownHosts, or
	// insecureIgnoreHostKey=true as an explicit opt-in.
	//
	// Token auth pushes over HTTP(S) as the Secret's username (a dedicated robot
	// account), so the git host records that account as the pusher; set repo to an
	// http:// or https:// URL. A token without username authenticates as
	// "x-access-token". If sshPrivateKey is also present, SSH wins.
	CredentialsSecretRef LocalObjectReference `json:"credentialsSecretRef,omitempty"`
	// CommitAuthor overrides the git author identity of the write-back commit.
	// By default SSH writes use argocd-tag-updater / argocd-tag-updater@localhost,
	// while token writes authenticated with a username use that username and
	// <username>@noreply.<repo host>.
	CommitAuthor CommitAuthorSpec `json:"commitAuthor,omitempty"`
}

// CommitAuthorSpec is a git author identity. Unset fields keep the default.
type CommitAuthorSpec struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
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
	// Targets identifies manifest documents and fields to update when a new tag matches.
	Targets []TargetSpec `json:"targets"`
	// Interval between tag polls. Defaults to 2m.
	Interval metav1.Duration `json:"interval,omitempty"`
	// WriteBack is the required git destination for rendered target patches.
	// Its credentialsSecretRef is optional when the controller has mounted Git
	// credentials configured through GIT_SSH_KEY_FILE.
	WriteBack WriteBackSpec `json:"writeBack"`
	// ArgoCDApp is a reference used to observe sync revision and health. The controller
	// never syncs this Application. It asks it to refresh after a write-back unless
	// ManagingApp is set.
	ArgoCDApp *ArgoCDAppRef `json:"argoCDApp,omitempty"`
	// ManagingApp optionally identifies the app-of-apps Application whose sync revision
	// must observe the pushed manifest commit, and which is asked to refresh after a
	// write-back. When omitted, ArgoCDApp is used for both revision and health observation.
	ManagingApp *ArgoCDAppRef `json:"managingApp,omitempty"`
	// Rollback commits the previous tag's rendered values when ArgoCD reports a
	// terminal deployment failure. Requires ArgoCDApp to be set.
	Rollback *RollbackSpec `json:"rollback,omitempty"`
}

type TagUpdaterStatus struct {
	// LastImageDigest and PreviousImageDigest preserve immutable release identities.
	LastImageDigest     string `json:"lastImageDigest,omitempty"`
	PreviousImageDigest string `json:"previousImageDigest,omitempty"`
	// ObservedGeneration gates the remote-head shortcut after configuration changes.
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	LastTag            string             `json:"lastTag,omitempty"`
	LastUpdated        *metav1.Time       `json:"lastUpdated,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	// LastWriteCommit is the manifest branch commit verified by the last
	// successful write-back. It enables a cheap remote-head guard before cloning.
	LastWriteCommit string `json:"lastWriteCommit,omitempty"`
	// PreviousTag is the tag applied before LastTag.
	PreviousTag string `json:"previousTag,omitempty"`
	// SkippedTags contains failed tags excluded from Latest() until a newer tag
	// deploys successfully.
	SkippedTags []string `json:"skippedTags,omitempty"`
	// LastSignedDigest and PreviousSignedDigest are the digests verified by
	// spec.source.requireSigned for LastTag and PreviousTag. Rollback renders
	// "signed_digest" from PreviousSignedDigest.
	LastSignedDigest     string `json:"lastSignedDigest,omitempty"`
	PreviousSignedDigest string `json:"previousSignedDigest,omitempty"`
	// AwaitingSignatureTag is the newest matching tag held back by
	// spec.source.requireSigned because its image carries no signature yet.
	AwaitingSignatureTag string `json:"awaitingSignatureTag,omitempty"`
	// WatchingTag is the deployed tag currently under health observation.
	WatchingTag string `json:"watchingTag,omitempty"`
	// WatchingCommit is the pushed manifest commit ArgoCD must observe before the
	// health timeout clock starts.
	WatchingCommit string `json:"watchingCommit,omitempty"`
	// WatchingArmedAt is when the watch was armed, i.e. when WatchingCommit was
	// written. It bounds the phase BEFORE ArgoCD observes the commit.
	//
	// Without it that phase is unbounded: WatchingSince only starts once ArgoCD
	// reports the commit, the rollback timeout is measured from WatchingSince,
	// and Reconcile short-circuits all tag work while WatchingTag is set. An
	// ArgoCD that never picks the commit up (app renamed or deleted, repo
	// credentials broken, branch mismatch, controller down) would therefore
	// freeze the updater permanently while still reporting success.
	WatchingArmedAt *metav1.Time `json:"watchingArmedAt,omitempty"`
	// WatchingSince is set when ArgoCD status.sync.revision equals WatchingCommit.
	WatchingSince *metav1.Time `json:"watchingSince,omitempty"`
}

// +kubebuilder:object:root=true
type TagUpdaterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TagUpdater `json:"items"`
}
