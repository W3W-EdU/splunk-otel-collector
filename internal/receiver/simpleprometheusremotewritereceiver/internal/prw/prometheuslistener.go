// Copyright Splunk, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prw

import (
	"context"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/prompb"
	"github.com/signalfx/gateway/protocol"
	"github.com/signalfx/golib/datapoint"
	"github.com/signalfx/golib/datapoint/dpsink"
	"github.com/signalfx/golib/log"
	"github.com/signalfx/golib/sfxclient"
)

// Server is the prometheus server
type Server struct {
	listener net.Listener
	//collector sfxclient.Collector
	decoder *decoder
	server  http.Server
	protocol.CloseableHealthCheck
}

type decoder struct {
	SendTo             dpsink.Sink
	Logger             log.Logger               // TODO Should go to zap logger in... obsreport?
	Bucket             *sfxclient.RollingBucket // TODO find the equivalent for otel
	DrainSize          *sfxclient.RollingBucket
	readAll            func(r io.Reader) ([]byte, error)
	TotalErrors        int64
	TotalNaNs          int64
	TotalBadDatapoints int64 // TODO send ot obsreport
}

func getDimensionsOrAttributesOrWhateverFromLabels(labels []prompb.Label) map[string]string {
	dims := make(map[string]string, len(labels))
	for _, l := range labels {
		dims[l.Name] = l.Value
	}
	return dims
}

func getMetricNameAndRemoveFromLabels(dims map[string]string) string {
	for k, v := range dims {
		if k == model.MetricNameLabel {
			delete(dims, k)
			return v
		}
	}
	return ""
}

// types are encoded into metric names for more information see below
// https://prometheus.io/docs/practices/naming/
// https://prometheus.io/docs/concepts/metric_types/
// https://prometheus.io/docs/instrumenting/writing_exporters/#metrics
// https://prometheus.io/docs/practices/histograms/
func getMetricType(metric string) datapoint.MetricType {

	// TODO hughesjj this should use the case when syntax we've been seeing in otel I think

	// _total is a convention for counters, you should use it if you’re using the COUNTER type.
	if strings.HasSuffix(metric, "_total") {
		return datapoint.Counter
	}
	// cumulative counters for the observation buckets, exposed as <basename>_bucket{le="<upper inclusive bound>"}
	if strings.HasSuffix(metric, "_bucket") {
		return datapoint.Counter
	}
	// the count of events that have been observed, exposed as <basename>_count
	if strings.HasSuffix(metric, "_count") {
		return datapoint.Counter
	}
	// _sum acts mostly like a counter, but can contain negative observations so must be sent in as a gauge
	// so everythign else is a gauge
	return datapoint.Gauge
}

func (d *decoder) getDatapoints(ts prompb.TimeSeries) []*datapoint.Datapoint {
	// TODO hughesjj Labels should be attributes
	// TODO hughesjj This should be changed to translate to pMetrics
	// TODO hughesjj Eh, honestly this is pretty specific to SFX
	dimensions := getDimensionsOrAttributesOrWhateverFromLabels(ts.Labels)
	metricName := getMetricNameAndRemoveFromLabels(dimensions)
	if metricName == "" {
		atomic.AddInt64(&d.TotalBadDatapoints, int64(len(ts.Samples)))
		return []*datapoint.Datapoint{}
	}
	metricType := getMetricType(metricName)

	dps := make([]*datapoint.Datapoint, 0, len(ts.Samples))
	for _, s := range ts.Samples {
		if math.IsNaN(s.Value) {
			atomic.AddInt64(&d.TotalNaNs, 1)
			continue
		}
		var value datapoint.Value
		if s.Value == float64(int64(s.Value)) {
			value = datapoint.NewIntValue(int64(s.Value))
		} else {
			value = datapoint.NewFloatValue(s.Value)
		}
		timestamp := time.Unix(0, int64(time.Millisecond)*s.Timestamp)
		dps = append(dps, datapoint.New(metricName, dimensions, value, metricType, timestamp))
	}
	return dps
}

// ServeHTTPC decodes datapoints for the connection and sends them to the decoder's sink
func (d *decoder) ServeHTTPC(ctx context.Context, rw http.ResponseWriter, req *http.Request) {

	// TODO This is going to be our ingest function prolly
	start := time.Now()
	defer d.Bucket.Add(float64(time.Since(start).Nanoseconds()))
	var err error
	var compressed []byte
	defer func() {
		if err != nil {
			atomic.AddInt64(&d.TotalErrors, 1)
			log.IfErr(d.Logger, err)
		}
	}()
	compressed, err = d.readAll(req.Body)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	var reqBuf []byte
	reqBuf, err = snappy.Decode(nil, compressed)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}

	var r prompb.WriteRequest
	if err = proto.Unmarshal(reqBuf, &r); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}

	dps := make([]*datapoint.Datapoint, 0, len(r.Timeseries))
	for _, ts := range r.Timeseries {
		datapoints := d.getDatapoints(ts)
		dps = append(dps, datapoints...)
	}

	d.DrainSize.Add(float64(len(dps)))
	if len(dps) > 0 {
		err = d.SendTo.AddDatapoints(ctx, dps)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
	}
}

// Datapoints about this decoder, including how many datapoints it decoded
func (d *decoder) Datapoints() []*datapoint.Datapoint {
	// TODO hughesjj More metadata to be sent to likely obsreport
	dps := d.Bucket.Datapoints()
	dps = append(dps, d.DrainSize.Datapoints()...)
	dps = append(dps,
		sfxclient.Cumulative("prometheus.invalid_requests", nil, atomic.LoadInt64(&d.TotalErrors)),
		sfxclient.Cumulative("prometheus.total_NAN_samples", nil, atomic.LoadInt64(&d.TotalNaNs)),
		sfxclient.Cumulative("prometheus.total_bad_datapoints", nil, atomic.LoadInt64(&d.TotalBadDatapoints)),
	)
	return dps
}