package web

import (
	"strconv"
	"strings"
	"testing"

	"github.com/voicetel/siprec-receiver/internal/siprecsrv"
)

// A call whose legs never received RTP says so on its page and in the
// list.
func TestNoAudioLegShownInTheUI(t *testing.T) {
	f := newFixture(t, adminEmail)
	id := recordLeg(t, f.store, siprecsrv.FinalizedLeg{CallID: "dlg-quiet", Label: "a", WAVPath: "/rec/q.wav", NoAudio: true})
	c := f.loginClient(t)

	if r := covGet(t, c, f.srv.URL+"/recordings/"+strconv.FormatInt(id, 10)); !strings.Contains(r.body, "No audio received") {
		t.Errorf("detail page does not flag the silent leg:\n%s", r.body)
	}

	if r := covGet(t, c, f.srv.URL+"/recordings"); !strings.Contains(r.body, "⚠ no audio") {
		t.Errorf("list does not badge the silent call:\n%s", r.body)
	}
}
