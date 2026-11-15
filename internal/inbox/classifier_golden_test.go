package inbox

import (
	"compress/gzip"
	"encoding/json"
	"math"
	"os"
	"slices"
	"testing"
)

// testdata/classifier_golden.json.gz holds ~7,000 emails built from every
// phrase the classifier knows (alone, combined, in subjects, with and without
// links, from bounce senders) and the verdicts the classifier gave them before
// it was rewritten. Where two types tied on points the old code picked one at
// random, so such a case lists every type it was seen to pick and only the
// type is compared.
//
// A tie is now settled the same way every time and always sent to review, so
// for those cases the test only requires one of the tied types plus the review flag.
type goldenCase struct {
	In  struct{ From, FromName, FromDomain, Subject, Body, HTMLBody string }
	Out struct {
		Types                                []string
		Confidence                           float64
		NeedsReview                          bool
		Reason, FormURL, ConfirmURL, Bounced string
		SubjTypes                            []string
		SubjConf                             float64
		SubjReview                           bool
	}
}

func TestClassifierMatchesGolden(t *testing.T) {
	f, err := os.Open("testdata/classifier_golden.json.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var cases []goldenCase
	if err := json.NewDecoder(zr).Decode(&cases); err != nil {
		t.Fatal(err)
	}
	bad := 0
	for i, c := range cases {
		in := c.In
		got := ClassifyResponse(&Email{From: in.From, FromName: in.FromName, FromDomain: in.FromDomain, Subject: in.Subject, Body: in.Body, HTMLBody: in.HTMLBody})
		fail := func(format string, args ...any) {
			if bad++; bad <= 15 {
				t.Errorf("case %d (%q / %q): "+format, append([]any{i, in.Subject, in.Body}, args...)...)
			}
		}
		subject, content := emailText(&Email{Subject: in.Subject, Body: in.Body, HTMLBody: in.HTMLBody})
		pts := scoreReply(subject, content, got.URLs)
		_, top, runnerUp := pts.top()
		viaScoring := slices.Contains(scored[:], got.Type) && got.Confidence < 1 // not a test email
		if viaScoring && top > 0 && top == runnerUp {
			if !got.NeedsReview {
				fail("tie on %d points not sent to review (type %s)", top, got.Type)
			}
		} else if !slices.Contains(c.Out.Types, string(got.Type)) {
			fail("type %s, golden %v", got.Type, c.Out.Types)
		} else if len(c.Out.Types) == 1 {
			o := c.Out
			if !near(got.Confidence, o.Confidence) || got.NeedsReview != o.NeedsReview || got.Reason != o.Reason ||
				got.FormURL != o.FormURL || got.ConfirmURL != o.ConfirmURL || got.BouncedRecipient != o.Bounced {
				fail("got conf %v review %v reason %q form %q confirm %q bounced %q; golden conf %v review %v reason %q form %q confirm %q bounced %q",
					got.Confidence, got.NeedsReview, got.Reason, got.FormURL, got.ConfirmURL, got.BouncedRecipient,
					o.Confidence, o.NeedsReview, o.Reason, o.FormURL, o.ConfirmURL, o.Bounced)
			}
		}
		ty, cf, rv := ClassifyBySubjectOnly(in.Subject)
		if !slices.Contains(c.Out.SubjTypes, string(ty)) || (len(c.Out.SubjTypes) == 1 && (!near(cf, c.Out.SubjConf) || rv != c.Out.SubjReview)) {
			fail("subject-only %s %v %v, golden %v %v %v", ty, cf, rv, c.Out.SubjTypes, c.Out.SubjConf, c.Out.SubjReview)
		}
	}
	if bad > 0 {
		t.Fatalf("%d of %d cases differ from the golden file", bad, len(cases))
	}
}

// near: fused multiply-add on arm64 makes the last bit of a float differ from amd64.
func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
