{{/*
Expand the name of the chart.
*/}}
{{- define "backflow.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
A fully qualified app name, at most 63 characters (DNS naming spec).
*/}}
{{- define "backflow.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
The name of the Deployment, the ServiceAccount and the roles.
*/}}
{{- define "backflow.managerName" -}}
{{- printf "%s-controller-manager" (include "backflow.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Chart name and version, as used by the chart label.
*/}}
{{- define "backflow.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "backflow.selectorLabels" -}}
app.kubernetes.io/name: {{ include "backflow.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
control-plane: controller-manager
{{- end }}

{{/*
Common labels.
*/}}
{{- define "backflow.labels" -}}
helm.sh/chart: {{ include "backflow.chart" . }}
{{ include "backflow.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
The name of the ServiceAccount.
*/}}
{{- define "backflow.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "backflow.managerName" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
The image of the operator.
*/}}
{{- define "backflow.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}
