package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Header to set for API auth
const APIHDR = "X-Api-Key"

// Chunk size of metric requests
const CHUNKSIZE = 10

const NAMESPACE = "newrelic"

// This is to support skipping verification for testing and
// is deliberately not exposed to the user
var TLSIGNORE bool = false

type Metric struct {
	App   string
	Name  string
	Value float64
	Label string
}

type AppList struct {
	Applications []struct {
		Id         int
		Name       string
		Health     string             `json:"health_status"`
		AppSummary map[string]float64 `json:"application_summary"`
		UsrSummary map[string]float64 `json:"end_user_summary"`
	}
}

func (a *AppList) get(api newRelicApi) error {

	body, err := api.req("/v2/applications.json", "")
	if err != nil {
		return err
	}

	err = json.Unmarshal(body, a)
	return err

}

func (a *AppList) sendMetrics(ch chan<- Metric) {
	for _, app := range a.Applications {
		for name, value := range app.AppSummary {
			ch <- Metric{
				App:   app.Name,
				Name:  name,
				Value: value,
				Label: "application_summary",
			}
		}

		for name, value := range app.UsrSummary {
			ch <- Metric{
				App:   app.Name,
				Name:  name,
				Value: value,
				Label: "end_user_summary",
			}
		}
	}
}

type MetricNames struct {
	Metrics []struct {
		Name   string
		Values []string
	}
}

func (m *MetricNames) get(api newRelicApi, appId int) error {

	path := fmt.Sprintf("/v2/applications/%s/metrics.json", strconv.Itoa(appId))

	body, err := api.req(path, "")
	if err != nil {
		return err
	}

	return json.Unmarshal(body, m)
}

type MetricData struct {
	Metric_Data struct {
		Metrics []struct {
			Name       string
			Timeslices []struct {
				Values map[string]interface{}
			}
		}
	}
}

func (m *MetricData) get(api newRelicApi, appId int, names MetricNames) error {

	path := fmt.Sprintf("/v2/applications/%s/metrics/data.json", strconv.Itoa(appId))

	var nameList []string

	for i := range names.Metrics {
		// We urlencode the metric names as the API will return
		// unencoded names which it cannot read
		nameList = append(nameList, names.Metrics[i].Name)
	}

	// Because the Go client does not yet support 100-continue
	// ( see issue #3665 ),
	// we have to process this in chunks, to ensure the response
	// fits within a single request.

	for i := 0; i < len(nameList); i += CHUNKSIZE {

		var thisData MetricData
		var thisList []string

		if i+CHUNKSIZE > len(nameList) {
			thisList = nameList[i:]
		} else {
			thisList = nameList[i : i+CHUNKSIZE]
		}

		params := url.Values{}

		for _, thisName := range thisList {
			params.Add("names[]", thisName)
		}

		params.Add("raw", "true")
		params.Add("summarize", "true")
		params.Add("period", strconv.Itoa(api.period))
		params.Add("from", api.from.Format(time.RFC3339))
		params.Add("to", api.to.Format(time.RFC3339))

		body, err := api.req(path, params.Encode())
		if err != nil {
			return err
		}

		// We ignore unmarshal errors as we don't want to abort if a single
		// metric is broken, and there's currently no sensible way of
		// communicating useful information back to the user from here.
		json.Unmarshal(body, &thisData)

		allData := m.Metric_Data.Metrics
		allData = append(allData, thisData.Metric_Data.Metrics...)
		m.Metric_Data.Metrics = allData

	}

	return nil
}

func (m *MetricData) sendMetrics(ch chan<- Metric, app string) {
	for _, set := range m.Metric_Data.Metrics {

		if len(set.Timeslices) == 0 {
			continue
		}

		// As we set summarise=true there will only be one timeseries.
		for name, value := range set.Timeslices[0].Values {

			if v, ok := value.(float64); ok {

				ch <- Metric{
					App:   app,
					Name:  name,
					Value: v,
					Label: set.Name,
				}

			}
		}

	}
}

type Exporter struct {
	mu              sync.Mutex
	duration, error prometheus.Gauge
	totalScrapes    prometheus.Counter
	metrics         map[string]prometheus.GaugeVec
	api             newRelicApi
}

func NewExporter() *Exporter {
	return &Exporter{
		duration: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: NAMESPACE,
			Name:      "exporter_last_scrape_duration_seconds",
			Help:      "The last scrape duration.",
		}),
		totalScrapes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: NAMESPACE,
			Name:      "exporter_scrapes_total",
			Help:      "Total scraped metrics",
		}),
		error: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: NAMESPACE,
			Name:      "exporter_last_scrape_error",
			Help:      "The last scrape error status.",
		}),
		metrics: map[string]prometheus.GaugeVec{},
	}
}

func (e *Exporter) scrape(ch chan<- Metric) {

	e.error.Set(0)
	e.totalScrapes.Inc()

	now := time.Now().UnixNano()

	var apps AppList
	err := apps.get(e.api)
	if err != nil {
		e.error.Set(1)
	}

	apps.sendMetrics(ch)

	for _, app := range apps.Applications {

		var names MetricNames

		err = names.get(e.api, app.Id)
		if err != nil {
			e.error.Set(1)
		}

		var data MetricData

		err = data.get(e.api, app.Id, names)
		if err != nil {
			e.error.Set(1)
		}

		data.sendMetrics(ch, app.Name)

	}

	close(ch)
	e.duration.Set(float64(time.Now().UnixNano()-now) / 1000000000)
}

func (e *Exporter) recieve(ch <-chan Metric) {

	for metric := range ch {
		id := fmt.Sprintf("%s_%s_%s", NAMESPACE, metric.App, metric.Name)

		if m, ok := e.metrics[id]; ok {
			m.WithLabelValues(metric.Label).Set(metric.Value)
		} else {
			g := prometheus.NewGaugeVec(
				prometheus.GaugeOpts{
					Namespace: NAMESPACE,
					Subsystem: metric.App,
					Name:      metric.Name,
				},
				[]string{"component"})

			e.metrics[id] = *g
		}
	}
}

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, m := range e.metrics {
		m.Describe(ch)
	}

	ch <- e.duration.Desc()
	ch <- e.totalScrapes.Desc()
	ch <- e.error.Desc()
}

func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.api.to = time.Now().UTC()
	e.api.from = e.api.to.Add(-time.Duration(e.api.period) * time.Second)

	metricChan := make(chan Metric)

	go e.scrape(metricChan)

	e.recieve(metricChan)

	for _, m := range e.metrics {
		m.Collect(ch)
	}
}

type newRelicApi struct {
	server string
	apiKey string
	from   time.Time
	to     time.Time
	period int
}

func (a *newRelicApi) req(path string, data string) ([]byte, error) {

	u := fmt.Sprintf("%s%s?%s", a.server, path, data)

	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: TLSIGNORE,
			},
		},
	}

	req.Header.Set(APIHDR, a.apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Bad response code: %s", resp.Status)
	}

	return body, nil

}

func main() {

	exporter := NewExporter()

	var listenAddress, metricPath string

	flag.StringVar(&exporter.api.apiKey, "api.key", "", "NewRelic API key")
	flag.StringVar(&exporter.api.server, "api.server", "https://api.newrelic.com", "NewRelic API URL")
	flag.IntVar(&exporter.api.period, "api.period", 60, "Period of data to extract in seconds")

	flag.StringVar(&listenAddress, "web.listen-address", ":9104", "Address to listen on for web interface and telemetry.")
	flag.StringVar(&metricPath, "web.telemetry-path", "/metrics", "Path under which to expose metrics.")

	flag.Parse()

	prometheus.MustRegister(exporter)

	http.Handle(metricPath, prometheus.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
<head><title>NewRelic exporter</title></head>
<body>
<h1>NewRelic exporter</h1>
<p><a href='` + metricPath + `'>Metrics</a></p>
</body>
</html>
`))
	})

	http.ListenAndServe(listenAddress, nil)

}
