package web

import (
	"encoding/json"
	"testing"
)

func TestNormalizeTenetProposalRejectsEmptyOrInvalid(t *testing.T) {
	cases := []TenetProposal{
		{Category: "backend", Name: "", Statement: "Prefer explicit errors.", Rationale: "Operators need context."},
		{Category: "backend", Name: "Errors", Statement: "", Rationale: "Operators need context."},
		{Category: "backend", Name: "Errors", Statement: "Prefer explicit errors.", Rationale: ""},
		{Category: "vibes", Name: "Errors", Statement: "Prefer explicit errors.", Rationale: "Operators need context."},
	}
	for _, tc := range cases {
		if _, ok := normalizeTenetProposal(tc); ok {
			t.Fatalf("expected proposal %#v to be rejected", tc)
		}
	}
}

func TestNormalizeTenetProposalTrimsAndDefaultsEvidence(t *testing.T) {
	got, ok := normalizeTenetProposal(TenetProposal{
		Category:  " Database ",
		Name:      " Operation context ",
		Statement: " Name every 500 by operation. ",
		Rationale: " Generic 500s make ops harder. ",
	})
	if !ok {
		t.Fatal("expected valid proposal")
	}
	if got.Category != "database" || got.Name != "Operation context" || got.Statement != "Name every 500 by operation." || got.Rationale != "Generic 500s make ops harder." {
		t.Fatalf("unexpected normalized proposal: %#v", got)
	}
	if string(got.Evidence) != "[]" {
		t.Fatalf("expected default evidence [], got %s", got.Evidence)
	}
}

func TestNormalizeTenetProposalPreservesEvidence(t *testing.T) {
	ev := json.RawMessage(`[{"type":"lesson","id":9}]`)
	got, ok := normalizeTenetProposal(TenetProposal{
		Category:  "security",
		Name:      "Trust boundaries",
		Statement: "Validate user supplied values at storage time.",
		Rationale: "Admin paths and user paths have different threat models.",
		Evidence:  ev,
	})
	if !ok {
		t.Fatal("expected valid proposal")
	}
	if string(got.Evidence) != string(ev) {
		t.Fatalf("expected evidence %s, got %s", ev, got.Evidence)
	}
}
