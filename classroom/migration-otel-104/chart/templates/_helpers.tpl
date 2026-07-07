{{/*
共用 labels
*/}}
{{- define "otelcr.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
app.kubernetes.io/name: {{ .Values.name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
對齊原 opentelemetry-collector chart 的 useGOMEMLIMIT:
取 resources.limits.memory 的 80%,輸出 MiB(只支援 Mi/Gi,其他格式直接 fail)
512Mi -> 409MiB, 1Gi -> 819MiB, 1.5Gi -> 1228MiB
*/}}
{{- define "otelcr.gomemlimit" -}}
{{- $mem := .Values.resources.limits.memory | toString -}}
{{- $mib := 0.0 -}}
{{- if hasSuffix "Gi" $mem -}}
{{- $mib = mulf (float64 (trimSuffix "Gi" $mem)) 1024 -}}
{{- else if hasSuffix "Mi" $mem -}}
{{- $mib = float64 (trimSuffix "Mi" $mem) -}}
{{- else -}}
{{- fail (printf "useGOMEMLIMIT: unsupported memory format %q (use Mi or Gi)" $mem) -}}
{{- end -}}
{{- printf "%dMiB" (int (floor (mulf $mib 0.8))) -}}
{{- end }}
