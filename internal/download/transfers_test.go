package download

import (
	"errors"
	"fmt"
	"testing"

	"github.com/elsbrock/go-putio"
)

func TestIsPutioNotFoundUnwrapsClientErrors(t *testing.T) {
	notFound := &putio.ErrorResponse{Type: "NotFound"}

	if !isPutioNotFound(notFound) {
		t.Fatal("expected direct Put.io NotFound error to be recognized")
	}
	if !isPutioNotFound(fmt.Errorf("get transfer files: %w", notFound)) {
		t.Fatal("expected wrapped Put.io NotFound error to be recognized")
	}
	if !isPutioNotFound(fmt.Errorf("process transfer: %w", fmt.Errorf("get transfer files: %w", notFound))) {
		t.Fatal("expected doubly wrapped Put.io NotFound error to be recognized")
	}
	if isPutioNotFound(&putio.ErrorResponse{Type: "Other"}) {
		t.Fatal("did not expect a different Put.io error type to match")
	}
	if isPutioNotFound(errors.New("not found")) {
		t.Fatal("did not expect an unrelated error to match")
	}
}

func TestHandleTransferErrorCleansUpWrappedNotFound(t *testing.T) {
	manager := newTestManager()
	transfer := &putio.Transfer{ID: 1, Name: "Example", FileID: 2}
	err := fmt.Errorf("get transfer files: %w", &putio.ErrorResponse{Type: "NotFound"})

	manager.processor.handleTransferError(transfer, err)

	transferContext, ok := manager.coordinator.GetTransferContext(transfer.ID)
	if !ok {
		t.Fatal("expected wrapped NotFound error to initialize transfer cleanup")
	}
	if got := transferContext.GetState(); got != TransferLifecycleProcessed {
		t.Fatalf("transfer state = %s, want Processed", got)
	}
}

func TestHandleTransferErrorLeavesUnrelatedErrorsUntracked(t *testing.T) {
	manager := newTestManager()
	transfer := &putio.Transfer{ID: 1, Name: "Example", FileID: 2}

	manager.processor.handleTransferError(transfer, errors.New("temporary API failure"))

	if _, ok := manager.coordinator.GetTransferContext(transfer.ID); ok {
		t.Fatal("did not expect unrelated error to initialize transfer cleanup")
	}
}
