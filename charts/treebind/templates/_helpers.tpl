{{/*
Expand the name of the chart.
*/}}
{{- define "treebind.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "treebind.fullname" -}}
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
Namespace for generated references.
Always uses the Helm release namespace.
*/}}
{{- define "treebind.namespaceName" -}}
{{- .Release.Namespace }}
{{- end }}

{{/*
Resource name with proper truncation for Kubernetes 63-character limit.
Takes a dict with:
  - .suffix: Resource name suffix (e.g., "metrics", "webhook")
  - .context: Template context (root context with .Values, .Release, etc.)
Dynamically calculates safe truncation to ensure total name length <= 63 chars.
*/}}
{{- define "treebind.resourceName" -}}
{{- $fullname := include "treebind.fullname" .context }}
{{- $suffix := .suffix }}
{{- $maxLen := sub 62 (len $suffix) | int }}
{{- if gt (len $fullname) $maxLen }}
{{- printf "%s-%s" (trunc $maxLen $fullname | trimSuffix "-") $suffix | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" $fullname $suffix | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
ServiceAccount name to use.
When enabled, use the chart's ServiceAccount name.
When disabled, serviceAccount.name must be set; use "default" to pick the namespace default ServiceAccount.
*/}}
{{- define "treebind.serviceAccountName" -}}
{{- if .Values.serviceAccount.enabled }}
{{- include "treebind.resourceName" (dict "suffix" "controller-manager" "context" .) }}
{{- else }}
{{- required "serviceAccount.name is required when serviceAccount.enabled=false (set name: default explicitly to use the namespace default ServiceAccount)" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
The namespaces that the operator serves, separated by commas. None when the roles are
cluster-wide (rbac.namespaced is false), the release namespace when rbac.namespaced is
set and rbac.namespaces is empty, and rbac.namespaces otherwise. The roles of the chart
and the --watch-namespaces argument of the operator both come from this, so that the
operator cannot watch a namespace that it has no Role in.

Every template that reads the namespaces reads them through this, and this is the only
place that checks them. It fails the install for a list without rbac.namespaced, because
a list that was ignored would leave a ClusterRole where Roles were asked for, and for an
entry that is not the name of a namespace (what the operator accepts: a DNS label of at
most 63 characters, with no blank around it) and for a namespace that comes twice, and
the message names the entry.
*/}}
{{- define "treebind.watchNamespaces" -}}
{{- if and .Values.rbac.namespaces (not .Values.rbac.namespaced) }}
{{- fail "rbac.namespaces needs rbac.namespaced: true. Without it the operator gets a ClusterRole for every namespace, which is not what a list of namespaces asks for." }}
{{- end }}
{{- if .Values.rbac.namespaced }}
{{- $namespaces := .Values.rbac.namespaces | default (list .Release.Namespace) }}
{{- $seen := dict }}
{{- range $namespaces }}
{{- $name := toString . }}
{{- if eq $name "" }}
{{- fail "rbac.namespaces has an empty entry. Each entry is the name of a namespace." }}
{{- end }}
{{- if or (gt (len $name) 63) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $name)) }}
{{- fail (printf "rbac.namespaces has %q, which is not the name of a namespace: lowercase letters, digits and dashes, starting and ending with a letter or a digit, at most 63 characters, and no blanks." $name) }}
{{- end }}
{{- if hasKey $seen $name }}
{{- fail (printf "rbac.namespaces lists %q twice." $name) }}
{{- end }}
{{- $_ := set $seen $name true }}
{{- end }}
{{- join "," $namespaces }}
{{- end }}
{{- end }}

{{/*
The argument that tells the operator which namespaces to serve, and nothing when the
roles are cluster-wide, so that a --watch-namespaces of the user in manager.args is
used as it was. The Deployment puts it after manager.args: a flag that comes twice takes
the last value, and the namespaces of the Roles have to win.
*/}}
{{- define "treebind.watchNamespacesArg" -}}
{{- with (include "treebind.watchNamespaces" .) }}--watch-namespaces={{ . }}{{- end }}
{{- end }}
