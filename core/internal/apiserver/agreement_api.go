package apiserver

import (
	"net/http"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/store/postgres"
	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/cost"
)

// Two read-only diagnostics about Fleet's own metering.
//
// Neither changes a price and neither is needed to serve traffic. They exist
// because the ledger can be quietly wrong in a way no total reveals: the
// gateway's count of an answer and the engine's account of the same answer are
// produced by different machinery, and when they drift apart the only symptom
// is an invoice that is a few percent wrong in a direction nobody can explain.
//
// The pair is deliberate. "The two measurements disagree" says there is a bug;
// "here is what everything was billed on" says how much the bug is worth. Either
// alone leaves the operator guessing: a clean agreement on a fleet of reserved
// rows means nothing, and a fault on a fleet that measured everything by hand is
// a much larger problem than the fault says.

// agreementResponse is the whole diagnostic. No pagination: one row per
// endpoint that served traffic in the window, which is bounded by the number of
// endpoints rather than by the number of requests.
type agreementResponse struct {
	Period string `json:"period"`
	From   string `json:"from"`
	To     string `json:"to"`
	AsOf   string `json:"asOf"`
	// Sources counts every settled row by what it was billed on. The three
	// values are not variations on a theme: `engine` is measured, `counted` is
	// measured by a different tool, `reserved` is arithmetic.
	Sources map[string]int64 `json:"sources"`
	// Keys is one entry per endpoint, ordered by key.
	Keys []agreementKey `json:"keys"`
	// Faults is the subset worth stopping for, repeated at the top so a
	// dashboard does not have to filter the whole table to draw one line.
	Faults int `json:"faults"`
	// MinSamples and TolerancePercent are the policy in force, echoed so a
	// report is reproducible: the same rows under a different threshold are a
	// different finding, and a finding nobody can reproduce is a rumour.
	MinSamples       int `json:"minSamples"`
	TolerancePercent int `json:"tolerancePercent"`
}

type agreementKey struct {
	Endpoint string  `json:"endpoint"`
	Ratio    float64 `json:"ratio"`
	Percent  int     `json:"percent"`
	Fault    bool    `json:"fault"`
	Reason   string  `json:"reason"`
	// EngineRows and CountedRows are the two populations behind the ratio.
	// Printed next to it because "80% off" means something different over five
	// requests than over five thousand.
	EngineRows  int64 `json:"engineRows"`
	CountedRows int64 `json:"countedRows"`
	// TruncatedRows is how many counted answers hit the capture cap. A key with
	// any of them cannot be judged, and Reason says so.
	TruncatedRows int64 `json:"truncatedRows"`
}

// getUsageAgreement reports how the two measurements of output tokens compare.
func (s *Server) getUsageAgreement(w http.ResponseWriter, r *http.Request) {
	if !s.requireDB(w) {
		return
	}
	ctx := r.Context()
	now := time.Now().UTC()
	period := cost.NewPeriod(now)
	window := postgres.Window{From: period.Start, To: period.End}

	obs, err := s.db.Agreement(ctx, window)
	if err != nil {
		failInternal(w, err)
		return
	}
	mix, err := s.db.SourceMix(ctx, window)
	if err != nil {
		failInternal(w, err)
		return
	}

	findings := billing.Verify(obs, billing.DefaultMinSamples, billing.DefaultTolerance)
	out := agreementResponse{
		Period:           period.String(),
		From:             period.Start.Format(time.RFC3339),
		To:               period.End.Format(time.RFC3339),
		AsOf:             now.Format(time.RFC3339),
		Sources:          mix,
		Keys:             make([]agreementKey, 0, len(findings)),
		MinSamples:       billing.DefaultMinSamples,
		TolerancePercent: billing.DefaultTolerance,
	}
	if out.Sources == nil {
		// A JSON null here would read as "unknown"; an empty object reads as
		// "nothing has been billed yet", which is what it means.
		out.Sources = map[string]int64{}
	}
	truncated := truncatedByKey(obs)
	for _, d := range findings {
		out.Keys = append(out.Keys, agreementKey{
			Endpoint: d.Key, Ratio: d.Ratio, Percent: d.Percent, Fault: d.Fault,
			Reason: d.Reason, EngineRows: d.EngineN, CountedRows: d.CountedN,
			TruncatedRows: truncated[d.Key],
		})
		if d.Fault {
			out.Faults++
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// truncatedByKey recounts the capped answers per endpoint.
//
// Recomputed here rather than carried out of Verify because Verify groups into
// a Sample it then discards, and a second grouping inside the domain package
// would exist only to hand back a number the query already had. The
// observation list is bounded by one entry per request in the month, which is
// the same data the report above already read.
func truncatedByKey(obs []billing.Observation) map[string]int64 {
	out := map[string]int64{}
	for _, o := range obs {
		if o.Truncated {
			out[o.Key]++
		}
	}
	return out
}
