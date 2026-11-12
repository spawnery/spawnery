{{/*
The selector pair, and nothing else, ever.

Several things pin exactly these two labels: the Deployment's own
spec.selector.matchLabels and its topology spread constraint, the
spawnery-operator-metrics Service's spec.selector, the PodDisruptionBudget's
and the NetworkPolicy's selectors, and test/e2e's operatorPods. Helm's
convention would add app.kubernetes.io/instance here; it must not. A
Deployment's selector is immutable after creation, so a selector carrying the
release name cannot be corrected in place -- and the metrics Service and the
NetworkPolicy would silently stop matching the pod, which looks like a network
fault rather than a label one.
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
{{ include "spawnery.commonLabels" . }}
{{- end }}

{{- define "spawnery.commonLabels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}

{{/*
Metadata labels of the world sync objects. They leave out the operator's
selector pair, so nothing that selects the operator matches them.
*/}}
{{- define "spawnery.worldSyncLabels" -}}
app.kubernetes.io/name: spawnery
app.kubernetes.io/component: worldsync
{{ include "spawnery.commonLabels" . }}
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

{{/*
The object store settings of the operator and of the node agent. Both render
from here: they must agree on bucket and prefix or the operator deletes and
sweeps a different place than the nodes write to.
*/}}
{{- define "spawnery.worldSyncEnv" -}}
{{- $s := .Values.worldSync.objectStore -}}
- name: WORLDSYNC_ENDPOINT
  value: {{ required "worldSync.objectStore.endpoint is required" $s.endpoint | quote }}
- name: WORLDSYNC_REGION
  value: {{ required "worldSync.objectStore.region is required" $s.region | quote }}
- name: WORLDSYNC_BUCKET
  value: {{ required "worldSync.objectStore.bucket is required" $s.bucket | quote }}
- name: WORLDSYNC_PREFIX
  value: {{ $s.prefix | quote }}
- name: AWS_ACCESS_KEY_ID
  valueFrom:
    secretKeyRef:
      name: {{ required "worldSync.objectStore.credentialsSecret is required" $s.credentialsSecret }}
      key: AWS_ACCESS_KEY_ID
- name: AWS_SECRET_ACCESS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ $s.credentialsSecret }}
      key: AWS_SECRET_ACCESS_KEY
{{- end }}

{{- define "spawnery.worldSyncImage" -}}
{{- $i := .Values.worldSync.image -}}
{{- if $i.digest -}}
{{ $i.repository }}@{{ $i.digest }}
{{- else -}}
{{ $i.repository }}:{{ $i.tag | default .Chart.AppVersion }}
{{- end -}}
{{- end }}
