# argocd-tag-updater

A generic Kubernetes controller that watches a source (git tags or OCI registry) for new tags matching a pattern, renders a Go template with named captures, and patches any field on any CR — then triggers an ArgoCD sync.

Inspired by [ArgoCD Image Updater](https://github.com/argoproj-labs/argocd-image-updater), but decoupled from OCI registries and generalised to any CR field.

## How it works

1. A `TagUpdater` CR declares a source, a tag pattern (named-group regex), a target CR + field, and a Go template.
2. The controller polls the source for tags on a configurable interval.
3. When a new tag matches the pattern, named captures are extracted and passed to the template.
4. The rendered value is JSON-patched onto the target CR field.
5. Optionally, an ArgoCD Application is synced immediately after.

## Example use case — Nix flake refs

Push a git tag from CI:

```
platform.main.build-42.abc1234
```

A `TagUpdater` matches it and patches `spec.flakeRef` on a NixMount:

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
  target:
    apiVersion: nix.csi.k8s.io/v1alpha1
    kind: NixMount
    name: example-flake-platform
    namespace: example-flake
    field: spec.flakeRef                        # dot-notation path on the target CR
  template: 'github:your-org/your-flake/{{ .sha }}#packages.x86_64-linux.platform'
  interval: 2m
  argoCDApp:
    name: example-flake
    namespace: argocd
```

### Tag pattern

`tagPattern` is a Go named-group regex. Captures are available in `template` as `{{ .captureName }}`. The capture named `n` is used as the sort key to select the latest tag (parsed as an integer, falls back to lexicographic).

`{{ .tag }}` is always available as the full matched tag string.

### Template

Standard Go `text/template` rendered with all named captures as a flat `map[string]string`. The rendered string is written verbatim to the target field.

### Target field

`field` uses dot-notation (`spec.flakeRef`, `spec.image.tag`) which is converted to a JSON Pointer for patching. The naive plural rule (`kind + "s"`) is used for the resource name — extend with a REST mapper for non-standard plurals.

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
  patcher/    — dot-notation → JSON Patch on any CR via dynamic client
  controller/ — reconciler loop + ArgoCD sync trigger
api/v1alpha1/ — TagUpdater CRD types
```

## Failure detection

The controller's historical failure mode is *silent*: the reconcile loop (or
ArgoCD convergence) breaks, nothing errors loudly, and deploys freeze until a
human notices. Three layers detect this:

1. **Per-updater reconcile-progress staleness.** Each TagUpdater's last
   successful reconcile (resolve+patch pipeline completed, whether or not a new
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

3. **Target-Application error guard.** During reconcile the controller inspects
   the target ArgoCD Application's conditions; any `*Error` condition
   (`ComparisonError` after a CRD/manifest schema skew, `InvalidSpecError`, …)
   means ArgoCD is not converging even though patching succeeds. It emits an
   error-level log and increments
   `tagupdater_target_app_error{app=..., type=...}` — best-effort, never fails
   the reconcile.

Suggested alerts: `tagupdater_reconcile_stale == 1` and
`rate(tagupdater_target_app_error[15m]) > 0`.

## Installation

```sh
kubectl apply -f config/crd/updater.argocd.io_tagupdaters.yaml
# helm chart coming soon
```
