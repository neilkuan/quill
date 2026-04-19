{{- define "quill.name" -}}
{{ .Chart.Name }}
{{- end }}

{{- define "quill.fullname" -}}
{{ .Release.Name }}-{{ .Chart.Name }}
{{- end }}

{{- define "quill.labels" -}}
app.kubernetes.io/name: {{ include "quill.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "quill.selectorLabels" -}}
app.kubernetes.io/name: {{ include "quill.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
