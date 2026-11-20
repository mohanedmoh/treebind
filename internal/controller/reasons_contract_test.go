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
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	treebindv1alpha1 "github.com/mohanedmoh/treebind/api/v1alpha1"
	"github.com/mohanedmoh/treebind/internal/agent/preflight"
)

// The reason of Synced False is the reason of the step of the agent that failed, and the reasons are API in the sense
// of docs/adr/0004: people and tooling match on them. The agent spells them where it makes the checks and the API
// spells them for the condition, and nothing but these tests says that the two are the same.

// failedStepReasons are the reasons of a step that failed, as the API names them, in the order of the steps of the
// agent. Synced False takes one of them, and Conflict True takes the ones of the pull.
var failedStepReasons = []string{
	// Mount, Writable and GitDir.
	treebindv1alpha1.ReasonMountPathInvalid,
	treebindv1alpha1.ReasonNotMounted,
	treebindv1alpha1.ReasonNotWritable,
	treebindv1alpha1.ReasonReadOnlyVolume,
	treebindv1alpha1.ReasonWriteFailed,
	treebindv1alpha1.ReasonGitDirUnsafe,
	treebindv1alpha1.ReasonTimedOut,
	// Lock.
	treebindv1alpha1.ReasonLockFailed,
	// Checkout.
	treebindv1alpha1.ReasonDirectoryNotEmpty,
	treebindv1alpha1.ReasonRepositoryMismatch,
	treebindv1alpha1.ReasonBranchNotFound,
	treebindv1alpha1.ReasonReservedPath,
	treebindv1alpha1.ReasonFetchFailed,
	treebindv1alpha1.ReasonCheckoutFailed,
	treebindv1alpha1.ReasonPathNotFound,
	treebindv1alpha1.ReasonPathMismatch,
	// Pull.
	treebindv1alpha1.ReasonBlocked,
	treebindv1alpha1.ReasonDiverged,
	treebindv1alpha1.ReasonBranchGone,
	treebindv1alpha1.ReasonTooManyDeletions,
	treebindv1alpha1.ReasonRefused,
	treebindv1alpha1.ReasonTooManyCommits,
	treebindv1alpha1.ReasonPullFailed,
	// Changes, Commit and Push.
	treebindv1alpha1.ReasonScanFailed,
	treebindv1alpha1.ReasonCommitFailed,
	treebindv1alpha1.ReasonNonFastForward,
	treebindv1alpha1.ReasonPushRejected,
	treebindv1alpha1.ReasonPushFailed,
}

// operatorReasons are the reasons that the operator gives to Synced and Conflict with the spelling that people match
// on, and each one is spelled like its name. ConflictResolved is also the reason that the agent gives the Conflict
// check when it resolved a conflict, which the test of the agent's reasons holds to the same spelling.
var operatorReasons = []struct{ name, spelling string }{
	{"AwaitingAgentReport", treebindv1alpha1.ReasonAwaitingAgentReport},
	{"AgentUnreachable", treebindv1alpha1.ReasonAgentUnreachable},
	{"SyncPending", treebindv1alpha1.ReasonSyncPending},
	{"AgentOutdated", treebindv1alpha1.ReasonAgentOutdated},
	{"InSync", treebindv1alpha1.ReasonInSync},
	{"SyncFailed", treebindv1alpha1.ReasonSyncFailed},
	{"NoConflict", treebindv1alpha1.ReasonNoConflict},
	{"ConflictResolved", treebindv1alpha1.ReasonConflictResolved},
}

// agentReasonsOfNoFailure are the reasons that the agent has and that Synced does not take as the reason of a step that
// failed: NotChecked is the reason of a step that was skipped, which fails nothing, and ConflictResolved is the reason
// of the Conflict check when it says a conflict was resolved, which is the reason of Conflict False.
var agentReasonsOfNoFailure = []string{
	preflight.ReasonNotChecked,
	treebindv1alpha1.ReasonConflictResolved,
}

// reasonConstants are the constants called Reason... that a scan of the agent's source found: the ones that have a
// string of their own, by package.Name, and the ones that are the name of another package's constant, with the name
// they point at.
type reasonConstants struct {
	named   map[string]string
	aliases map[string]string
}

// read adds the constants called Reason... of a file. A form that it does not read is reported and not skipped, so
// that a reason that is written another way is not missed without anyone knowing.
func (found *reasonConstants) read(file *ast.File, path string, report func(format string, args ...any)) {
	for _, declaration := range file.Decls {
		block, ok := declaration.(*ast.GenDecl)
		if !ok || block.Tok != token.CONST {
			continue
		}
		for _, spec := range block.Specs {
			constants := spec.(*ast.ValueSpec)
			for index, name := range constants.Names {
				if !strings.HasPrefix(name.Name, "Reason") {
					continue
				}
				key := file.Name.Name + "." + name.Name
				if index >= len(constants.Values) {
					report("%s: %s has no value of its own, and this scan reads a string and the name of another constant", path, key)
					continue
				}
				switch value := constants.Values[index].(type) {
				case *ast.BasicLit:
					text, err := strconv.Unquote(value.Value)
					if value.Kind != token.STRING || err != nil {
						report("%s: %s is %s, and a reason is a string", path, key, value.Value)
						continue
					}
					found.named[key] = text
				case *ast.SelectorExpr:
					pkg, ok := value.X.(*ast.Ident)
					if !ok {
						report("%s: %s names something that is not a constant of a package", path, key)
						continue
					}
					found.aliases[key] = pkg.Name + "." + value.Sel.Name
				default:
					report("%s: %s is neither a string nor the name of another constant, which is all this scan reads", path, key)
				}
			}
		}
	}
}

// agentReasons returns the reasons that the agent can give, as the spelling it writes, each with the constants that
// name it: the value of every constant called Reason... in a package of the agent. It reads the source and does not
// list the constants, so that a reason that is added to the agent is found here and not only by whoever remembers to
// add it to a list. A constant that is the name of another package's constant is the same reason, and has to point at
// one that exists.
func agentReasons(t *testing.T) map[string][]string {
	t.Helper()
	fileSet := token.NewFileSet()
	found := &reasonConstants{named: map[string]string{}, aliases: map[string]string{}}
	walkErr := filepath.WalkDir(filepath.Join("..", "agent"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		found.read(file, path, t.Errorf)
		return nil
	})
	if walkErr != nil {
		t.Fatalf("reading the packages of the agent: %v", walkErr)
	}

	reasons := map[string][]string{}
	for key, value := range found.named {
		reasons[value] = append(reasons[value], key)
	}
	for key, target := range found.aliases {
		value, ok := found.named[target]
		if !ok {
			t.Errorf("%s names %s, which is not a reason of a package of the agent", key, target)
			continue
		}
		reasons[value] = append(reasons[value], key)
	}
	for value := range reasons {
		slices.Sort(reasons[value])
	}
	return reasons
}

func TestEveryReasonOfTheAgentIsNamedByTheAPIWithTheSameSpelling(t *testing.T) {
	agent := agentReasons(t)

	known := map[string]bool{}
	for _, reason := range failedStepReasons {
		known[reason] = true
	}
	for _, reason := range agentReasonsOfNoFailure {
		known[reason] = true
	}
	for reason, constants := range agent {
		if !known[reason] {
			t.Errorf("the agent gives the reason %q (%s) and the API has no constant for it: add one to api/v1alpha1 and to "+
				"the lists of this test, so that Synced can say it and the operator does not write SyncFailed for it",
				reason, strings.Join(constants, ", "))
		}
	}
	for _, reason := range failedStepReasons {
		if _, ok := agent[reason]; !ok {
			t.Errorf("the API names the reason %q and no step of the agent gives it: it is not spelled as the agent "+
				"spells it, or the agent dropped it", reason)
		}
	}
}

func TestTheReasonsOfAFailedStepAreListedOnceAndAreNotTheOperatorsOwn(t *testing.T) {
	seen := map[string]bool{}
	for _, reason := range failedStepReasons {
		if seen[reason] {
			t.Errorf("failedStepReasons lists %q twice", reason)
		}
		seen[reason] = true
	}
	// A step that failed with the reason of the operator's own would read as a state of the operator, and a user who
	// matches on the reason could not tell the two apart.
	for _, reason := range operatorReasons {
		if seen[reason.spelling] {
			t.Errorf("the reason %q of a failed step is also the operator's reason %s", reason.spelling, reason.name)
		}
	}
}

func TestTheReasonsOfTheOperatorKeepTheirSpelling(t *testing.T) {
	for _, reason := range operatorReasons {
		if reason.spelling != reason.name {
			t.Errorf("the reason %s is spelled %q, and a user who matches on it would not find it",
				reason.name, reason.spelling)
		}
	}
}
