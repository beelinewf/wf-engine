package engine

import (
	"testing"

	"github.com/beelinewf/wf-engine/internal/workflow/model"
)

// TestScopeKind pins the kind discrimination: the constructors decide
// parent vs branch, never the branchID sentinel.
func TestScopeKind(t *testing.T) {
	parent := instanceScope(model.WorkflowInstance{})
	if parent.isBranch() {
		t.Error("instanceScope isBranch = true, want false")
	}
	branch := branchScope(model.ParallelBranch{ID: "b-1"}, model.ParallelExecution{}, false)
	if !branch.isBranch() {
		t.Error("branchScope isBranch = false, want true")
	}
	// A branch id without the branch kind stays a parent scope.
	if (&execScope{branchID: "b-1"}).isBranch() {
		t.Error("execScope{branchID} isBranch = true, want false: kind, not the sentinel, decides")
	}
}

// TestParkStatus pins the post-checkpoint status: waiting unless a
// deferred pause or debug step-through forces paused. Parent and branch
// scopes share it; branch reasons go through nextBranchReason.
func TestParkStatus(t *testing.T) {
	e := &Engine{}
	cases := []struct {
		name  string
		scope *execScope
		want  model.WorkflowStatus
	}{
		{"parent", instanceScope(model.WorkflowInstance{}), model.WorkflowWaiting},
		{"branch", branchScope(model.ParallelBranch{ID: "b-1"}, model.ParallelExecution{}, false), model.WorkflowWaiting},
		{"pause requested", &execScope{pauseRequested: true}, model.WorkflowPaused},
		{"debug", &execScope{debug: true}, model.WorkflowPaused},
	}
	for _, c := range cases {
		if got := e.parkStatus(c.scope); got != c.want {
			t.Errorf("parkStatus(%s) = %s, want %s", c.name, got, c.want)
		}
	}

	branch := branchScope(model.ParallelBranch{ID: "b-1"}, model.ParallelExecution{}, true)
	if got := nextBranchReason(branch, model.WaitingReasonRunnable); got != model.WaitingReasonPaused {
		t.Errorf("nextBranchReason(debug, runnable) = %s, want paused", got)
	}
	if got := nextBranchReason(branch, model.WaitingReasonInput); got != model.WaitingReasonInput {
		t.Errorf("nextBranchReason(debug, input) = %s, want input passthrough", got)
	}
}
