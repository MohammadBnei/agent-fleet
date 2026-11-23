package decisions

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Card events use ce_id card/<id>/<state> (docs/adr/0078 amendment): each state is
// published once, so no writer may move a decision back to 'open'. A card is
// inserted open (the column default) and only ever leaves it.
func TestNoWriterReopensADecision(t *testing.T) {
	upd := regexp.MustCompile(`(?is)UPDATE\s+decisions(?:\s+\w+)?\s+SET(.*?)(?:WHERE|$)`)
	re := regexp.MustCompile(`(?is)(?:^|,)\s*(?:\w+\.)?state\s*=\s*'open'|\b(?:THEN|ELSE)\s+'open'`)
	walk := func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || (filepath.Ext(p) != ".go" && !strings.HasSuffix(p, ".up.sql")) || filepath.Base(p) == "never_reopen_test.go" {
			return err
		}
		b, err := os.ReadFile(p)
		for _, m := range upd.FindAllSubmatch(b, -1) { // the SET clause only, not the WHERE
			if re.Match(m[1]) {
				t.Errorf("%s moves a decision back to open", p)
			}
		}
		return err
	}
	// Go callers, and migrations. Parameterised writers (SET state = $1) are out of a regex's
	// reach: migration 000042's trigger refuses them in the database.
	for _, root := range []string{"..", "../../../db/migrations"} {
		if err := filepath.Walk(root, walk); err != nil {
			t.Fatal(err)
		}
	}
}
