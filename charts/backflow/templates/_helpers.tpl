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
Where the injected CA bundles are mounted in the pod.
*/}}
{{- define "backflow.serviceCAFile" -}}/var/run/argocd-ca/service-ca.crt{{- end }}
{{- define "backflow.trustedCAFile" -}}/var/run/trusted-ca/ca-bundle.crt{{- end }}

{{/*
Stops the install when the Argo CD values do not add up.
*/}}
{{- define "backflow.validate" -}}
{{- if .Values.argocd.server }}
{{- if not (and .Values.argocd.tokenSecret.namespace .Values.argocd.tokenSecret.name) }}
{{- fail "argocd.server needs argocd.tokenSecret.namespace and argocd.tokenSecret.name" }}
{{- end }}
{{- else if or .Values.argocd.tokenSecret.namespace .Values.argocd.tokenSecret.name .Values.argocd.caFile .Values.argocd.serviceCA }}
{{- fail "argocd.tokenSecret, argocd.caFile and argocd.serviceCA need argocd.server" }}
{{- end }}
{{- if and .Values.argocd.serviceCA .Values.argocd.caFile }}
{{- fail "set argocd.serviceCA or argocd.caFile, not both" }}
{{- end }}
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

{{/*
The label selector of the operator's Deployment, as `manager migrate --operator-selector` takes it.
*/}}
{{- define "backflow.operatorSelector" -}}
{{- $parts := list }}
{{- range $k, $v := (fromYaml (include "backflow.selectorLabels" .)) }}{{ $parts = append $parts (printf "%s=%s" $k $v) }}{{ end }}
{{- sortAlpha $parts | join "," }}
{{- end }}

{{/*
The actor webhook: its name, TLS mode, path and certificate.
*/}}
{{- define "backflow.actorName" -}}
{{- printf "%s-actor-webhook" (include "backflow.managerName" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "backflow.actorTLSMode" -}}
{{- $mode := .Values.actorWebhook.tls.mode | default "auto" -}}
{{- if eq $mode "auto" -}}
{{- if .Capabilities.APIVersions.Has "route.openshift.io/v1" -}}serviceCA
{{- else if .Capabilities.APIVersions.Has "cert-manager.io/v1" -}}certManager
{{- else -}}helm{{- end -}}
{{- else -}}{{ $mode }}{{- end -}}
{{- end }}

{{- define "backflow.actorIsOpenShift" -}}
{{- if .Capabilities.APIVersions.Has "route.openshift.io/v1" -}}true{{- end -}}
{{- end }}

{{/*
The URL path of the webhook: the value, or the one kept in a Secret, or a new random one. A
random one is made once per render (the value is cached in .Values, which every template shares).
*/}}
{{- define "backflow.actorPath" -}}
{{- if .Values.actorWebhook.path -}}{{ .Values.actorWebhook.path }}
{{- else -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (printf "%s-path" (include "backflow.actorName" .)) -}}
{{- if and $existing $existing.data (index $existing.data "path") -}}{{ index $existing.data "path" | b64dec }}
{{- else -}}
{{- if not (hasKey .Values "_actorPath") }}{{- $_ := set .Values "_actorPath" (printf "/observe/%s" (randAlphaNum 32)) }}{{- end -}}
{{- .Values._actorPath -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
The certificate made by Helm (mode helm), as JSON {ca, crt, key}: the one in the Secret from an
earlier install unless tls.renew, else a new one, made once per render.
*/}}
{{- define "backflow.actorCert" -}}
{{- $name := include "backflow.actorName" . -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (printf "%s-tls" $name) -}}
{{- if and $existing $existing.data (index $existing.data "ca.crt") (not .Values.actorWebhook.tls.renew) -}}
{{- dict "ca" (index $existing.data "ca.crt" | b64dec) "crt" (index $existing.data "tls.crt" | b64dec) "key" (index $existing.data "tls.key" | b64dec) | toJson -}}
{{- else -}}
{{- if not (hasKey .Values "_actorCert") -}}
{{- $cn := printf "%s.%s.svc" $name .Release.Namespace -}}
{{- $days := int .Values.actorWebhook.tls.days -}}
{{- $ca := genCA (printf "%s-ca" $name) $days -}}
{{- $cert := genSignedCert $cn nil (list $cn (printf "%s.cluster.local" $cn)) $days $ca -}}
{{- $_ := set .Values "_actorCert" (dict "ca" $ca.Cert "crt" $cert.Cert "key" $cert.Key) -}}
{{- end -}}
{{- .Values._actorCert | toJson -}}
{{- end -}}
{{- end }}

{{/*
The CA bundle for the webhook configuration (base64), when the mode gives it here.
*/}}
{{- define "backflow.actorCABundle" -}}
{{- $mode := include "backflow.actorTLSMode" . -}}
{{- if eq $mode "helm" -}}{{ (include "backflow.actorCert" . | fromJson).ca | b64enc }}
{{- else if eq $mode "secret" -}}{{ .Values.actorWebhook.tls.caBundle | b64enc }}
{{- end -}}
{{- end }}

{{/*
The name of the Secret with the serving certificate.
*/}}
{{- define "backflow.actorTLSSecret" -}}
{{- if eq (include "backflow.actorTLSMode" .) "secret" -}}{{ .Values.actorWebhook.tls.secretName }}{{- else -}}{{ include "backflow.actorName" . }}-tls{{- end -}}
{{- end }}

{{/*
Stops the install when the actor webhook values do not add up.
*/}}
{{- define "backflow.actorValidate" -}}
{{- if .Values.actorWebhook.enabled -}}
{{- if and (semverCompare "<1.28.0-0" .Capabilities.KubeVersion.Version) (not .Values.actorWebhook.allowUnfiltered) -}}
{{- fail "actorWebhook needs Kubernetes 1.28 or later (matchConditions); set actorWebhook.allowUnfiltered=true to send every update to it anyway" -}}
{{- end -}}
{{- if not (has (int .Values.actorWebhook.timeoutSeconds) (list 1 2)) -}}
{{- fail "actorWebhook.timeoutSeconds must be 1 or 2" -}}
{{- end -}}
{{- $mode := include "backflow.actorTLSMode" . -}}
{{- if not (has $mode (list "serviceCA" "certManager" "helm" "secret")) -}}
{{- fail "actorWebhook.tls.mode must be auto, serviceCA, certManager, helm or secret" -}}
{{- end -}}
{{- if and (eq $mode "secret") (not (and .Values.actorWebhook.tls.secretName .Values.actorWebhook.tls.caBundle)) -}}
{{- fail "actorWebhook.tls.mode secret needs tls.secretName and tls.caBundle" -}}
{{- end -}}
{{- if and .Values.actorWebhook.networkPolicy.enabled (not (include "backflow.actorIsOpenShift" .)) (not .Values.actorWebhook.networkPolicy.apiserverCIDRs) -}}
{{- fail "actorWebhook.networkPolicy needs actorWebhook.networkPolicy.apiserverCIDRs (the addresses of the API servers) outside OpenShift; set networkPolicy.enabled=false to go without" -}}
{{- end -}}
{{- end -}}
{{- end }}
