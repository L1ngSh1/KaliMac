package runtime

import "context"

// FakeCall records one external command invocation.
type FakeCall struct {
	Name string
	Args []string
}

// FakeExecutor is a scriptable Executor for tests. Respond is called for
// every Run; calls are recorded in order.
type FakeExecutor struct {
	LookPathResult string
	LookPathErr    error
	Respond        func(name string, args []string) (stdout []byte, stderr []byte, err error)

	Calls []FakeCall
}

func (f *FakeExecutor) LookPath(name string) (string, error) {
	if f.LookPathErr != nil {
		return "", f.LookPathErr
	}
	if f.LookPathResult != "" {
		return f.LookPathResult, nil
	}
	return "/usr/bin/" + name, nil
}

func (f *FakeExecutor) Run(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
	f.Calls = append(f.Calls, FakeCall{Name: name, Args: append([]string(nil), args...)})
	if f.Respond == nil {
		return nil, nil, nil
	}
	stdout, stderr, err := f.Respond(name, args)
	return stdout, stderr, err
}

// RunErr builds a RunError the way CommandExecutor would.
func RunErr(stderr string, exitCode int) error {
	return &RunError{Err: errString("exit status"), Stderr: []byte(stderr), ExitCode: exitCode}
}

type errString string

func (e errString) Error() string { return string(e) }
