package files

import (
	"context"
	"errors"
	"testing"
)

// failRedactorConstruction makes the process-wide redactor fail to build
// for the rest of the test.
func failRedactorConstruction(t *testing.T, err error) {
	t.Helper()
	original := processSecretRedactor
	processSecretRedactor = func() (*secretRedactor, error) { return nil, err }
	t.Cleanup(func() { processSecretRedactor = original })
}

func TestWorkerRunRefusesWithoutARedactor(t *testing.T) {
	errBuild := errors.New("detector did not build")
	failRedactorConstruction(t, errBuild)

	err := (&Worker{}).Run(context.Background())
	if !errors.Is(err, errBuild) {
		t.Fatalf("Run = %v, want the redactor's construction error", err)
	}
}

func TestNewRedactorReturnsNoRedactorOnError(t *testing.T) {
	errBuild := errors.New("detector did not build")
	failRedactorConstruction(t, errBuild)

	redactor, err := NewRedactor()
	if !errors.Is(err, errBuild) {
		t.Fatalf("NewRedactor error = %v, want %v", err, errBuild)
	}
	if redactor != nil {
		t.Fatalf("NewRedactor returned a Redactor (%T) with its error", redactor)
	}
}
