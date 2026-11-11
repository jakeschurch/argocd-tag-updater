{{/* 2026-11-11: CRD admission and controller/RBAC share gateKinds. */}}
{{- define "argocd-tag-updater.gateAllowlistRule" -}}
{{- $presets := dict "argoproj.io/v1alpha1/Application" "argocd-application" "argoproj.io/v1alpha1/Rollout" "argo-rollout" "argoproj.io/v1alpha1/AnalysisRun" "analysisrun" "apps/v1/Deployment" "deployment" "apps/v1/StatefulSet" "statefulset" "kargo.akuity.io/v1alpha1/Promotion" "kargo-promotion" -}}
{{- $names := list -}}
{{- $kinds := list -}}
{{- range .Values.gateKinds -}}
{{- $key := printf "%s/%s" .apiVersion .kind -}}
{{- $kinds = append $kinds (printf "'%s'" $key) -}}
{{- if hasKey $presets $key -}}
{{- $names = append $names (printf "'%s'" (get $presets $key)) -}}
{{- end -}}
{{- end -}}
has(self.preset) ? self.preset in [{{ join ", " $names }}] : (has(self.apiVersion) && has(self.kind) && (self.apiVersion + '/' + self.kind) in [{{ join ", " $kinds }}])
{{- end -}}
