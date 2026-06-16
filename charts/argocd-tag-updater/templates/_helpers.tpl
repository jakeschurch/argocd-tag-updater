{{/* Chart name. */}}
{{- define "argocd-tag-updater.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully qualified app name. */}}
{{- define "argocd-tag-updater.fullname" -}}
{{- default .Chart.Name .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Common labels. */}}
{{- define "argocd-tag-updater.labels" -}}
app.kubernetes.io/name: {{ include "argocd-tag-updater.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{/* Selector labels. */}}
{{- define "argocd-tag-updater.selectorLabels" -}}
app.kubernetes.io/name: {{ include "argocd-tag-updater.name" . }}
{{- end -}}

{{/* ServiceAccount name. */}}
{{- define "argocd-tag-updater.serviceAccountName" -}}
{{- default (include "argocd-tag-updater.fullname" .) .Values.serviceAccount.name -}}
{{- end -}}
