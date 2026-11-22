// Package prommerge scrapes several Prometheus text-format endpoints and merges
// them into a single exposition, adding per-target labels to every series.
//
// Parsing and encoding use the official github.com/prometheus/common/expfmt
// implementation, so escaping, special float values (NaN, ±Inf), timestamps,
// histograms and summaries are handled the same way the Prometheus server does.
package prommerge

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	log "github.com/sirupsen/logrus"
)

const (
	// DefaultConcurrency is the number of targets scraped in parallel when Async is enabled.
	DefaultConcurrency = 100
	// DefaultMaxResponseBytes caps a single target response to protect the wrapper's memory.
	DefaultMaxResponseBytes int64 = 64 << 20
	// DefaultTimeout is used when no HTTP client is provided.
	DefaultTimeout = 30 * time.Second

	acceptHeader = `text/plain;version=0.0.4;q=1,*/*;q=0.1`
)

// ContentType is the Content-Type of the merged output.
var ContentType = string(expfmt.NewFormat(expfmt.TypeTextPlain))

// Target is a single metrics endpoint.
type Target struct {
	Name string
	URL  string
	// Processes gate the target: it is scraped while at least one of them is
	// ready. Empty means the process named Name.
	Processes []string
	// ExtraLabels are added to every series of the target and override labels with the same name.
	ExtraLabels map[string]string
}

// Options configures a Merger.
type Options struct {
	// EmptyOnFailure makes Collect return no metrics if any target fails.
	EmptyOnFailure bool
	// Async scrapes targets concurrently (up to DefaultConcurrency at a time).
	Async bool
	// Sort orders series within each metric family by their labels.
	// Metric families are always ordered by name.
	Sort bool
	// OmitMeta drops HELP strings from the output. TYPE is always kept, because
	// it is required to interpret histograms and summaries correctly.
	OmitMeta bool
	// SuppressErrors disables logging of individual target failures.
	SuppressErrors bool
	// HTTPClient is used to scrape targets. A client with DefaultTimeout is used if nil.
	HTTPClient *http.Client
	// MaxResponseBytes limits a single target response size. DefaultMaxResponseBytes if <= 0.
	MaxResponseBytes int64
}

// Merger scrapes and merges targets. It is safe for concurrent use.
type Merger struct {
	opts Options
}

// New creates a Merger.
func New(opts Options) *Merger {
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: DefaultTimeout}
	}
	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = DefaultMaxResponseBytes
	}
	return &Merger{opts: opts}
}

type scrapeResult struct {
	families map[string]*dto.MetricFamily
	err      error
}

// Collect scrapes all targets and returns the merged metric families ordered by name.
// Failed targets are skipped unless EmptyOnFailure is set, in which case the first
// failure is returned together with a nil result.
func (m *Merger) Collect(ctx context.Context, targets []Target) ([]*dto.MetricFamily, error) {
	results := make([]scrapeResult, len(targets))

	concurrency := 1
	if m.opts.Async {
		concurrency = DefaultConcurrency
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer func() {
				<-sem
				wg.Done()
			}()
			families, err := m.scrape(ctx, targets[i])
			results[i] = scrapeResult{families: families, err: err}
		}(i)
	}
	wg.Wait()

	merged := make(map[string]*dto.MetricFamily)
	var errs []error
	for i, r := range results {
		if r.err != nil {
			err := fmt.Errorf("target %q: %w", targets[i].Name, r.err)
			if !m.opts.SuppressErrors {
				log.WithFields(log.Fields{"name": targets[i].Name, "url": targets[i].URL}).Warnf("Prommerge target failed, %v", r.err)
			}
			if m.opts.EmptyOnFailure {
				return nil, err
			}
			errs = append(errs, err)
			continue
		}
		m.mergeInto(merged, r.families, targets[i])
	}

	out := make([]*dto.MetricFamily, 0, len(merged))
	for _, mf := range merged {
		if m.opts.Sort {
			sortMetrics(mf)
		}
		out = append(out, mf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out, errors.Join(errs...)
}

func (m *Merger) scrape(ctx context.Context, t Target) (map[string]*dto.MetricFamily, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", acceptHeader)

	resp, err := m.opts.HTTPClient.Do(req) // #nosec G704 -- targets come from the operator's config
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("unexpected status code %d", resp.StatusCode)
	}
	// We ask for the text format; anything that is explicitly something else
	// (protobuf, OpenMetrics) can't be parsed by the text parser.
	switch expfmt.ResponseFormat(resp.Header).FormatType() {
	case expfmt.TypeTextPlain, expfmt.TypeUnknown:
	default:
		return nil, fmt.Errorf("unsupported content type %q", resp.Header.Get("Content-Type"))
	}

	body := &limitedReader{r: resp.Body, n: m.opts.MaxResponseBytes}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(bufio.NewReader(body))
	if body.exceeded {
		return nil, fmt.Errorf("response exceeds %d bytes", m.opts.MaxResponseBytes)
	}
	if err != nil {
		return nil, fmt.Errorf("parse metrics: %w", err)
	}
	return families, nil
}

func (m *Merger) mergeInto(merged, families map[string]*dto.MetricFamily, t Target) {
	for name, mf := range families {
		if len(mf.Metric) == 0 {
			continue
		}
		for _, metric := range mf.Metric {
			setLabels(metric, t.ExtraLabels)
		}
		if m.opts.OmitMeta {
			mf.Help = nil
		}
		existing, ok := merged[name]
		if !ok {
			merged[name] = mf
			continue
		}
		if existing.GetType() != mf.GetType() {
			if !m.opts.SuppressErrors {
				log.WithFields(log.Fields{"name": t.Name, "metric": name}).Warnf(
					"Prommerge metric type conflict: %v vs %v, metric skipped", existing.GetType(), mf.GetType())
			}
			continue
		}
		if existing.Help == nil && mf.Help != nil {
			existing.Help = mf.Help
		}
		existing.Metric = append(existing.Metric, mf.Metric...)
	}
}

// setLabels adds or overrides labels on a metric.
func setLabels(metric *dto.Metric, labels map[string]string) {
	for name, value := range labels {
		replaced := false
		for _, lp := range metric.Label {
			if lp.GetName() == name {
				lp.Value = new(value)
				replaced = true
				break
			}
		}
		if !replaced {
			metric.Label = append(metric.Label, &dto.LabelPair{Name: new(name), Value: new(value)})
		}
	}
}

func sortMetrics(mf *dto.MetricFamily) {
	for _, metric := range mf.Metric {
		sort.Slice(metric.Label, func(i, j int) bool { return metric.Label[i].GetName() < metric.Label[j].GetName() })
	}
	sort.SliceStable(mf.Metric, func(i, j int) bool {
		return labelsKey(mf.Metric[i]) < labelsKey(mf.Metric[j])
	})
}

func labelsKey(metric *dto.Metric) string {
	var b strings.Builder
	for _, lp := range metric.Label {
		b.WriteString(lp.GetName())
		b.WriteByte(0xff)
		b.WriteString(lp.GetValue())
		b.WriteByte(0xff)
	}
	return b.String()
}

// Write encodes metric families in the Prometheus text format.
func Write(w io.Writer, families []*dto.MetricFamily) error {
	bw := bufio.NewWriter(w)
	for _, mf := range families {
		if _, err := expfmt.MetricFamilyToText(bw, mf); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// limitedReader is like io.LimitedReader but remembers whether the limit was hit.
type limitedReader struct {
	r        io.Reader
	n        int64
	exceeded bool
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		// Probe for more data to distinguish "exactly at limit" from "too big".
		var one [1]byte
		if n, _ := l.r.Read(one[:]); n > 0 {
			l.exceeded = true
			return 0, errors.New("response too large")
		}
		return 0, io.EOF
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	return n, err
}
