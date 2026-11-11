#!/usr/bin/env bash
# Writes the chart's copy of the CRD from the CRD that controller-gen generates.
#
# Usage: hack/sync-chart-crd.sh <generated-crd> <chart-crd>
#
# The chart template is the generated CRD with three changes: the whole file is
# wrapped in the crd.enabled switch, the document marker goes, and the
# crd.keep switch adds the helm.sh/resource-policy annotation. Anything else
# that differs means the copy was edited by hand, which this script overwrites.
set -euo pipefail

src="${1:?usage: sync-chart-crd.sh <generated-crd> <chart-crd>}"
dst="${2:?usage: sync-chart-crd.sh <generated-crd> <chart-crd>}"

if [ "$(head -n 1 "$src")" != "---" ]; then
  echo "$src does not start with a '---' document marker; controller-gen output changed" >&2
  exit 1
fi

tmp="$(mktemp "$dst.XXXXXX")"
trap 'rm -f "$tmp"' EXIT

awk '
  NR == 1 { print "{{- if .Values.crd.enabled }}"; next }
  { print }
  $0 == "  annotations:" && !annotated {
    print "    {{- if .Values.crd.keep }}"
    print "    \"helm.sh/resource-policy\": keep"
    print "    {{- end }}"
    annotated = 1
  }
  END {
    print "{{- end }}"
    if (!annotated) exit 1
  }
' "$src" > "$tmp" || { echo "found no metadata.annotations in $src" >&2; exit 1; }

mv "$tmp" "$dst"
trap - EXIT
