package dashboard

import (
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/journal"
)

// ADR-0086 §5: GetJournal labels quarantined rows so the owner sees what waits for promotion.
func TestJournalEntryToProtoCarriesQuarantined(t *testing.T) {
	if !journalEntryToProto(journal.Entry{Quarantined: true}).GetQuarantined() || journalEntryToProto(journal.Entry{}).GetQuarantined() {
		t.Error("quarantined flag not carried through")
	}
}
