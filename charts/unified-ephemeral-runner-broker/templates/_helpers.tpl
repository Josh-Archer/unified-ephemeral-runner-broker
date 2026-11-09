{{- define "uecb.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "uecb.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- include "uecb.name" . -}}
{{- end -}}
{{- end -}}

{{- define "uecb.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "uecb.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Per-backend allocation latency and failure SLO rules.
Thresholds come from .Values.observability.slo and match docs/observability.md.
*/}}
{{- define "uecb.sloRules" -}}
groups:
  - name: uecb-backend-slo
    rules:
      - alert: UECBBackendAllocationLatencyHigh
        expr: histogram_quantile({{ .Values.observability.slo.latency.quantile }}, sum by (le, pool, backend) (rate(uecb_allocation_latency_seconds_bucket{result="success"}[{{ .Values.observability.slo.latency.window }}]))) > {{ .Values.observability.slo.latency.seconds }}
        for: {{ .Values.observability.slo.latency.for }}
        labels:
          severity: warning
          slo: allocation-latency
        annotations:
          summary: 'UECB allocation latency SLO breached for {{ `{{ $labels.pool }}` }}/{{ `{{ $labels.backend }}` }}'
          description: 'The p95 successful allocation latency for this backend has been above {{ .Values.observability.slo.latency.seconds }} seconds for {{ .Values.observability.slo.latency.for }}.'
      - alert: UECBBackendAllocationFailureRateHigh
        expr: sum by (pool, backend) (rate(uecb_allocations_total{result="failure"}[{{ .Values.observability.slo.failure.window }}])) / clamp_min(sum by (pool, backend) (rate(uecb_allocations_total[{{ .Values.observability.slo.failure.window }}])), 0.001) > {{ .Values.observability.slo.failure.ratio }}
        for: {{ .Values.observability.slo.failure.for }}
        labels:
          severity: critical
          slo: allocation-failure
        annotations:
          summary: 'UECB allocation failure SLO breached for {{ `{{ $labels.pool }}` }}/{{ `{{ $labels.backend }}` }}'
          description: 'The allocation failure ratio for this backend is above {{ .Values.observability.slo.failure.ratio }} over {{ .Values.observability.slo.failure.window }}.'
{{- end -}}

