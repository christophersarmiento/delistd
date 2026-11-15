package history

import (
	"path/filepath"
	"testing"
	"time"
)

// Times are stored as text, which only sorts by time when every value has the
// same offset. Around a clock change a later send can carry a smaller local
// time than an earlier one.
func TestSendsAcrossAClockChangeKeepTheirOrder(t *testing.T) {
	s := newTestStore(t)
	summer := time.FixedZone("EEST", 3*3600)
	winter := time.FixedZone("EET", 2*3600)
	earlier := time.Date(2026, 10, 25, 3, 30, 0, 0, summer) // 00:30 UTC
	later := time.Date(2026, 10, 25, 3, 10, 0, 0, winter)   // 01:10 UTC
	for _, at := range []time.Time{earlier, later} {
		if err := s.Add(&Record{BrokerID: "x", BrokerName: "X", Email: "e", Template: "t", Status: StatusSent, SentAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	st, err := s.GetBrokerStatus("default", "x")
	if err != nil || !st.LastSent.Equal(later) {
		t.Errorf("last send = %v (%v), want %v", st.LastSent, err, later)
	}
	if n, _ := s.CountSentSince("default", time.Date(2026, 10, 25, 0, 45, 0, 0, time.UTC)); n != 1 {
		t.Errorf("sends since 00:45 UTC = %d, want 1", n)
	}
	latest, _ := s.LatestRequests("default")
	if len(latest) != 1 || !latest[0].SentAt.Equal(later) {
		t.Errorf("latest request = %+v", latest)
	}
}

// A database written before times were stored in UTC is converted on open.
func TestOldLocalTimestampsAreConvertedToUTC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO removal_requests (broker_id, broker_name, email, template, status, sent_at, created_at) VALUES
			('a', 'A', 'e', 't', 'sent', '2026-10-25 03:30:00 +0300 EEST', '2026-10-25 03:30:00.5 +0300 EEST m=+1.5'),
			('a', 'A', 'e', 't', 'sent', '2026-10-25 03:10:00 +0200 EET', '2026-10-25 03:10:00 +0200 EET'),
			('b', 'B', 'e', 't', 'sent', 'not a time', NULL)`,
		`DELETE FROM data_migrations WHERE name = 'utc_timestamps_v1'`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()

	s, err = NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	rows, err := s.db.Query(`SELECT sent_at, created_at FROM removal_requests ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	want := [][2]string{
		{"2026-10-25T00:30:00Z", "2026-10-25T00:30:00.5Z"},
		{"2026-10-25T01:10:00Z", "2026-10-25T01:10:00Z"},
		{"not a time", ""},
	}
	for i := 0; rows.Next(); i++ {
		var sent string
		var created *string
		if err := rows.Scan(&sent, &created); err != nil {
			t.Fatal(err)
		}
		got := [2]string{sent, ""}
		if created != nil {
			got[1] = *created
		}
		if got != want[i] {
			t.Errorf("row %d = %v, want %v", i, got, want[i])
		}
	}
	latest, err := s.GetBrokerStatus("default", "a")
	if err != nil || !latest.LastSent.Equal(time.Date(2026, 10, 25, 1, 10, 0, 0, time.UTC)) {
		t.Errorf("latest after conversion = %v (%v)", latest.LastSent, err)
	}
}
