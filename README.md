# argocd-tag-updater

A generic Kubernetes controller that watches a tag source, renders Go templates with named captures, and commits the resulting field values into Kubernetes manifests in git for ArgoCD to apply.

Inspired by [ArgoCD Image Updater](https://github.com/argoproj-labs/argocd-image-updater), but decoupled from OCI registries and generalised to any CR field.

## How it works

1. A `TagUpdater` CR declares a source, a tag pattern (named-group regex), a target CR + field, and a Go template.
2. The controller polls the source for tags on a configurable interval.
3. When a new tag matches the pattern, named captures are extracted and passed to the template.
4. The rendered value is written surgically into the configured manifest.
5. The controller commits and pushes the manifest; ArgoCD converges from git.

### Requeue timing

A steady updater, or one whose tag was written to git, waits the full `spec.interval` between polls (a webhook can still wake it earlier). A wake can arrive before the newest tag is usable, though, and idling a whole interval after the missing piece appears is slow. So while the newest candidate is in a transient state, the controller requeues on a short doubling backoff (15s, 30s, 1m, then 2m, never above `spec.interval`) instead:

- the tag is `AwaitingSignature` (no signed marker yet);
- a source or registry lookup failed: the git fetch or tag listing, the nix-cache release record, the registry digest lookup, or the signature check.

The fast phase lasts at most 15 minutes from when that state first appeared for the tag (kept in memory; a controller restart starts a new window). After that the updater falls back to `spec.interval` until the state clears. Failures in this set are logged and shown as `Ready=False` / `Error` but are not returned to controller-runtime, so its workqueue rate limiter does not apply and `controller_runtime_reconcile_errors_total` does not count them. No CRD change.

## Example use case — Nix flake refs

Push a git tag from CI:

```
platform.main.build-42.abc1234
```

A `TagUpdater` matches it and updates `spec.flakeRef` in the NixMount manifest:

```
github:your-org/your-flake/abc1234#packages.x86_64-linux.platform
```

ArgoCD syncs, nix-csi picks up the new flake ref, pods roll with the new closure. No OCI registry involved.

## TagUpdater spec

```yaml
apiVersion: updater.argocd.io/v1alpha1
kind: TagUpdater
metadata:
  name: example-flake-platform
  namespace: argocd
spec:
  source:
    type: git                                   # git | oci (oci is a stub)
    repo: git@github.com:your-org/your-flake.git
    tagPattern: 'platform\.(?P<branch>[^.]+)\.build-(?P<n>\d+)\.(?P<sha>[0-9a-f]{6,})'
  targets:
    - apiVersion: nix.csi.k8s.io/v1alpha1
      kind: NixMount
      name: example-flake-platform
      namespace: example-flake
      patches:
        - field: spec.flakeRef
          template: 'github:your-org/your-flake/{{ .sha }}#packages.x86_64-linux.platform'
  writeBack:
    repo: git@github.com:your-org/cluster-manifests.git
    branch: main
    path: apps/example-flake-platform.yaml
  argoCDApp:
    name: example-flake
    namespace: argocd
  managingApp:
    name: platform-apps
    namespace: argocd
  rollback:
    enabled: true
    timeout: 20m
  interval: 2m
```

### Tag pattern

`tagPattern` is a Go named-group regex. Captures are available in `template` as `{{ .captureName }}`. The capture named `n` is used as the sort key to select the latest tag (parsed as an integer, falls back to lexicographic).

`{{ .tag }}` is always available as the full matched tag string.

### Template

Standard Go `text/template` rendered with all named captures as a flat `map[string]string`. The rendered string is written verbatim to the target field.

### Target field

`field` uses dot-notation (`spec.flakeRef`, `spec.sources.0.targetRevision`) to navigate YAML mappings and numeric sequence indices.

Patches default to `operation: set`, preserving existing template behavior.
For an owner-approved obsolete string field, use a guarded removal:

```yaml
patches:
  - field: spec.settings.obsoleteMode
    operation: remove
    expectedValue: old-mode
```

Removal requires an exact string `expectedValue` and no template. An absent
field or mapping is already converged; a different value aborts the entire
write-back without a commit. Removal supports standalone, single-line scalar
entries in block mappings with a retained sibling, including paths through sequence indices. It rejects
collections, flow mappings, duplicate keys, sequence-element deletion, anchors and multiline
values. Unrelated pins, comments and formatting retain their original bytes.
Changing a TagUpdater's generation runs its patches even when the tag is unchanged.

Install the updated CRD and controller before enabling removal patches. A source
PR or offline patch preview does not authorize publishing a managed manifest.

### Git write-back (required)

`spec.writeBack` is the controller's only update destination:

```yaml
spec:
  writeBack:
    repo: git@github.com:your-org/cluster-manifests.git
    branch: main
    path: apps/example.yaml
    credentialsSecretRef:
      name: manifest-git-credentials
```

By default, write-back uses the controller's mounted `GIT_SSH_KEY_FILE` and
`GIT_KNOWN_HOSTS_FILE` credentials. To override them per updater, set
`credentialsSecretRef`; the Secret must be in the TagUpdater namespace and
contain `token`, `username` plus `password`, or `sshPrivateKey`. SSH credentials
must also contain a `knownHosts` entry; `insecureIgnoreHostKey: "true"` is
available only as an explicit opt-in.

**HTTPS token write-back (robot identity).** SSH deploy keys make the git host
record the key's repo owner as the pusher. To push as a dedicated robot account
instead, set `writeBack.repo` to an `http://` or `https://` URL and put
`token` plus `username` (the robot's login) in the Secret; the controller
authenticates with HTTP basic auth `username:token`. SSH remains the default;
if the Secret also holds `sshPrivateKey`, SSH wins, so remove it when
migrating. A token without `username` authenticates as `x-access-token`.
An `ssh://` or scp-style repo URL is rewritten to `https://` with the SSH port
dropped, but in-cluster hosts that serve plain HTTP on a custom port need the
explicit `http://host:port/...` URL.

```yaml
spec:
  writeBack:
    repo: http://forgejo-http.forgejo.svc.cluster.local:3000/jake/homelab.git
    branch: v1
    path: vmetal/apps/argocd-tag-updater.yaml
    credentialsSecretRef:
      name: argocd-tag-updater-forgejo-writeback   # keys: token, username
    commitAuthor:          # optional
      name: margo-robot
      email: margo-robot@noreply.forgejo.example
```

The commit author defaults to `argocd-tag-updater <argocd-tag-updater@localhost>`
for SSH and username/password. With token auth and a `username` it defaults to that username and
`<username>@noreply.<repo host>`; `commitAuthor.name`/`email` override either
default per field. The author is informational: the git host attributes the push
to the authenticated account, so identity checks must key on the pusher, not the
commit author email.

The manifest may contain
multiple YAML documents; each target is selected by apiVersion, kind,
name/namespace or label selector. Only configured scalar field tokens are
replaced, preserving comments and unrelated formatting. No commit is created
when every rendered value is already present.

#### Several files from one tag

When one tag stream pins values in more than one manifest file, give every
target its own `path` and leave `writeBack.path` empty. All files are edited
in one commit and pushed once, instead of one commit per TagUpdater:

```yaml
spec:
  targets:
    - path: apps/nix-farm.yaml
      apiVersion: argoproj.io/v1alpha1
      kind: Application
      name: nix-farm
      namespace: argocd
      patches:
        - field: spec.sources.0.helm.valuesObject.image.tag
          template: '{{ .tag }}'
    - path: apps/nix-csi-go.yaml
      apiVersion: argoproj.io/v1alpha1
      kind: Application
      name: nix-csi-go
      namespace: argocd
      patches:
        - field: spec.sources.0.helm.valuesObject.farmSubmitters.image
          template: harbor.example/foundry/nix-farm@{{ .image_digest }}
  writeBack:
    repo: ssh://git@forgejo.example:22/foundry-labs/foundrybox.git
    branch: main
    # no path: each target names its file
```

- Use one form only. Either set `writeBack.path` and no target `path`, or set
  a `path` on every target and leave `writeBack.path` empty. Mixing the two is
  rejected with `WriteBackReady=False` (reason `MissingWriteBackConfig`).
- Targets that name the same file (after path cleaning, so `./a.yaml` is
  `a.yaml`) are applied together.
- The write is all or nothing. Every file is edited in memory first. If any
  target is missing from its file, or any file is missing, nothing is
  committed.
- A rejected push is retried by recloning the branch and re-applying every
  file, as with a single path.
- Rollback rewrites every file in one commit. `status.lastWriteCommit` is that
  single commit.
- `argoCDApp` still observes one Application. For health gating and rollback,
  pick the Application that is slowest or riskiest to roll out.

Existing TagUpdaters that set only `writeBack.path` are unaffected.

### Health-gated rollback

After each commit it writes, the controller sets
`argocd.argoproj.io/refresh: normal` on `managingApp`, or on `argoCDApp` without
one, so ArgoCD reads the commit now and not when its cached branch heads expire.
That is its only write to an Application: it never syncs one or touches its
operation, and a failed patch costs only the wait.

When `rollback.enabled` is set, `argoCDApp` is a health reference. For
app-of-apps deployments, optional `managingApp` is the revision observer;
otherwise `argoCDApp` serves both roles. After pushing a tag update, the controller
records the manifest commit SHA and waits for the revision observer's
`status.sync.revision` (or an entry in `status.sync.revisions`) to equal it.
Only then does the health timeout start. The timeout defaults to 20m
and measures workload convergence after ArgoCD has observed the commit; git
polling latency is excluded.

A terminal Degraded result, or expiry of that timeout, writes the previous
tag's values back through a new surgical commit. A Degraded result is ignored
while `status.operationState.phase` is Running or Terminating. Failed tags are
kept in `status.skippedTags` so they are not immediately selected again.

## Tag format convention

```
$service.$branch.build-$n.$sha6
```

Example: `platform.main.build-42.abc1234`

Push this tag from CI after a successful build. The controller matches it, extracts `branch=main`, `n=42`, `sha=abc1234`, and renders the template.

## Sources

| Type | Status | Notes |
|------|--------|-------|
| `git` | Implemented | Uses `git ls-remote --tags` |
| `oci` | Stub | Implement with `distribution/v3` or `google/go-containerregistry` |

## Architecture

```
internal/
  matcher/    — named-group regex matching + "n"-sorted Latest()
  source/     — Source interface; git and oci implementations
  writeback/  — surgical YAML editing and git commit/push flow
  controller/ — tag resolution and write-back reconciliation
api/v1alpha1/ — TagUpdater CRD types
```

## Failure detection

The controller's historical failure mode is *silent*: the reconcile loop
breaks, nothing errors loudly, and deploys freeze until a human notices.
Two layers detect this:

1. **Per-updater reconcile-progress staleness.** Each TagUpdater's last
   successful reconcile (resolve+write-back pipeline completed, whether or not a new
   tag existed) is tracked in memory. An updater with no success within
   `max(multiplier × interval, floor)` — flags `--reconcile-stale-multiplier`
   (default `10`) and `--reconcile-stale-floor` (default `15m`) — is *stale*:
   - metric `tagupdater_reconcile_stale{updater="<ns>/<name>"}` reports `1`
     (computed at scrape time, so it stays accurate even if the loop is wedged);
   - the CR gets a `Stalled=True` condition (reason `ReconcileStale`).

2. **Aggregate liveness (`/healthz` check `reconcile-progress`).** Unhealthy
   only when *every* tracked TagUpdater is stale — a systemic freeze — so the
   pod restarts and leadership migrates. A single stale updater (one broken
   repo) never restarts the shared controller; use layer 1 to alert on it.
   The `tag-resolution` healthz check similarly restarts the pod when git
   tag→rev resolution has been failing past its staleness window.

Suggested alert: `tagupdater_reconcile_stale == 1`.

## Installation

```sh
kubectl apply -f config/crd/updater.argocd.io_tagupdaters.yaml
# helm chart coming soon
```

### Chart: self-updating controller

The chart in `charts/argocd-tag-updater` can render the controller's own TagUpdater. It is off by default. Values under `selfUpdate`:

| Value | Default | Meaning |
|---|---|---|
| `selfUpdate.enabled` | `false` | Render the self-update TagUpdater. |
| `selfUpdate.interval` | `15s` | Poll interval for the tag stream (Go duration). |
| `selfUpdate.labels` | `{}` | Extra `metadata.labels` on the TagUpdater, added to the chart's standard labels. Use for cluster tooling that selects TagUpdaters by label, such as `foundry.io/source-repo`. |
| `selfUpdate.argoCDAppName` | chart fullname | Application to patch. |
| `selfUpdate.managingApp` | `cluster-apps` in `argocd` | App that owns the Application. |
| `selfUpdate.source.repo` | required | Git tag stream to watch. |
| `selfUpdate.source.tagPattern` | release tag regex | Tag pattern with `branch`, `n`, `sha` captures. |
| `selfUpdate.flakeRefTemplate` | required | Template for `nixMount.flakeRef`. |
| `selfUpdate.writeBack` | `{}` | Git write-back target (`repo`, `branch`, `path`, `credentialsSecretRef`, `commitAuthor`). |

### Opt-in immutable image digests

`source.resolveDigest: true` enables exact-tag manifest HEAD resolution for **Git release sources only**, exposing `{{ .image_digest }}`. It defaults to false. `source.digestRepo` identifies the registry repository whose tag matches the selected Git tag. CI must publish Git tags only after image signing and attestation; registry tag listing alone is not a release gate. Opt-in OCI/Nix sources are rejected, preserving their legacy tag behavior without introducing credential-bearing pagination paths.

```yaml
source:
  type: git
  repo: ssh://git@forgejo.example/org/app.git
  tagPattern: '^app\.(?P<branch>main)\.(?P<n>\d{14})\.(?P<sha>[0-9a-f]{6})$'
  resolveDigest: true
  digestRepo: https://registry.example/org/app
  digestAuthRealm: https://registry.example/service/token
  imagePullSecretRef:
    name: registry-reader
targets:
  - apiVersion: argoproj.io/v1alpha1
    kind: Application
    name: app
    namespace: argocd
    patches:
      - field: spec.sources.0.helm.valuesObject.image.tag
        template: '{{ .tag }}'
      - field: spec.sources.0.helm.valuesObject.image.digest
        template: '{{ .image_digest }}'
```

Dockerconfigjson credentials reuse the existing Basic mechanism. A Bearer challenge can exchange them for a bounded token at the exact repository `pull` scope; anonymous exchanges are permitted when no secret is configured. Token responses are limited to 64KiB. A cross-origin realm must use HTTPS and match `digestAuthRealm` exactly; same-origin HTTP/HTTPS realms are trusted only when no explicit realm is configured. An explicit realm must always match exactly. Manifest and token redirects are rejected. Explicit credential errors, scope/realm mismatches, registry errors and missing/invalid lowercase SHA256 digest headers stop write-back. OCI and Docker manifest/index media types are requested. Private projects still require a reader credential: receiving an anonymous token does not grant pull permission.

A recorded release tag resolving to a different digest fails closed with `RegistryDigestChanged`; it does not silently replace the known-good pin, even after unrelated configuration changes. Publish a new gated release tag to promote changed content.

Successful writes retain `status.lastImageDigest` and rotate the old tag's digest to `status.previousImageDigest`. Rollback restores that recorded digest without trusting the tag's current registry binding; missing prior digest history blocks rollback. `status.observedGeneration` and the resolved digest gate the unchanged-head optimization, so enabling digest resolution or changing desired patch fields at an existing tag cannot silently skip write-back.

### Opt-in signature gate

`source.requireSigned: marker` withholds a new tag until its image is signed. The newest matching tag is resolved to its manifest digest (one HEAD), and it is written only when the same repository also holds the release pipeline's signed marker: the digest with `:` replaced by `-`, plus `.signed` (`sha256:abcd…` → `sha256-abcd….signed`). The pipeline pushes that tag only after `cosign sign` succeeds.

```yaml
source:
  type: oci
  repo: http://harbor.oci-registry.svc.cluster.local/foundry/app
  tagPattern: '^app\.(?P<branch>main)\.(?P<n>\d{14})\.(?P<sha>[0-9a-f]{6})$'
  requireSigned: marker
  # Harbor answers the internal host with a token challenge on its external
  # hostname; a cross-origin realm is followed only when named here.
  digestAuthRealm: https://registry.example/service/token
  imagePullSecretRef:
    name: harbor-pull-secret
targets:
  - # ...
    patches:
      - field: spec.template.spec.containers.0.image
        template: 'harbor.oci-registry.svc.cluster.local/foundry/app:{{ .tag }}@{{ .signed_digest }}'
```

**Upgrade order:** roll out a controller that has this feature before setting `requireSigned` on any TagUpdater. An older controller ignores the field and keeps writing unsigned tags without any error. Once enabled, a `SignatureVerified` condition on the CR confirms the gate is active.

- **Unsigned:** nothing is written. `status.awaitingSignatureTag` names the held tag, the `SignatureVerified` and `Ready` conditions are False with reason `AwaitingSignature`, and one `AwaitingSignature` Warning event is recorded per held tag. The tag is not added to `skippedTags`; it is rechecked on the fast-retry schedule (see Requeue timing) and written once its marker appears.
- **Registry error** (anything but 200 or 404 on the tag or the marker): nothing is written, the reconcile fails as for any pre-write-back error (retried on the fast-retry schedule), `SignatureVerified` becomes Unknown with reason `SignatureCheckFailed`, and a `SignatureCheckFailed` Warning event is recorded.
- **Rollback** renders `{{ .signed_digest }}` from `status.previousSignedDigest`, the digest verified when that tag deployed, without asking the registry again. Written digests are kept in `status.lastSignedDigest` / `previousSignedDigest`. If a template uses `signed_digest` and no previous digest is recorded (the tag deployed before the gate was enabled), rollback fails closed with `RollbackWriteFailed`; restore that image by hand.
- **No fallback:** an older signed tag is never written in place of an unsigned newer one, so enabling the gate cannot roll a deployment back. A tag already in git that is unsigned is held there, reported as `AwaitingSignature`, until a newer signed tag arrives.
- **Repository:** `source.repo` for an OCI source; `source.digestRepo` for a git source (required). Nix sources are rejected. With `resolveDigest` the resolved `image_digest` must equal the verified digest.
- **Cost:** the candidate tag is resolved on every poll, since tags can move. A digest found signed is cached in memory; absence is never cached.
- Unset, the controller makes no extra registry requests and behaves as before.

The verified digest is exposed to templates as `{{ .signed_digest }}`. Writing `repo:{{ .tag }}@{{ .signed_digest }}` pins the image that was checked; a bare `{{ .tag }}` leaves a window in which the tag could be moved after the check. The check is a marker, not a cryptographic verification: it trusts whoever can push tags to the repository. Verifying the cosign signature against a public key Secret is a planned `cosign` mode.

CRD generation: `CGO_ENABLED=0 controller-gen crd paths=./api/... output:crd:artifacts:config=config/crd` and `CGO_ENABLED=0 controller-gen object paths=./api/...`; the Helm CRD mirrors the generated schema inside its install conditional. The optional real Harbor test uses `LIVE_HARBOR_DIGEST_TEST=1` and `LIVE_HARBOR_REPOSITORY`. `LIVE_HARBOR_PULL_SECRET=1` explicitly authorizes reading the existing argocd/harbor-pull-secret into test memory for the known registry release; no credentials or tokens are printed. Without that flag the release must be anonymously readable.
