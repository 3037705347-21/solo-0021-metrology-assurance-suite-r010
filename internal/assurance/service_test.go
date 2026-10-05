package assurance_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"example.com/solo-0021-metrology-assurance-suite/internal/assurance"
	"example.com/solo-0021-metrology-assurance-suite/internal/contracts"
	"example.com/solo-0021-metrology-assurance-suite/internal/domain"
	"example.com/solo-0021-metrology-assurance-suite/internal/repository"
)

type fixture struct {
	svc    *assurance.Service
	ctx    context.Context
	device string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	svc := assurance.NewService(repository.New())
	ctx := context.Background()
	device, err := svc.RegisterDevice(ctx, contracts.RegisterDeviceRequest{
		AssetTag: " met-9001 ",
		Model:    "Bench Meter",
		Room:     "Room A",
		Steward:  "Lin",
	})
	if err != nil {
		t.Fatalf("register device: %v", err)
	}
	return &fixture{svc: svc, ctx: ctx, device: device.ID}
}

func (f *fixture) openCase(t *testing.T) string {
	t.Helper()
	view, err := f.svc.OpenCase(f.ctx, contracts.OpenCaseRequest{
		DeviceID:  f.device,
		Nominal:   25.0,
		Tolerance: 0.5,
		Steward:   "Qiao",
	})
	if err != nil {
		t.Fatalf("open case: %v", err)
	}
	return view.Case.ID
}

func (f *fixture) mustMeasure(t *testing.T, caseID string, observed float64, technician string) contracts.CaseResponse {
	t.Helper()
	view, err := f.svc.RecordMeasurement(f.ctx, caseID, contracts.RecordMeasurementRequest{
		Observed:   observed,
		Technician: technician,
	})
	if err != nil {
		t.Fatalf("record measurement %v: %v", observed, err)
	}
	return view
}

func (f *fixture) mustReopen(t *testing.T, caseID, reason string) {
	t.Helper()
	if _, err := f.svc.ReopenCase(f.ctx, caseID, contracts.ReopenCaseRequest{
		Actor:  "Lin",
		Reason: reason,
	}); err != nil {
		t.Fatalf("reopen case: %v", err)
	}
}

func measurementByID(items []domain.Measurement, id string) (domain.Measurement, bool) {
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return domain.Measurement{}, false
}

// After a rejected measurement is repaired and a passing measurement is
// recorded, the seal must cite the current cycle's accepted measurement, never
// the pre-repair out-of-tolerance reading.
func TestSealUsesAcceptedMeasurementFromCurrentCycle(t *testing.T) {
	f := newFixture(t)
	caseID := f.openCase(t)

	rejected := f.mustMeasure(t, caseID, 26.5, "Rui") // cycle 1, out of tolerance
	if rejected.Case.State != domain.CaseRecheckRequired {
		t.Fatalf("state after rejected measurement = %q", rejected.Case.State)
	}
	f.mustReopen(t, caseID, "setup replaced")
	accepted := f.mustMeasure(t, caseID, 25.1, "Rui") // cycle 2, in tolerance
	if accepted.Case.State != domain.CaseReviewReady {
		t.Fatalf("state after accepted measurement = %q", accepted.Case.State)
	}
	if accepted.Case.ReviewCycle != 2 {
		t.Fatalf("review cycle = %d, want 2", accepted.Case.ReviewCycle)
	}

	sealed, err := f.svc.SealCase(f.ctx, caseID, contracts.SealCaseRequest{
		Reviewer: "Vera",
		Summary:  "accepted measurement sealed for review",
	})
	if err != nil {
		t.Fatalf("seal case: %v", err)
	}
	if sealed.Case.State != domain.CaseSealed || sealed.Seal == nil {
		t.Fatalf("case was not sealed: state=%q seal=%v", sealed.Case.State, sealed.Seal)
	}
	if sealed.Seal.MeasurementID != "measurement-0002" {
		t.Fatalf("seal cites measurement %q, want measurement-0002 (cycle 2)", sealed.Seal.MeasurementID)
	}
	if sealed.Seal.ReviewCycle != 2 {
		t.Fatalf("seal review cycle = %d, want 2", sealed.Seal.ReviewCycle)
	}
	cited, ok := measurementByID(sealed.Measurements, sealed.Seal.MeasurementID)
	if !ok {
		t.Fatalf("sealed measurement %s is not in case history", sealed.Seal.MeasurementID)
	}
	if !cited.Accepted || cited.ReviewCycle != 2 {
		t.Fatalf("cited measurement accepted=%v cycle=%d, want accepted cycle 2", cited.Accepted, cited.ReviewCycle)
	}

	// The rejected reading is preserved for the audit trail but stays in its
	// own cycle and must never be the seal basis.
	old, ok := measurementByID(sealed.Measurements, rejected.Measurements[0].ID)
	if !ok {
		t.Fatalf("pre-repair measurement was erased from history")
	}
	if old.Accepted || old.ReviewCycle != 1 {
		t.Fatalf("pre-repair measurement accepted=%v cycle=%d, want rejected cycle 1", old.Accepted, old.ReviewCycle)
	}
}

// Across several repair rounds, old readings must not leak into the current
// review, and each round remains independently attributable.
func TestMultipleRepairRoundsStayIndependent(t *testing.T) {
	f := newFixture(t)
	caseID := f.openCase(t)

	// Round 1: reject (26.5), reopen.
	f.mustMeasure(t, caseID, 26.5, "Rui")
	f.mustReopen(t, caseID, "first repair")
	// Round 2: reject again (25.9), reopen.
	second := f.mustMeasure(t, caseID, 25.9, "Rui")
	if second.Measurements[1].ReviewCycle != 2 {
		t.Fatalf("second measurement cycle = %d, want 2", second.Measurements[1].ReviewCycle)
	}
	f.mustReopen(t, caseID, "second repair")
	// Round 3: pass (25.2).
	third := f.mustMeasure(t, caseID, 25.2, "Rui")
	if third.Case.ReviewCycle != 3 {
		t.Fatalf("review cycle = %d, want 3", third.Case.ReviewCycle)
	}
	if third.Measurements[2].ReviewCycle != 3 || !third.Measurements[2].Accepted {
		t.Fatalf("round 3 measurement not tagged as accepted cycle 3")
	}

	sealed, err := f.svc.SealCase(f.ctx, caseID, contracts.SealCaseRequest{
		Reviewer: "Vera",
		Summary:  "third round accepted",
	})
	if err != nil {
		t.Fatalf("seal case: %v", err)
	}
	if sealed.Seal.MeasurementID != "measurement-0003" || sealed.Seal.ReviewCycle != 3 {
		t.Fatalf("seal cites %s in cycle %d, want measurement-0003 cycle 3", sealed.Seal.MeasurementID, sealed.Seal.ReviewCycle)
	}

	// Every round must be visible on the measurements and the audit trail.
	cycles := map[int]int{}
	for _, measurement := range sealed.Measurements {
		cycles[measurement.ReviewCycle]++
	}
	if cycles[1] != 1 || cycles[2] != 1 || cycles[3] != 1 {
		t.Fatalf("measurements per cycle = %v, want one in each of cycles 1-3", cycles)
	}
	reopenCount := 0
	for _, event := range sealed.Events {
		if event.Kind == "case_reopened" {
			reopenCount++
			if event.Detail == "" {
				t.Fatalf("reopen event has no detail")
			}
		}
		if event.Kind == "case_sealed" {
			if !strings.Contains(event.Detail, "review cycle 3") {
				t.Fatalf("seal event detail %q does not identify review cycle 3", event.Detail)
			}
		}
	}
	if reopenCount != 2 {
		t.Fatalf("reopen events = %d, want 2", reopenCount)
	}
}

// A case with no accepted measurement in the current cycle cannot be sealed.
func TestSealWithoutCurrentCycleEvidenceIsRejected(t *testing.T) {
	f := newFixture(t)
	caseID := f.openCase(t)

	f.mustMeasure(t, caseID, 26.5, "Rui")

	// Sealing straight out of recheck_required must fail without touching state.
	if _, err := f.svc.SealCase(f.ctx, caseID, contracts.SealCaseRequest{
		Reviewer: "Vera",
		Summary:  "premature",
	}); !errors.Is(err, domain.ErrStateConflict) {
		t.Fatalf("seal in recheck_required error = %v, want state conflict", err)
	}
	f.mustReopen(t, caseID, "repair")
	// After reopen the old rejected reading must not serve as seal basis.
	if _, err := f.svc.SealCase(f.ctx, caseID, contracts.SealCaseRequest{
		Reviewer: "Vera",
		Summary:  "premature",
	}); !errors.Is(err, domain.ErrStateConflict) {
		t.Fatalf("seal awaiting new measurement error = %v, want state conflict", err)
	}

	view, err := f.svc.Case(f.ctx, caseID)
	if err != nil {
		t.Fatalf("read case: %v", err)
	}
	if view.Case.State != domain.CaseAwaitingMeasurement {
		t.Fatalf("rejected seal changed state to %q", view.Case.State)
	}
	if view.Seal != nil {
		t.Fatalf("rejected seal left a readable seal: %v", view.Seal)
	}
}

// An incomplete seal submission must neither change the case nor occupy a later
// correct submission.
func TestIncompleteSealSubmissionDoesNotOccupyCase(t *testing.T) {
	f := newFixture(t)
	caseID := f.openCase(t)
	f.mustMeasure(t, caseID, 25.2, "Rui")

	if _, err := f.svc.SealCase(f.ctx, caseID, contracts.SealCaseRequest{
		Reviewer: "",
		Summary:  "",
	}); !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("incomplete seal error = %v, want invalid input", err)
	}

	view, err := f.svc.Case(f.ctx, caseID)
	if err != nil {
		t.Fatalf("read case: %v", err)
	}
	if view.Case.State != domain.CaseReviewReady {
		t.Fatalf("incomplete seal changed state to %q", view.Case.State)
	}
	if view.Seal != nil {
		t.Fatalf("incomplete seal created evidence: %v", view.Seal)
	}

	sealed, err := f.svc.SealCase(f.ctx, caseID, contracts.SealCaseRequest{
		Reviewer: "Vera",
		Summary:  "complete evidence packet",
	})
	if err != nil {
		t.Fatalf("correct seal submission blocked after incomplete one: %v", err)
	}
	if sealed.Case.State != domain.CaseSealed || sealed.Seal == nil {
		t.Fatalf("case not sealed by the complete submission: state=%q", sealed.Case.State)
	}
}

// Two seals arriving concurrently must result in exactly one seal, and the
// losing attempt must not leave the case occupied without evidence.
func TestConcurrentSealsCreateExactlyOneCompleteSeal(t *testing.T) {
	f := newFixture(t)
	caseID := f.openCase(t)
	f.mustMeasure(t, caseID, 25.2, "Rui")

	const attempts = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			_, err := f.svc.SealCase(f.ctx, caseID, contracts.SealCaseRequest{
				Reviewer: fmt.Sprintf("Reviewer-%d", index),
				Summary:  "concurrent evidence",
			})
			results <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	successes, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, domain.ErrSealAlreadyCreated):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent seal error: %v", err)
		}
	}
	if successes != 1 || conflicts != attempts-1 {
		t.Fatalf("concurrent outcomes = %d success, %d conflict", successes, conflicts)
	}

	view, err := f.svc.Case(f.ctx, caseID)
	if err != nil {
		t.Fatalf("read case: %v", err)
	}
	if view.Case.State != domain.CaseSealed || view.Seal == nil {
		t.Fatalf("sealed case is not readable: state=%q seal=%v", view.Case.State, view.Seal)
	}
}

// A sealed case is terminal and keeps exactly one immutable evidence record.
func TestSealedCaseRejectsFurtherMutation(t *testing.T) {
	f := newFixture(t)
	caseID := f.openCase(t)
	f.mustMeasure(t, caseID, 25.2, "Rui")
	if _, err := f.svc.SealCase(f.ctx, caseID, contracts.SealCaseRequest{
		Reviewer: "Vera",
		Summary:  "done",
	}); err != nil {
		t.Fatalf("seal case: %v", err)
	}

	if _, err := f.svc.SealCase(f.ctx, caseID, contracts.SealCaseRequest{
		Reviewer: "Vera",
		Summary:  "again",
	}); !errors.Is(err, domain.ErrSealAlreadyCreated) {
		t.Fatalf("second seal error = %v, want seal exists", err)
	}
	if _, err := f.svc.RecordMeasurement(f.ctx, caseID, contracts.RecordMeasurementRequest{
		Observed:   25.1,
		Technician: "Rui",
	}); !errors.Is(err, domain.ErrStateConflict) {
		t.Fatalf("measurement after seal error = %v, want state conflict", err)
	}
	if _, err := f.svc.ReopenCase(f.ctx, caseID, contracts.ReopenCaseRequest{
		Actor:  "Lin",
		Reason: "reopen sealed",
	}); !errors.Is(err, domain.ErrStateConflict) {
		t.Fatalf("reopen after seal error = %v, want state conflict", err)
	}
}
