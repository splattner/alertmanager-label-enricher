{{- define "alertmanager-label-enricher.name" -}}
{{- .Chart.Name -}}
{{- end -}}

{{- define "alertmanager-label-enricher.fullname" -}}
{{- if .Release.Name | eq .Chart.Name -}}
{{- .Chart.Name -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "alertmanager-label-enricher.labels" -}}
app.kubernetes.io/name: {{ include "alertmanager-label-enricher.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "alertmanager-label-enricher.selectorLabels" -}}
app.kubernetes.io/name: {{ include "alertmanager-label-enricher.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "alertmanager-label-enricher.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- .Values.serviceAccount.name | default (include "alertmanager-label-enricher.fullname" .) -}}
{{- else -}}
{{- .Values.serviceAccount.name | default "default" -}}
{{- end -}}
{{- end -}}
