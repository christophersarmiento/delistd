package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/drumandbytes/delistd/internal/evidence"
	"github.com/drumandbytes/delistd/internal/history"
	"github.com/go-chi/chi/v5"
)

// urlID reads a numeric id from the route. A part that is not a plain number
// ("12abc") is not an id.
func urlID(r *http.Request, param string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, param), 10, 64)
	return id, err == nil
}

// store returns the history store, or answers 500 itself when there is none.
func (s *Server) store(w http.ResponseWriter, message string) *history.Store {
	if s.historyStore == nil {
		http.Error(w, message, http.StatusInternalServerError)
	}
	return s.historyStore
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if cfg := s.getConfig(); cfg == nil || !cfg.HasProfile() {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	active := s.activeProfile(r)
	s.renderWithCSRF(w, r, "dashboard.html", map[string]interface{}{
		"Title":         "Dashboard",
		"Profile":       active.Profile,
		"BrokerCount":   len(s.brokers().Brokers),
		"RecentHistory": s.getRecentHistory(active.ID, 10),
		"Stats":         s.getStats(active.ID),
		"PipelineStats": s.getPipelineStats(active.ID),
	})
}

// brokerPageSize is how many broker rows the list renders at a time. All 700+
// rendered twice (table and phone cards) made a 2 MB page that phones took
// seconds to lay out; "Show more" asks for another page.
const brokerPageSize = 50

// pageOfBrokers returns the rows to render for ?limit=, which grows by
// brokerPageSize each time "Show more" is pressed.
func pageOfBrokers(r *http.Request, all []BrokerWithStatus) (rows []BrokerWithStatus, limit int) {
	limit = brokerPageSize
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > limit {
		limit = n
	}
	return all[:min(limit, len(all))], limit
}

// brokerQueryFrom reads the brokers page's filters from the URL.
func brokerQueryFrom(r *http.Request) brokerQuery {
	q := r.URL.Query()
	return brokerQuery{
		Search: q.Get("search"), Category: q.Get("category"), Region: q.Get("region"), Status: q.Get("status"),
		MissingEmail: q.Get("missing_email") == "true", ShowExcluded: q.Get("show_excluded") == "true",
	}
}

func (s *Server) handleBrokers(w http.ResponseWriter, r *http.Request) {
	q := brokerQueryFrom(r)
	profileID := s.activeProfile(r).ID
	all := s.listBrokers(profileID, q)
	shown, limit := pageOfBrokers(r, all)
	hidden, country := s.countryHidden(profileID)
	cfg := s.getConfig()

	s.renderWithCSRF(w, r, "brokers.html", map[string]interface{}{
		"Title":         "Data Brokers",
		"Brokers":       shown,
		"Limit":         limit,
		"Categories":    s.getUniqueCategories(),
		"Regions":       s.getUniqueRegions(),
		"Search":        q.Search,
		"Category":      q.Category,
		"Region":        q.Region,
		"Status":        q.Status,
		"MissingEmail":  q.MissingEmail,
		"ShowExcluded":  q.ShowExcluded,
		"Total":         len(s.brokers().Brokers),
		"Filtered":      len(all),
		"CountryHidden": hidden,
		"Country":       country,
		"DailyLimit":    effectiveDailyLimit(cfg),
		"ManualMode":    cfg != nil && cfg.IsManualSend(),
	})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("status")
	records := s.getRecentHistory(s.activeProfile(r).ID, 1000)
	if filter == "sent" || filter == "failed" {
		var kept []history.Record
		for _, rec := range records {
			if string(rec.Status) == filter {
				kept = append(kept, rec)
			}
		}
		records = kept
	}
	s.renderWithCSRF(w, r, "history.html", map[string]interface{}{
		"Title":        "History",
		"History":      records,
		"StatusFilter": filter,
	})
}

// PipelineStats are the pipeline dashboard's counters.
type PipelineStats struct {
	EmailSent            int
	AwaitingResponse     int
	FormRequired         int
	FormFilled           int
	AwaitingCaptcha      int
	CaptchaSolved        int
	AwaitingConfirmation int
	Confirmed            int
	NoData               int
	Rejected             int
	Failed               int
	PendingTasks         int
	NeedsReview          int
}

func (s *Server) getPipelineStats(profileID string) PipelineStats {
	stats, _, _ := s.loadPipelineStats(profileID)
	return stats
}

func (s *Server) handleResponseReview(w http.ResponseWriter, r *http.Request) {
	id, ok := urlID(r, "responseID")
	if !ok {
		http.Error(w, "Response not found", http.StatusNotFound)
		return
	}
	store := s.store(w, "Database not available")
	if store == nil {
		return
	}
	response, err := store.GetBrokerResponseByID(id, s.activeProfile(r).ID)
	if err != nil || response == nil {
		http.Error(w, "Response not found", http.StatusNotFound)
		return
	}
	s.renderWithCSRF(w, r, "response-review.html", map[string]interface{}{"Title": "Review Response", "Response": response})
}

// handleExport downloads the active profile's evidence report, the same one
// `delistd export` writes (?format=html|json, optional ?since=YYYY-MM-DD).
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "html"
	}
	if format != "html" && format != "json" {
		http.Error(w, "format must be html or json", http.StatusBadRequest)
		return
	}
	var since time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			http.Error(w, "since must be YYYY-MM-DD", http.StatusBadRequest)
			return
		}
		since = t
	}
	store := s.store(w, "History database not available")
	if store == nil {
		return
	}

	profile := s.activeProfile(r)
	requests, err := store.GetAllRequests(profile.ID)
	if err != nil {
		http.Error(w, "Failed to read requests: "+err.Error(), http.StatusInternalServerError)
		return
	}
	responses, err := store.GetBrokerResponsesForExport(profile.ID)
	if err != nil {
		http.Error(w, "Failed to read responses: "+err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now()
	report := evidence.Build(profile.Profile, requests, responses, s.brokers(), s.tmplEngine, since, now)

	contentType := "text/html; charset=utf-8"
	var body []byte
	if format == "json" {
		contentType = "application/json"
		body, err = json.MarshalIndent(report, "", "  ")
	} else {
		body, err = evidence.RenderHTML(report)
	}
	if err != nil {
		http.Error(w, "Failed to render report: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="delistd-evidence-%s-%s.%s"`, profile.ID, now.Format("2006-01-02"), format))
	_, _ = w.Write(body)
}
