package toolpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"reasonix/internal/diff"
	"reasonix/internal/evidence"
	"reasonix/internal/jobs"
	"reasonix/internal/memory"
)

// stageRecorder logs which policy stages a call reached, in order. The ordering
// is the contract this package publishes, so it is what the tests assert.
type stageRecorder struct {
	seen []string
	// gateAllow / hookBlock drive the two stages that can refuse a call.
	gateAllow bool
	gateErr   error
	hookBlock bool
}

func (r *stageRecorder) Check(_ context.Context, name string, _ json.RawMessage, _ bool) (bool, string, error) {
	r.seen = append(r.seen, "gate")
	return r.gateAllow, "denied by policy", r.gateErr
}

func (r *stageRecorder) PreToolUse(_ context.Context, _ string, _ json.RawMessage) (bool, string) {
	r.seen = append(r.seen, "pre-hook")
	return r.hookBlock, "hook says no"
}

func (r *stageRecorder) PostToolUse(_ context.Context, _ string, _ json.RawMessage, _ string) {
	r.seen = append(r.seen, "post-hook")
}

func (r *stageRecorder) SubagentStop(_ context.Context, _ string) {
	r.seen = append(r.seen, "subagent")
}

func writerCall(rec *stageRecorder) Call {
	return Call{
		ID: "1", Name: "write_file", Args: json.RawMessage(`{"path":"x"}`), ReadOnly: false,
		Preview: func(json.RawMessage) (diff.Change, error) {
			rec.seen = append(rec.seen, "preview")
			return diff.Change{Path: "x"}, nil
		},
	}
}

func pipelineFor(rec *stageRecorder) *Pipeline {
	p := New(rec, rec, nil)
	p.PreEdit = func(diff.Change) { rec.seen = append(rec.seen, "checkpoint") }
	return p
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestBeforeRunsStagesInOrder pins the documented order. It is not cosmetic:
// prompting the user before a deny rule bites, or snapshotting a checkpoint for a
// call that is then refused, are both real misbehaviours.
func TestBeforeRunsStagesInOrder(t *testing.T) {
	rec := &stageRecorder{gateAllow: true}
	p := pipelineFor(rec)

	ctx, block := p.Before(context.Background(), writerCall(rec))
	if block != nil {
		t.Fatalf("cleared call was blocked: %+v", block)
	}
	if want := []string{"gate", "pre-hook", "preview", "checkpoint"}; !equal(rec.seen, want) {
		t.Fatalf("stage order = %v, want %v", rec.seen, want)
	}
	// The cleared call's context carries the ledger the tools read.
	if _, ok := evidence.FromContext(ctx); !ok {
		t.Error("cleared call's context should carry the evidence ledger")
	}
}

// TestPlanModeRefusesWritersBeforeAnythingElse: plan mode is a local decision, so
// it must not cost the user a permission prompt first.
func TestPlanModeRefusesWritersBeforeAnythingElse(t *testing.T) {
	rec := &stageRecorder{gateAllow: true}
	p := pipelineFor(rec)
	p.SetPlanMode(true)

	_, block := p.Before(context.Background(), writerCall(rec))
	if block == nil {
		t.Fatal("plan mode should refuse a writer")
	}
	if len(rec.seen) != 0 {
		t.Fatalf("plan mode reached later stages: %v", rec.seen)
	}
	// Read-only calls are unaffected — that is the whole point of plan mode.
	ro := Call{ID: "2", Name: "read_file", Args: json.RawMessage(`{}`), ReadOnly: true}
	if _, block := p.Before(context.Background(), ro); block != nil {
		t.Fatalf("plan mode blocked a read-only call: %+v", block)
	}
}

// TestBlockedCallLeavesNoCheckpoint is the invariant the ordering exists for: a
// refused call must not leave a rewind point for a change that never happened.
func TestBlockedCallLeavesNoCheckpoint(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  *stageRecorder
		want []string
	}{
		{"gate denies", &stageRecorder{gateAllow: false}, []string{"gate"}},
		{"gate errors", &stageRecorder{gateAllow: true, gateErr: errors.New("cancelled")}, []string{"gate"}},
		{"hook blocks", &stageRecorder{gateAllow: true, hookBlock: true}, []string{"gate", "pre-hook"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := pipelineFor(tc.rec)
			_, block := p.Before(context.Background(), writerCall(tc.rec))
			if block == nil {
				t.Fatal("call should have been blocked")
			}
			if !equal(tc.rec.seen, tc.want) {
				t.Fatalf("stages = %v, want %v (a blocked call must not reach the checkpoint seam)", tc.rec.seen, tc.want)
			}
		})
	}
}

// TestReadOnlyCallsAreNotCheckpointed: there is nothing to restore.
func TestReadOnlyCallsAreNotCheckpointed(t *testing.T) {
	rec := &stageRecorder{gateAllow: true}
	p := pipelineFor(rec)
	call := Call{ID: "1", Name: "read_file", Args: json.RawMessage(`{}`), ReadOnly: true,
		Preview: func(json.RawMessage) (diff.Change, error) { return diff.Change{}, nil }}
	if _, block := p.Before(context.Background(), call); block != nil {
		t.Fatalf("read-only call blocked: %+v", block)
	}
	for _, s := range rec.seen {
		if s == "checkpoint" {
			t.Fatal("a read-only call was checkpointed")
		}
	}
}

// TestFailedCompleteStepIsNotItsOwnReceipt: complete_step is the *claim*. If a
// rejected claim were recorded as evidence, the next claim could cite it.
func TestFailedCompleteStepIsNotItsOwnReceipt(t *testing.T) {
	rec := &stageRecorder{gateAllow: true}
	p := pipelineFor(rec)
	call := Call{ID: "1", Name: "complete_step", Args: json.RawMessage(`{"step":"x"}`), ReadOnly: true}

	p.After(context.Background(), call, "rejected", errors.New("no evidence"))
	if _, ok := p.Evidence.LatestTodos(); ok {
		t.Error("a failed complete_step should leave no receipt")
	}
	// The observing hook still fires — the tool did run.
	if len(rec.seen) == 0 || rec.seen[len(rec.seen)-1] != "post-hook" {
		t.Errorf("PostToolUse should fire even for a failed call, stages=%v", rec.seen)
	}
}

// TestNilPipelineIsPassThrough: a bare engine (headless run, sub-agent, test)
// must work with no policy at all rather than needing empty stubs.
func TestNilPipelineIsPassThrough(t *testing.T) {
	var p *Pipeline
	ctx, block := p.Before(context.Background(), Call{Name: "bash"})
	if block != nil || ctx == nil {
		t.Fatal("a nil pipeline should clear every call")
	}
	p.After(context.Background(), Call{Name: "bash"}, "", nil)
	p.SubagentStopped(context.Background(), "")
	p.ResetTurn()
	if p.PlanMode() {
		t.Error("nil pipeline should report plan mode off")
	}
	if _, ok := p.EndOfTurnReminder(false); ok {
		t.Error("nil pipeline should have no reminder")
	}
}

// TestContextCarriesTheSessionHandles: the background tools and the memory tools
// find their manager/queue on the call context, not by importing a global.
func TestContextCarriesTheSessionHandles(t *testing.T) {
	rec := &stageRecorder{gateAllow: true}
	p := pipelineFor(rec)
	p.Jobs = jobs.NewManager(nil)
	p.Memory = queueFunc(func(string) {})

	ctx, block := p.Before(context.Background(), Call{ID: "1", Name: "bash", Args: json.RawMessage(`{}`), ReadOnly: true})
	if block != nil {
		t.Fatalf("blocked: %+v", block)
	}
	if _, ok := jobs.FromContext(ctx); !ok {
		t.Error("call context should carry the job manager")
	}
	if _, ok := memory.QueueFromContext(ctx); !ok {
		t.Error("call context should carry the memory queue")
	}
}

type queueFunc func(string)

func (f queueFunc) QueueMemory(note string) { f(note) }
