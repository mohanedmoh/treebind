/*
Copyright The Treebind Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/jsonpath"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
	"github.com/mohanedmoh/treebind/internal/agent/app"
)

// The kubectl commands of docs/recovering-from-a-conflict-or-a-rejected-push.md read the fields of a Treebind, so they are
// evaluated here, with the library that kubectl evaluates `-o jsonpath` with, over a Treebind that is made of the Go types and
// whose conditions are made by the code that makes them, and what they print is held to what the page says. There is no cluster
// here: that the commands are what kubectl accepts is the library's, and that the paths are the paths of the API is the types'.

const (
	recoveryPage = "recovering-from-a-conflict-or-a-rejected-push.md"

	refusedAt = "2026-11-20T09:15:00Z"
)

// pageText returns the page, which is two levels above the package.
func pageText(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", recoveryPage))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// fenced returns what follows a marker, in the fenced block that comes next, which has the language when it is not empty.
func fenced(t *testing.T, kind, name, language string) string {
	t.Helper()
	marker := "<!-- " + kind + ": " + name + " -->\n```" + language + "\n"
	_, rest, found := strings.Cut(pageText(t), marker)
	if !found {
		t.Fatalf("docs/%s has no %s marked %q", recoveryPage, kind, name)
	}
	block, _, found := strings.Cut(rest, "\n```")
	if !found {
		t.Fatalf("the %s %q of docs/%s is not closed", kind, name, recoveryPage)
	}
	return block + "\n"
}

var jsonpathArgument = regexp.MustCompile(`-o jsonpath='([^']*)'`)

// expressions are the jsonpath expressions of the kubectl commands of a tested block, in the order they are written.
func expressions(t *testing.T, name string) []string {
	t.Helper()
	var found []string
	for line := range strings.Lines(fenced(t, "tested", name, "sh")) {
		if match := jsonpathArgument.FindStringSubmatch(line); match != nil {
			found = append(found, match[1])
		}
	}
	if len(found) == 0 {
		t.Fatalf("the block %q has no kubectl command with a jsonpath", name)
	}
	return found
}

// evaluate prints the object as `kubectl get -o jsonpath` does: the object as JSON, read back as the generic value that the
// library gets, and a template that is told to leave out what is not there.
func evaluate(t *testing.T, expression string, object any) string {
	t.Helper()
	data, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	var generic any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatal(err)
	}
	printer := jsonpath.New("page").AllowMissingKeys(true)
	if err := printer.Parse(expression); err != nil {
		t.Fatalf("kubectl would not accept %q: %v", expression, err)
	}
	var out bytes.Buffer
	if err := printer.Execute(&out, generic); err != nil {
		t.Fatalf("%q does not run on a Treebind: %v", expression, err)
	}
	return out.String()
}

// listOf is what kubectl gets for a list: a List of the objects.
func listOf(items ...*treebindv1alpha1.Treebind) map[string]any {
	return map[string]any{"apiVersion": "v1", "kind": "List", "items": items}
}

// refusedPush is a Treebind whose last push the host refused, with the conditions that the code makes for it, in the order
// that the page shows them: the reconcile writes SpecSupported and Ready, and the poller Synced, Conflict and AgentUnreachable.
func refusedPush(t *testing.T) *treebindv1alpha1.Treebind {
	t.Helper()
	treebind := syncTreebind(bidirectional, treebindv1alpha1.TreebindStatus{})
	treebind.Spec.Sync.Interval = &metav1.Duration{Duration: 30 * time.Second}
	synced := syncStatusOf(treebind, syncReport(bidirectional, commitOne, failedCheck(app.CheckPush, reasonPushRejected)),
		metav1.ConditionFalse, 0, hashOfSpec)
	if synced.synced == nil || synced.synced.Reason != reasonPushRejected {
		t.Fatalf("Synced = %+v, want it False with PushRejected", synced.synced)
	}
	changed := metav1.NewTime(time.Date(2026, 11, 20, 9, 15, 0, 0, time.UTC))
	synced.synced.LastTransitionTime, synced.conflict.LastTransitionTime = changed, changed

	ready := readyCondition(&appsv1.StatefulSet{Status: appsv1.StatefulSetStatus{ReadyReplicas: 1}}, synced.synced,
		bidirectional, syncGeneration)
	for _, condition := range []metav1.Condition{specSupportedCondition(nil, syncGeneration), ready} {
		condition.LastTransitionTime = changed
		meta.SetStatusCondition(&treebind.Status.Conditions, condition)
	}
	synced.apply(&treebind.Status)
	reachable := agentUnreachableCondition(treebind, pollOutcome{}, nil, 0)
	reachable.LastTransitionTime = changed
	meta.SetStatusCondition(&treebind.Status.Conditions, reachable)
	treebind.Status.LastSyncedCommit = commitOne
	agreed := metav1.NewTime(agreedAt)
	treebind.Status.LastSyncTime = &agreed
	return treebind
}

func TestTheCommandsThatReadTheConditionsPrintWhatThePageShows(t *testing.T) {
	treebind := refusedPush(t)

	commands := expressions(t, "read-the-conditions")
	if len(commands) != 2 {
		t.Fatalf("the block has %d commands, want the conditions and the commit", len(commands))
	}
	conditions := evaluate(t, commands[0], treebind)
	agreed := evaluate(t, commands[1], treebind)

	if want := fenced(t, "output", "a-push-that-the-host-refused", ""); conditions != want {
		t.Errorf("the conditions print\n%s\nthe page shows\n%s", conditions, want)
	}
	if want := commitOne + "\t" + agreedAt.Format(time.RFC3339) + "\n"; agreed != want {
		t.Errorf("the commit and the time print %q, want %q", agreed, want)
	}
}

func TestTheCommandThatReadsTheSettingsPrintsThem(t *testing.T) {
	treebind := refusedPush(t)
	treebind.Spec.Sync.ConflictPolicy = treebindv1alpha1.ConflictPolicyFollowLocal
	treebind.Spec.Repository.Path = "team/dashboards"

	got := evaluate(t, expressions(t, "read-the-settings")[0], treebind)

	if want := "bidirectional\tfollowLocal\t30s\tteam/dashboards\n"; got != want {
		t.Errorf("the settings print %q, want %q", got, want)
	}
}

func TestTheCommandThatSaysSinceWhenSyncedSaysWhatItSaysPrintsWhatThePageShows(t *testing.T) {
	got := evaluate(t, expressions(t, "since-when-synced-says-what-it-says")[0], refusedPush(t))

	if want := fenced(t, "output", "since-when-synced-says-what-it-says", ""); got != want {
		t.Errorf("the command prints %q, the page shows %q", got, want)
	}
	if !strings.Contains(got, refusedAt) {
		t.Errorf("the command prints %q, want the time of the last change of status", got)
	}
}

func TestTheCommandsThatListEveryTreebindPrintOneLineEach(t *testing.T) {
	refused := refusedPush(t)
	refused.Namespace, refused.Name = "apps", "grafana"
	other := syncTreebind(bidirectional, treebindv1alpha1.TreebindStatus{})
	other.Namespace, other.Name, other.Spec.Target.ClaimName, other.Spec.Target.SubPath = "apps", "second", testClaimName, "alerts"
	refused.Spec.Target.ClaimName, refused.Spec.Target.SubPath = testClaimName, "alerts"
	pending := syncTreebind(bidirectional, treebindv1alpha1.TreebindStatus{})
	pending.Namespace, pending.Name = "other", "bystander"
	list := listOf(refused, other, pending)

	synced := evaluate(t, expressions(t, "synced-of-every-treebind")[0], list)
	claims := evaluate(t, expressions(t, "find-treebinds-that-share-a-claim")[0], list)

	if want := "apps/grafana\tFalse\tPushRejected\t" + refusedAt + "\napps/second\t\t\t\nother/bystander\t\t\t\n"; synced != want {
		t.Errorf("the list prints %q, want %q: a Treebind with no Synced condition yet is an empty line, not a missing one", synced, want)
	}
	if want := "apps/grafana\t" + testClaimName + "\talerts\napps/second\t" + testClaimName + "\talerts\nother/bystander\t" + testClaimName + "\t\n"; claims != want {
		t.Errorf("the claims print %q, want %q", claims, want)
	}
}

func TestTheCommandThatSaysHowLongSyncedHasSaidItSaysIt(t *testing.T) {
	if err := exec.Command("date", "-u", "-d", "2026-01-01T00:00:00Z", "+%s").Run(); err != nil {
		t.Skip("date here is not GNU date, which the page says the command needs")
	}
	dir := t.TempDir()
	// A kubectl that gives the time that the test says, for the command to count from.
	script := "#!/bin/sh\nprintf '%s' \"$SINCE\"\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	block := strings.ReplaceAll(fenced(t, "tested", "how-long-synced-has-said-it", "sh"), "<name>", "grafana")
	cmd := exec.Command("sh", "-e", "-c", block)
	cmd.Env = []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "SINCE=" + time.Now().UTC().Add(-90*time.Minute).Format(time.RFC3339)}

	out, err := cmd.CombinedOutput()

	if err != nil || string(out) != "Synced has said what it says for 90 minutes\n" {
		t.Errorf("the command printed %q (%v), want the 90 minutes that the time is old", out, err)
	}
}

// reasonRows are the rows of the table that says where each reason is dealt with: the reasons of its first column, and the
// text of its third, which says what happens to Ready.
func reasonRows(t *testing.T) map[string]string {
	t.Helper()
	_, rest, found := strings.Cut(pageText(t), "### Which section deals with a reason\n")
	if !found {
		t.Fatal("the page has no table of the reasons")
	}
	quoted := regexp.MustCompile("`([A-Za-z]+)`")
	rows := map[string]string{}
	for line := range strings.Lines(rest) {
		if strings.HasPrefix(line, "## ") {
			break
		}
		cells := strings.Split(line, "|")
		if !strings.HasPrefix(line, "| `") || len(cells) < 6 {
			continue
		}
		for _, match := range quoted.FindAllStringSubmatch(cells[1], -1) {
			rows[match[1]] = strings.TrimSpace(cells[3])
		}
	}
	return rows
}

func TestTheTableOfReasonsHasEveryReasonOfSyncedAndSaysWhatReadyDoes(t *testing.T) {
	rows := reasonRows(t)

	want := make([]string, 0, len(failedStepReasons)+1)
	for reason := range failedStepReasons {
		want = append(want, reason)
	}
	want = append(want, treebindv1alpha1.ReasonSyncFailed)
	slices.Sort(want)
	got := make([]string, 0, len(rows))
	for reason := range rows {
		got = append(got, reason)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("the table has the reasons %v, want %v: the reasons of Synced False that the operator defines, and SyncFailed", got, want)
	}

	for reason, ready := range rows {
		if reason == treebindv1alpha1.ReasonSyncFailed {
			if !strings.HasPrefix(ready, "`False`") {
				t.Errorf("%s: Ready = %q, want False: a reason that waits for a person", reason, ready)
			}
			continue
		}
		stays := strings.HasPrefix(ready, "stays `True`")
		if waits := WaitsForRepository(reason, bidirectional); stays != waits {
			t.Errorf("%s: the page says Ready %q, and the code says that the reason waits for the repository: %v", reason, ready, waits)
		}
		// What the page says of a Treebind that only pulls: it says so when the reason is not waited for there.
		qualified := strings.Contains(ready, "`False` for a Treebind that only pulls") || strings.Contains(ready, "for a `bidirectional` Treebind")
		if waits := WaitsForRepository(reason, pullOnly); qualified != (stays && !waits) {
			t.Errorf("%s: the page says Ready %q, and for a Treebind that only pulls the code says that it waits for the repository: %v", reason, ready, waits)
		}
	}
}
