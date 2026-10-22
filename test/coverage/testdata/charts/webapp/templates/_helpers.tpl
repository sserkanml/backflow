{{- define "webapp.name" -}}
{{- .Release.Name }}-webapp
{{- end }}
{{- define "webapp.labels" -}}
app.kubernetes.io/name: webapp
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Values.image.tag | quote }}
{{- end }}
{{- define "webapp.selector" -}}
app.kubernetes.io/name: webapp
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
