package workflow

import "go.klarlabs.de/statekit"

// DeclaredStatuses lists every state the workflow machine knows.
//
// It exists so callers can be checked against the machine BEFORE they run.
// Job.SetStatus takes a bare string and validates at runtime, so an undeclared
// status compiles fine and fails only when a user invokes the command — which
// is exactly how `mnemos quality` shipped setting "computing", a state that has
// never existed, and failed on its first action for every user.
//
// This list is kept honest by TestDeclaredStatusesMatchTheMachine, which parses
// the State(...) literals below and fails if the two disagree. Add a state here
// only together with its transitions.
func DeclaredStatuses() []string {
	return []string{
		"pending", "running", "loading", "extracting", "relating",
		"saving", "querying", "embedding", "retrying", "completed", "failed",
	}
}

// newStatusMachine returns the workflow state machine used to validate job
// status transitions.
func newStatusMachine() (*statekit.Interpreter[struct{}], error) {
	machine, err := statekit.NewMachine[struct{}]("workflow").
		WithInitial("pending").
		State("pending").On("running").Target("running").Done().
		State("running").
		On("loading").Target("loading").
		On("querying").Target("querying").
		On("completed").Target("completed").
		On("failed").Target("failed").
		On("retrying").Target("retrying").
		Done().
		State("loading").
		On("extracting").Target("extracting").
		On("querying").Target("querying").
		On("saving").Target("saving").
		On("relating").Target("relating").
		On("embedding").Target("embedding").
		On("completed").Target("completed").
		On("failed").Target("failed").
		On("retrying").Target("retrying").
		Done().
		State("extracting").
		On("saving").Target("saving").
		On("relating").Target("relating").
		On("embedding").Target("embedding").
		On("completed").Target("completed").
		On("failed").Target("failed").
		On("retrying").Target("retrying").
		Done().
		State("relating").
		On("saving").Target("saving").
		On("completed").Target("completed").
		On("failed").Target("failed").
		On("retrying").Target("retrying").
		Done().
		State("saving").
		On("embedding").Target("embedding").
		On("completed").Target("completed").
		On("failed").Target("failed").
		On("retrying").Target("retrying").
		Done().
		State("querying").
		On("completed").Target("completed").
		On("failed").Target("failed").
		On("retrying").Target("retrying").
		Done().
		State("embedding").
		On("completed").Target("completed").
		On("failed").Target("failed").
		On("retrying").Target("retrying").
		Done().
		State("retrying").
		On("running").Target("running").
		On("loading").Target("loading").
		On("querying").Target("querying").
		On("extracting").Target("extracting").
		On("relating").Target("relating").
		On("saving").Target("saving").
		On("embedding").Target("embedding").
		On("failed").Target("failed").
		Done().
		State("completed").Final().Done().
		State("failed").Final().Done().
		Build()
	if err != nil {
		return nil, err
	}

	return statekit.NewInterpreter(machine), nil
}
