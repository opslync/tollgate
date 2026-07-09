package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/opslync/tollgate/internal/proxy"
)

const (
	bufferCapacity = 1000
	batchMaxSpans  = 50
	batchInterval  = 2 * time.Second
	postTimeout    = 5 * time.Second
)

type spanJob struct {
	rec     proxy.RequestRecord
	costUSD float64
}

// Exporter batches OTLP/HTTP+JSON spans and POSTs them to a collector.
// Export never blocks the request goroutine: a full buffer drops the span
// and logs a warning, matching how usage-parse failures never break the
// proxy elsewhere in this codebase.
type Exporter struct {
	endpoint string
	client   *http.Client
	logger   *slog.Logger
	jobs     chan spanJob
}

func NewExporter(endpoint string, logger *slog.Logger) *Exporter {
	return &Exporter{
		endpoint: endpoint,
		client:   &http.Client{Timeout: postTimeout},
		logger:   logger,
		jobs:     make(chan spanJob, bufferCapacity),
	}
}

// Export queues one request's span for background export.
func (e *Exporter) Export(rec proxy.RequestRecord, costUSD float64) {
	select {
	case e.jobs <- spanJob{rec: rec, costUSD: costUSD}:
	default:
		e.logger.Warn("trace export buffer full; dropping span")
	}
}

// Run batches queued spans — by count or by interval, whichever comes first
// — and POSTs each batch, flushing whatever remains when ctx is cancelled.
func (e *Exporter) Run(ctx context.Context) {
	batch := make([]spanJob, 0, batchMaxSpans)
	t := time.NewTicker(batchInterval)
	defer t.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		e.send(batch)
		batch = batch[:0]
	}
	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case job := <-e.jobs:
			batch = append(batch, job)
			if len(batch) >= batchMaxSpans {
				flush()
			}
		case <-t.C:
			flush()
		}
	}
}

// send POSTs one batch, best-effort: failures are logged and the batch is
// dropped, never retried. Runs from Run's loop, never on the request
// goroutine.
func (e *Exporter) send(batch []spanJob) {
	spans := make([]span, 0, len(batch))
	for _, j := range batch {
		s, err := newSpan(j.rec, j.costUSD)
		if err != nil {
			e.logger.Warn("build span failed; dropping", "error", err)
			continue
		}
		spans = append(spans, s)
	}
	if len(spans) == 0 {
		return
	}

	body, err := json.Marshal(wrapSpans(spans))
	if err != nil {
		e.logger.Warn("marshal span batch failed; dropping", "error", err, "spans", len(spans))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), postTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		e.logger.Warn("build otlp request failed; dropping batch", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		e.logger.Warn("otlp export failed; dropping batch", "error", err, "spans", len(spans))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		e.logger.Warn("otlp collector rejected batch", "status", resp.StatusCode, "spans", len(spans))
	}
}
