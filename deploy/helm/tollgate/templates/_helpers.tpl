{{- define "tollgate.name" -}}
{{- .Chart.Name -}}
{{- end -}}

{{- define "tollgate.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "tollgate.labels" -}}
app.kubernetes.io/name: {{ include "tollgate.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "tollgate.selectorLabels" -}}
app.kubernetes.io/name: {{ include "tollgate.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
