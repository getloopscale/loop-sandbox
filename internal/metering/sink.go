package metering

import (
	"context"
	"log/slog"
)

// LogSink is a MeterSink that logs each emitted batch. It is the default sink until
// the real LoopScale billing HTTP sink (cross-repo) is wired; it is idempotent by nature
// (logging the same EventID twice is harmless), satisfying the sink contract.
type LogSink struct{ logger *slog.Logger }

// NewLogSink returns a sink that logs batches at INFO.
func NewLogSink(logger *slog.Logger) *LogSink { return &LogSink{logger: logger} }

// Emit logs the batch (tenant/metric/amount per event) and its id range.
func (s *LogSink) Emit(_ context.Context, batch []MeterInput) error {
	if len(batch) == 0 {
		return nil
	}
	for _, m := range batch {
		s.logger.Info("meter.emit", "event_id", m.EventID, "tenant", m.TenantID,
			"metric", m.Metric, "amount", m.Amount, "period", m.Period)
	}
	return nil
}
