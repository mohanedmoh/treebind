#!/usr/bin/env bash
# Writes the chart's copy of the operator's role from the role that controller-gen
# generates.
#
# Usage: hack/sync-chart-rbac.sh <generated-role> <chart-role>
#
# The chart template is the generated role with its header replaced, so that the
# kind, the namespace and the name follow the rbac.namespaced value and the release.
# The rules, from the "rules:" line down, are copied as they are. Anything else
# that differs means the copy was edited by hand, which this script overwrites.
# The generated file must hold one role: a second document, such as a Role that
# a namespaced RBAC marker adds, would need its own template, so it is an error.
set -euo pipefail

src="${1:?usage: sync-chart-rbac.sh <generated-role> <chart-role>}"
dst="${2:?usage: sync-chart-rbac.sh <generated-role> <chart-role>}"

tmp="$(mktemp "$dst.XXXXXX")"
trap 'rm -f "$tmp"' EXIT

{
  cat <<'EOF'
apiVersion: rbac.authorization.k8s.io/v1
{{- if .Values.rbac.namespaced }}
kind: Role
{{- else }}
kind: ClusterRole
{{- end }}
metadata:
{{- if .Values.rbac.namespaced }}
  namespace: {{ .Release.Namespace }}
{{- end }}
  name: {{ include "treebind.resourceName" (dict "suffix" "manager-role" "context" $) }}
EOF
  awk '
    $0 == "rules:" { found = 1 }
    found && $0 == "---" { exit 2 }
    found { print }
    END { if (!found) exit 1 }
  ' "$src"
} > "$tmp" || status=$?

case "${status:-0}" in
  0) ;;
  1) echo "found no rules: line in $src" >&2; exit 1 ;;
  2) echo "$src holds more than one document; the chart template follows rbac.namespaced for one role" >&2; exit 1 ;;
  *) exit "$status" ;;
esac

# mktemp creates the file with mode 0600, and mv keeps it.
chmod 0644 "$tmp"
mv "$tmp" "$dst"
trap - EXIT
