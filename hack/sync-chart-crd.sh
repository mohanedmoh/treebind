#!/usr/bin/env bash
# Writes the chart's copy of the CRD from the CRD that controller-gen generates.
#
# Usage: hack/sync-chart-crd.sh <generated-crd> <chart-crd>
#
# The chart template is the generated CRD with four changes: the whole file is
# wrapped in the crd.enabled switch, the document marker goes, the crd.keep
# switch adds the helm.sh/resource-policy annotation, and every "{{" and "}}" of
# the CRD is written as a Helm string, as {{ "{{" }}, because Helm would take it
# for the start or the end of an action and drop what is between (the
# description of messageTemplate has {{.Count}}). Anything else that differs
# means the copy was edited by hand, which this script overwrites.
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
  # escape writes each "{{" and "}}" of a line as a Helm string, in one pass, so that the "}}" it writes is not escaped again.
  function escape(line,   out) {
    out = ""
    while (match(line, /[{][{]|[}][}]/)) {
      out = out substr(line, 1, RSTART - 1) "{{ \"" substr(line, RSTART, 2) "\" }}"
      line = substr(line, RSTART + 2)
    }
    return out line
  }
  NR == 1 { print "{{- if .Values.crd.enabled }}"; next }
  { print escape($0) }
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

# mktemp creates the file with mode 0600, and mv keeps it.
chmod 0644 "$tmp"
mv "$tmp" "$dst"
trap - EXIT
