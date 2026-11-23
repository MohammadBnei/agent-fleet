package decisions

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// Card events use ce_id card/<id>/<state> (docs/adr/0078 amendment): each state is
// published once, so no writer may move a decision back to 'open'. A card is
// inserted open (the column default) and only ever leaves it.
func TestNoWriterReopensADecision(t *testing.T) {
	upd := regexp.MustCompile(`(?is)UPDATE\s+decisions\s+SET(.*?)(?:WHERE|$)`)
	re := regexp.MustCompile(`(?is)(?:\bSET|,)\s*state\s*=\s*'open'|\b(?:THEN|ELSE)\s+'open'`)
	err := filepath.Walk("..", func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Ext(p) != ".go" || filepath.Base(p) == "never_reopen_test.go" {
			return err
		}
		b, err := os.ReadFile(p)
		for _, m := range upd.FindAllSubmatch(b, -1) { // the SET clause only, not the WHERE
			if re.Match(m[1]) {
				t.Errorf("%s moves a decision back to open", p)
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
