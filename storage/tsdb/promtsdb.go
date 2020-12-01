package tsdb

import (
	"context"
	"fmt"
	"strings"

	"github.com/nuclio/logger"
	"github.com/pkg/errors"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/v3io/v3io-tsdb/pkg/aggregate"
	tsdbAppender "github.com/v3io/v3io-tsdb/pkg/appender"
	"github.com/v3io/v3io-tsdb/pkg/config"
	"github.com/v3io/v3io-tsdb/pkg/pquerier"
	"github.com/v3io/v3io-tsdb/pkg/tsdb"
	"github.com/v3io/v3io-tsdb/pkg/utils"
)

type V3ioPromAdapter struct {
	db     *tsdb.V3ioAdapter
	logger logger.Logger

	useV3ioAggregations bool // Indicate whether or not to use v3io aggregations by default (passed from prometheus.yml)
}

func NewV3ioProm(cfg *config.V3ioConfig, logger logger.Logger) (*V3ioPromAdapter, error) {

	if logger == nil {
		newLogger, err := utils.NewLogger(cfg.LogLevel)
		if err != nil {
			return nil, errors.Wrap(err, "Unable to initialize logger.")
		}
		logger = newLogger
	}

	adapter, err := tsdb.NewV3ioAdapter(cfg, nil, logger)
	newAdapter := V3ioPromAdapter{db: adapter, logger: logger.GetChild("v3io-prom-adapter")}
	return &newAdapter, err
}

func (a *V3ioPromAdapter) SetUseV3ioAggregations(useV3ioAggregations bool) {
	a.useV3ioAggregations = useV3ioAggregations
}

func (a *V3ioPromAdapter) Appender() (storage.Appender, error) {
	err := a.db.InitAppenderCache()
	if err != nil {
		return nil, err
	}

	newAppender := v3ioAppender{metricsCache: a.db.MetricsCache}
	return newAppender, nil
}

func (a *V3ioPromAdapter) StartTime() (int64, error) {
	return a.db.StartTime()
}

func (a *V3ioPromAdapter) Close() error {
	return nil
}

func (a *V3ioPromAdapter) Querier(_ context.Context, mint, maxt int64) (storage.Querier, error) {
	v3ioQuerier, err := a.db.QuerierV2()
	promQuerier := V3ioPromQuerier{v3ioQuerier: v3ioQuerier,
		logger: a.logger.GetChild("v3io-prom-query"),
		mint:   mint, maxt: maxt,
		UseAggregatesConfig: a.useV3ioAggregations}
	return &promQuerier, err
}

type V3ioPromQuerier struct {
	v3ioQuerier *pquerier.V3ioQuerier
	logger      logger.Logger
	mint, maxt  int64

	UseAggregatesConfig    bool // Indicate whether or not to use v3io aggregations by default (passed from prometheus.yml)
	UseAggregates          bool // Indicate whether the current query is eligible for using v3io aggregations (should be set after creating a Querier instance)
	LastTSDBAggregatedAggr string
}

func (promQuery *V3ioPromQuerier) UseV3ioAggregations() bool {
	return promQuery.UseAggregates && promQuery.UseAggregatesConfig
}

func (promQuery *V3ioPromQuerier) IsAlreadyAggregated(op string) bool {
	if promQuery.UseV3ioAggregations() && promQuery.LastTSDBAggregatedAggr == op {
		return true
	}
	return false
}

// Select returns a set of series that matches the given label matchers.
func (promQuery *V3ioPromQuerier) Select(params *storage.SelectParams, oms ...*labels.Matcher) (storage.SeriesSet, storage.Warnings, error) {
	name, filter, function := match2filter(oms, promQuery.logger)
	noAggr := false

	// if a nil params is passed we assume it's a metadata query, so we fetch only the different labelsets withtout data.
	if params == nil {
		labelSets, err := promQuery.v3ioQuerier.GetLabelSets(name, filter)
		if err != nil {
			return nil, nil, err
		}

		return &V3ioPromSeriesSet{newMetadataSeriesSet(labelSets)}, nil, nil
	}

	promQuery.logger.Debug("SelectParams: %+v", params)
	overTimeSuffix := "_over_time"

	// Currently we do aggregations only for:
	// 1. All Cross-series aggregation
	// 2. Over-time aggregations where no Step or aggregationWindow was specified
	// 3. Over-time aggregation for v3io-tsdb compatible aggregates
	// 4. Down sampling - when only a step is provided
	// Note: in addition to the above cases, we also take into consider the `UseAggregatesConfig` configuration and of
	// course whether or not the requested aggregation is a valid v3io-tsdb aggregation
	if params.Func != "" {
		// only pass xx_over_time functions (just the xx part)
		// TODO: support count/stdxx, require changes in Prometheus: promql/functions.go, not calc aggregate twice
		if strings.HasSuffix(params.Func, overTimeSuffix) {
			if promQuery.UseV3ioAggregations() {
				function = strings.TrimSuffix(params.Func, overTimeSuffix)
			} else {
				f := params.Func[0:3]
				if params.Step == 0 && params.AggregationWindow == 0 &&
					(f == "min" || f == "max" || f == "sum" || f == "avg") {
					function = f
				} else {
					noAggr = true
				}
			}
		} else if promQuery.UseV3ioAggregations() {
			function = fmt.Sprintf("%v_all", params.Func)
		}
	}

	if function != "" && !noAggr {
		promQuery.LastTSDBAggregatedAggr = params.Func
	}

	// In case we can not do aggregations, make sure no step or aggregation window is passed.
	step,aggrWindow := params.Step, params.AggregationWindow
	if !promQuery.UseV3ioAggregations(){
		step = 0
		aggrWindow = 0
	}

	selectParams := &pquerier.SelectParams{Name: name,
		Functions:         function,
		Step:              step,
		Filter:            filter,
		From:              promQuery.mint,
		To:                promQuery.maxt,
		AggregationWindow: aggrWindow}

	promQuery.logger.DebugWith("Going to query tsdb", "params", selectParams,
		"UseAggregates", promQuery.UseAggregates, "UseAggregatesConfig", promQuery.UseAggregatesConfig)
	set, err := promQuery.v3ioQuerier.SelectProm(selectParams, noAggr)
	return &V3ioPromSeriesSet{s: set}, nil, err
}

// LabelValues returns all potential values for a label name.
func (promQuery *V3ioPromQuerier) LabelValues(name string) ([]string, storage.Warnings, error) {
	values, err := promQuery.v3ioQuerier.LabelValues(name)
	return values, nil, err
}

func (promQuery *V3ioPromQuerier) LabelNames() ([]string, storage.Warnings, error) {
	values, err := promQuery.v3ioQuerier.LabelNames()
	return values, nil, err
}

// Close releases the resources of the Querier.
func (promQuery *V3ioPromQuerier) Close() error {
	return nil
}

func match2filter(oms []*labels.Matcher, logger logger.Logger) (string, string, string) {
	var filter []string
	agg := ""
	name := ""

	for _, matcher := range oms {
		logger.Debug("Matcher: %+v", matcher)
		if matcher.Name == aggregate.AggregateLabel {
			agg = matcher.Value
		} else if matcher.Name == "__name__" && matcher.Type == labels.MatchEqual {
			name = matcher.Value
		} else {
			switch matcher.Type {
			case labels.MatchEqual:
				filter = append(filter, fmt.Sprintf("%s=='%s'", matcher.Name, matcher.Value))
			case labels.MatchNotEqual:
				filter = append(filter, fmt.Sprintf("%s!='%s'", matcher.Name, matcher.Value))
			case labels.MatchRegexp:
				filter = append(filter, fmt.Sprintf("regexp_instr(%s,'%s') == 0", matcher.Name, matcher.Value))
			case labels.MatchNotRegexp:
				filter = append(filter, fmt.Sprintf("regexp_instr(%s,'%s') != 0", matcher.Name, matcher.Value))

			}
		}
	}
	filterExp := strings.Join(filter, " and ")
	return name, filterExp, agg
}

type V3ioPromSeriesSet struct {
	s utils.SeriesSet
}

func (s *V3ioPromSeriesSet) Next() bool { return s.s.Next() }
func (s *V3ioPromSeriesSet) Err() error { return s.s.Err() }
func (s *V3ioPromSeriesSet) At() storage.Series {
	series := s.s.At()
	return &V3ioPromSeries{series}
}

// Series represents a single time series.
type V3ioPromSeries struct {
	s utils.Series
}

// Labels returns the complete set of labels identifying the series.
func (s *V3ioPromSeries) Labels() labels.Labels {
	lbls := labels.Labels{}
	for _, l := range s.s.Labels() {
		lbls = append(lbls, labels.Label{Name: l.Name, Value: l.Value})
	}

	return lbls
}

// Iterator returns a new iterator of the data of the series.
func (s *V3ioPromSeries) Iterator() storage.SeriesIterator {
	return &V3ioPromSeriesIterator{s: s.s.Iterator()}
}

// SeriesIterator iterates over the data of a time series.
type V3ioPromSeriesIterator struct {
	s utils.SeriesIterator
}

// Seek advances the iterator forward to the given timestamp.
// If there's no value exactly at t, it advances to the first value
// after t.
func (s *V3ioPromSeriesIterator) Seek(t int64) bool { return s.s.Seek(t) }

// Next advances the iterator by one.
func (s *V3ioPromSeriesIterator) Next() bool { return s.s.Next() }

// At returns the current timestamp/value pair.
func (s *V3ioPromSeriesIterator) At() (t int64, v float64) { return s.s.At() }

// error returns the current error.
func (s *V3ioPromSeriesIterator) Err() error { return s.s.Err() }

type v3ioAppender struct {
	metricsCache *tsdbAppender.MetricsCache
}

func (a v3ioAppender) Add(lset labels.Labels, t int64, v float64) (uint64, error) {
	lbls := Labels{lbls: &lset}
	return a.metricsCache.Add(lbls, t, v)
}

func (a v3ioAppender) AddFast(lset labels.Labels, ref uint64, t int64, v float64) error {
	err := a.metricsCache.AddFast(ref, t, v)
	if err != nil && strings.Contains(err.Error(), "metric not found") {
		return storage.ErrNotFound
	}
	return nil
}

func (a v3ioAppender) Commit() error   { return nil }
func (a v3ioAppender) Rollback() error { return nil }

type Labels struct {
	lbls *labels.Labels
}

func (ls Labels) String() string {
	return fmt.Sprint(ls.lbls)
}

// convert Label set to a string in the form key1=v1,key2=v2.. + name + hash
func (ls Labels) GetKey() (string, string, uint64) {
	var keyBuilder strings.Builder
	name := ""
	for _, lbl := range *ls.lbls {
		if lbl.Name == "__name__" {
			name = lbl.Value
		} else {
			keyBuilder.WriteString(lbl.Name)
			keyBuilder.WriteString("=")
			keyBuilder.WriteString(lbl.Value)
			keyBuilder.WriteString(",")
		}
	}
	if keyBuilder.Len() == 0 {
		return name, "", ls.lbls.Hash()
	}

	// Discard last comma
	key := keyBuilder.String()[:keyBuilder.Len()-1]

	return name, key, ls.lbls.Hash()

}


func (ls Labels) HashWithName() uint64 {
	return ls.lbls.Hash()
}

// create update expression
func (ls Labels) GetExpr() string {
	var lblExprBuilder strings.Builder
	for _, lbl := range *ls.lbls {
		if lbl.Name != "__name__" {
			fmt.Fprintf(&lblExprBuilder, "%s='%s'; ", lbl.Name, lbl.Value)
		} else {
			fmt.Fprintf(&lblExprBuilder, "_name='%s'; ", lbl.Value)
		}
	}

	return lblExprBuilder.String()
}

func (ls Labels) LabelNames() []string {
	res := make([]string, ls.lbls.Len())

	for i, l := range *ls.lbls {
		res[i] = l.Name
	}
	return res
}

func newMetadataSeriesSet(labels []utils.Labels) utils.SeriesSet {
	return &metadataSeriesSet{labels: labels, currentIndex: -1, size: len(labels)}
}

type metadataSeriesSet struct {
	labels       []utils.Labels
	currentIndex int
	size         int
}

func (ss *metadataSeriesSet) Next() bool {
	ss.currentIndex++
	return ss.currentIndex < ss.size
}
func (ss *metadataSeriesSet) At() utils.Series {
	return &metadataSeries{labels: ss.labels[ss.currentIndex]}
}
func (ss *metadataSeriesSet) Err() error {
	return nil
}

type metadataSeries struct {
	labels utils.Labels
}

func (s *metadataSeries) Labels() utils.Labels           { return s.labels }
func (s *metadataSeries) Iterator() utils.SeriesIterator { return utils.NullSeriesIterator{} }
func (s *metadataSeries) GetKey() uint64                 { return s.labels.Hash() }

func (ls Labels) Filter(keep []string) utils.LabelsIfc {
	var res labels.Labels
	for _, l := range *ls.lbls {
		for _, keepLabel := range keep {
			if l.Name == labels.MetricName || l.Name == keepLabel {
				res = append(res, l)
			}
		}
	}
	return Labels{lbls: &res}
}
