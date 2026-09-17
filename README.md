# argocd-tag-updater

A generic Kubernetes controller that watches a tag source, renders Go templates with named captures, and commits the resulting field values into Kubernetes manifests in git for ArgoCD to apply.

Inspired by [ArgoCD Image Updater](https://github.com/argoproj-labs/argocd-image-updater), but decoupled from OCI registries and generalised to any CR field.

## How it works

1. A `TagUpdater` CR declares a source, a tag pattern (named-group regex), a target CR + field, and a Go template.
2. The controller polls the source for tags on a configurable interval.
3. When a new tag matches the pattern, named captures are extracted and passed to the template.
4. The rendered value is written surgically into the configured manifest.
5. The controller commits and pushes the manifest; ArgoCD converges from git.

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
    credentialsSecretRef:
      name: manifest-git-credentials
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

The Secret must be in the TagUpdater namespace and contain `token`, `username`
plus `password`, or `sshPrivateKey`. SSH credentials must also contain a
`knownHosts` entry; `insecureIgnoreHostKey: "true"` is available only as an
explicit opt-in. Git tag-source SSH uses `GIT_KNOWN_HOSTS_FILE`, or the explicit
`GIT_INSECURE_IGNORE_HOST_KEY=true` fallback. The manifest may contain
multiple YAML documents; each target is selected by apiVersion, kind,
name/namespace or label selector. Only configured scalar field tokens are
replaced, preserving comments and unrelated formatting. No commit is created
when every rendered value is already present.

### Health-gated rollback

When `rollback.enabled` is set, `argoCDApp` is a read-only health reference:
the controller never patches or sync-triggers it. For app-of-apps deployments,
optional `managingApp` is the read-only revision observer; otherwise
`argoCDApp` serves both roles. After pushing a tag update, the controller
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
