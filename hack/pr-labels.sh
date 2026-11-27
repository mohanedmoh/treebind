#!/usr/bin/env bash
# Prints the labels that a pull request gets from its title, one per line, so that the generated release notes
# sort themselves (.github/release.yml). A title that is not a Conventional Commits title gets none.
#
# Usage: hack/pr-labels.sh TITLE
set -euo pipefail

title=${1?usage: pr-labels.sh TITLE}

pattern='^([a-z]+)(\([a-z]+\))?(!?): '
[[ $title =~ $pattern ]] || exit 0

case ${BASH_REMATCH[1]} in
  feat) echo kind/feature ;;
  fix) echo kind/bug ;;
  docs) echo area/docs ;;
  ci) echo area/ci ;;
esac

if [[ -n ${BASH_REMATCH[3]} ]]; then
  echo breaking-change
fi
