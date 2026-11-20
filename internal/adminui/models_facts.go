package adminui

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/setup"
)

// handleModelFacts turns reading the published model lists on or off.
func (s *Server) handleModelFacts(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetup(w) || !s.requireMutation(w, r) {
		return
	}
	version, err := strconv.ParseInt(r.Form.Get("version"), 10, 64)
	if err != nil {
		http.Error(w, "version is required", http.StatusBadRequest)
		return
	}
	on := r.Form.Get("published") == "on"
	_, err = s.setup.SetPublishedFacts(r.Context(), s.setupActor(r), version, on)
	switch {
	case errors.Is(err, settings.ErrConflict), errors.Is(err, settings.ErrInvalid):
		message, _ := setupNotice(err)
		redirectWithResult(w, r, modelsPath+"#facts", message, true)
	case err != nil:
		s.logger.Error("model facts switch failed", "err", err)
		http.Error(w, "models save failed", http.StatusInternalServerError)
	case on:
		redirectWithResult(w, r, modelsPath+"#facts", "Published model facts are on. The lists are read within a minute, and once a day after.", false)
	default:
		redirectWithResult(w, r, modelsPath+"#facts", "Published model facts are off. Nothing is fetched, and what was kept is no longer used.", false)
	}
}

// Dollars is an amount of money in words: cents below ten dollars, whole
// dollars above.
func dollars(amount float64) string {
	switch {
	case amount < 0.005:
		return "less than $0.01"
	case amount < 10:
		return "$" + strconv.FormatFloat(amount, 'f', 2, 64)
	}
	return "$" + strconv.FormatFloat(amount, 'f', 0, 64)
}

// factsLists says, for the Models page, when each published list was read.
func (d modelsData) FactsLists() []setup.FactsList { return d.Models.Facts.Lists }
