{{/*
The selector pair, and nothing else, ever.

Four things pin exactly these two labels: the Deployment's own
spec.selector.matchLabels, the Service's spec.selector, the NetworkPolicy's
spec.podSelector, and test/e2e's operatorPod. Helm's convention would add
app.kubernetes.io/instance here; it must not. A Deployment's selector is
immutable after creation, so a selector carrying the release name cannot be
corrected in place -- and the Service and NetworkPolicy would silently stop
matching the pod, which looks like a network fault rather than a label one.
*/}}
{{- define "spawnery.selectorLabels" -}}
app.kubernetes.io/name: spawnery
app.kubernetes.io/component: operator
{{- end }}

{{/*
Metadata labels: the selector pair plus what Helm expects to find on objects
it manages. Never used in a selector -- see above.
*/}}
{{- define "spawnery.labels" -}}
{{ include "spawnery.selectorLabels" . }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}

{{/*
The operator image. A digest beats a tag because a tag can move under a
running cluster; hack/publish.sh writes .Values.image.digest after a real
publish.
*/}}
{{- define "spawnery.image" -}}
{{- if .Values.image.digest -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ .Values.image.tag }}
{{- end -}}
{{- end }}
