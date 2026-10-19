package core

import (
	"testing"

	"github.com/michael-berardi/openmail/internal/model"
)

func TestPlanSync(t *testing.T) {
	base := model.Mailbox{UIDValidity: 7, UIDNext: 101, LastUID: 100, Exists: 100, ModSeq: 500}
	cases := []struct {
		name    string
		mb      model.Mailbox
		valid   uint32
		exists  int
		uidNext uint32
		modSeq  uint64
		cond    bool
		full    bool
		want    syncPlan
	}{
		{"nothing changed", base, 7, 100, 101, 500, true, false, planSkip},
		{"two arrivals", base, 7, 102, 103, 503, true, false, planAppends},
		{"flag change only", base, 7, 100, 101, 510, true, false, planAppends},
		{"one expunge", base, 7, 99, 101, 505, true, false, planFull},
		{"expunge plus arrival looks equal in count but not in uidnext", base, 7, 100, 102, 505, true, false, planFull},
		{"two expunges one arrival", base, 7, 99, 102, 520, true, false, planFull},
		{"uidvalidity changed", base, 8, 100, 101, 500, true, false, planFull},
		{"no condstore", base, 7, 100, 101, 500, false, false, planFull},
		{"first sync", model.Mailbox{}, 7, 50, 51, 400, true, false, planFull},
		{"forced", base, 7, 100, 101, 500, true, true, planFull},
		{"modseq went backwards (server reset)", base, 7, 100, 101, 100, true, false, planFull},
		{"never recorded a modseq", model.Mailbox{UIDValidity: 7, UIDNext: 101, LastUID: 100, Exists: 100}, 7, 100, 101, 500, true, false, planFull},
	}
	for _, c := range cases {
		if got := planSync(c.mb, c.valid, c.exists, c.uidNext, c.modSeq, c.cond, c.full); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}
