package history

import "testing"

// A store whose database is gone must report it from every migration step
// instead of carrying on with half a schema.
func TestMigrationsFailOnAClosedDatabase(t *testing.T) {
	s := newTestStore(t)
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	steps := map[string]func() error{
		"migrate":           s.migrate,
		"migrateTimestamps": s.migrateTimestamps,
		"migrateApprovals":  s.migrateApprovals,
		"migrateAutomation": s.migrateAutomation,
		"MigrateNoData": func() error {
			return s.MigrateNoData(func(BrokerResponse) (bool, float64, bool) { return true, 1, true })
		},
		"ResolveNoData": func() error { return s.ResolveNoData("p", "b", true) },
	}
	for name, step := range steps {
		if err := step(); err == nil {
			t.Errorf("%s succeeded on a closed database", name)
		}
	}
}
