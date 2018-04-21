package main

// TODO: implement JSON parser that loops through the output from api.Get()

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/log"
)

// ChunkSize of metric requests
const ChunkSize = 10

// Namespace for metrics
const Namespace = "newrelic"

// UserAgent string
const UserAgent = "NewRelic Exporter"

// Regular expression to parse Link headers
var rexp = `<([[:graph:]]+)>; rel="next"`
var linkRegexp *regexp.Regexp

func init() {
	linkRegexp = regexp.MustCompile(rexp)
}

type metric struct {
	App   string
	Name  string
	Value float64
	Label string
}

type appList struct {
	Applications []struct {
		ID         int
		Name       string
		Health     string             `json:"health_status"`
		AppSummary map[string]float64 `json:"application_summary"`
		UsrSummary map[string]float64 `json:"end_user_summary"`
	}
}

func (a *appList) get(api *newRelicAPI) error {
	log.Debugf("Requesting application list from %s.", api.server.String())
	body, err := api.req("/v2/applications.json", "")
	if err != nil {
		log.Error("Error getting application list: ", err)
		return err
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	for {

		page := new(appList)
		if err := dec.Decode(page); err == io.EOF {
			break
		} else if err != nil {
			log.Error("Error decoding application list: ", err)
			return err
		}

		a.Applications = append(a.Applications, page.Applications...)

	}

	return nil
}

func (a *appList) sendMetrics(ch chan<- metric) {
	for _, app := range a.Applications {
		for name, value := range app.AppSummary {
			ch <- metric{
				App:   app.Name,
				Name:  name,
				Value: value,
				Label: "application_summary",
			}
		}

		for name, value := range app.UsrSummary {
			ch <- metric{
				App:   app.Name,
				Name:  name,
				Value: value,
				Label: "end_user_summary",
			}
		}
	}
}

type metricNames struct {
	Metrics []struct {
		Name   string
		Values []string
	}
}

func (m *metricNames) get(api *newRelicAPI, appID int) error {
	log.Debugf("Requesting metrics names for application id %d.", appID)
	path := fmt.Sprintf("/v2/applications/%s/metrics.json", strconv.Itoa(appID))

	body, err := api.req(path, "")
	if err != nil {
		log.Error("Error getting metric names: ", err)
		return err
	}

	dec := json.NewDecoder(bytes.NewReader(body))

	for {
		var part metricNames
		if err = dec.Decode(&part); err == io.EOF {
			break
		} else if err != nil {
			log.Error("Error decoding metric names: ", err)
			return err
		}
		tmpMetrics := append(m.Metrics, part.Metrics...)
		m.Metrics = tmpMetrics
	}

	return nil
}

type metricDataResp struct {
	MetricData metricData `json:"metric_data"`
}

type metricData struct {
	Metrics []struct {
		Name       string
		Timeslices []struct {
			Values map[string]interface{}
		}
	}
}

func (m *metricDataResp) get(api *newRelicAPI, appID int, names metricNames) error {
	path := fmt.Sprintf("/v2/applications/%s/metrics/data.json", strconv.Itoa(appID))

	var nameList []string

	for i := range names.Metrics {
		// We urlencode the metric names as the API will return
		// unencoded names which it cannot read
		nameList = append(nameList, names.Metrics[i].Name)
	}
	log.Debugf("Requesting %d metrics for application id %d.", len(nameList), appID)

	// Because the Go client does not yet support 100-continue
	// ( see issue #3665 ),
	// we have to process this in chunks, to ensure the response
	// fits within a single request.

	chans := make([]chan metricDataResp, 0)

	for i := 0; i < len(nameList); i += ChunkSize {

		chans = append(chans, make(chan metricDataResp))

		var thisList []string

		if i+ChunkSize > len(nameList) {
			thisList = nameList[i:]
		} else {
			thisList = nameList[i : i+ChunkSize]
		}

		go func(names []string, ch chan<- metricDataResp) {

			var data metricDataResp

			params := url.Values{}

			for _, thisName := range names {
				params.Add("names[]", thisName)
			}

			params.Add("raw", "true")
			params.Add("summarize", "true")
			params.Add("period", strconv.Itoa(api.period))
			params.Add("from", api.from.Format(time.RFC3339))
			params.Add("to", api.to.Format(time.RFC3339))

			body, err := api.req(path, params.Encode())
			if err != nil {
				log.Error("Error requesting metrics: ", err)
				close(ch)
				return
			}

			dec := json.NewDecoder(bytes.NewReader(body))
			for {

				page := new(metricDataResp)
				if err := dec.Decode(page); err == io.EOF {
					break
				} else if err != nil {
					log.Error("Error decoding metrics data: ", err)
					close(ch)
					return
				}

				data.MetricData.Metrics = append(data.MetricData.Metrics, page.MetricData.Metrics...)

			}

			ch <- data
			close(ch)

		}(thisList, chans[len(chans)-1])

	}

	allData := m.MetricData.Metrics

	for _, ch := range chans {
		m := <-ch
		allData = append(allData, m.MetricData.Metrics...)
	}
	m.MetricData.Metrics = allData

	return nil
}

func (m *metricDataResp) sendMetrics(ch chan<- metric, app string) {
	for _, set := range m.MetricData.Metrics {

		if len(set.Timeslices) == 0 {
			continue
		}

		// As we set summarise=true there will only be one timeseries.
		for name, value := range set.Timeslices[0].Values {

			if v, ok := value.(float64); ok {

				ch <- metric{
					App:   app,
					Name:  name,
					Value: v,
					Label: set.Name,
				}

			}
		}

	}
}

type exporter struct {
	mu              sync.Mutex
	duration, error prometheus.Gauge
	totalScrapes    prometheus.Counter
	metrics         map[string]prometheus.GaugeVec
	api             *newRelicAPI
}

func newExporter() *exporter {
	return &exporter{
		duration: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: Namespace,
			Name:      "exporter_last_scrape_duration_seconds",
			Help:      "The last scrape duration.",
		}),
		totalScrapes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "exporter_scrapes_total",
			Help:      "Total scraped metrics",
		}),
		error: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: Namespace,
			Name:      "exporter_last_scrape_error",
			Help:      "The last scrape error status.",
		}),
		metrics: map[string]prometheus.GaugeVec{},
	}
}

func (e *exporter) scrape(ch chan<- metric) {

	e.error.Set(0)
	e.totalScrapes.Inc()

	now := time.Now().UnixNano()
	log.Debugf("Starting new scrape at %d.", now)

	var apps appList
	err := apps.get(e.api)
	if err != nil {
		log.Error(err)
		e.error.Set(1)
	}

	apps.sendMetrics(ch)

	var wg sync.WaitGroup

	for i := range apps.Applications {

		app := apps.Applications[i]

		wg.Add(1)
		api := e.api

		go func() {

			defer wg.Done()
			var names metricNames

			err = names.get(api, app.ID)
			if err != nil {
				log.Error(err)
				e.error.Set(1)
			}

			var data metricDataResp

			err = data.get(api, app.ID, names)
			if err != nil {
				log.Error(err)
				e.error.Set(1)
			}

			data.sendMetrics(ch, app.Name)

		}()

	}

	wg.Wait()

	close(ch)
	e.duration.Set(float64(time.Now().UnixNano()-now) / 1000000000)
}

func (e *exporter) recieve(ch <-chan metric) {

	for metric := range ch {
		id := fmt.Sprintf("%s_%s", Namespace, metric.Name)

		if m, ok := e.metrics[id]; ok {
			m.WithLabelValues(metric.App, metric.Label).Set(metric.Value)
		} else {
			g := prometheus.NewGaugeVec(
				prometheus.GaugeOpts{
					Namespace: Namespace,
					Name:      metric.Name,
				},
				[]string{"app", "component"})

			e.metrics[id] = *g
		}
	}
}

func (e *exporter) Describe(ch chan<- *prometheus.Desc) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, m := range e.metrics {
		m.Describe(ch)
	}

	ch <- e.duration.Desc()
	ch <- e.totalScrapes.Desc()
	ch <- e.error.Desc()
}

func (e *exporter) Collect(ch chan<- prometheus.Metric) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Align requests to minute boundary.
	// As time.Round rounds to the nearest integar rather than floor or ceil,
	// subtract 30 seconds from the time before rounding.
	e.api.to = time.Now().Add(-time.Second * 30).Round(time.Minute).UTC()
	e.api.from = e.api.to.Add(-time.Duration(e.api.period) * time.Second)

	metricChan := make(chan metric)

	go e.scrape(metricChan)

	e.recieve(metricChan)

	ch <- e.duration
	ch <- e.totalScrapes
	ch <- e.error

	for _, m := range e.metrics {
		m.Collect(ch)
	}

}

type newRelicAPI struct {
	server          url.URL
	apiKey          string
	from            time.Time
	to              time.Time
	period          int
	unreportingApps bool
	client          *http.Client
}

func newNewRelicAPI(server string, apikey string, timeout time.Duration) *newRelicAPI {
	parsed, err := url.Parse(server)
	if err != nil {
		log.Fatal("Could not parse API URL: ", err)
	}
	if apikey == "" {
		log.Fatal("Cannot continue without an API key.")
	}
	return &newRelicAPI{
		server: *parsed,
		apiKey: apikey,
		client: &http.Client{Timeout: timeout},
	}
}

func (a *newRelicAPI) req(path string, params string) ([]byte, error) {

	u, err := url.Parse(a.server.String() + path)
	if err != nil {
		return nil, err
	}
	u.RawQuery = params

	log.Debug("Making API call: ", u.String())

	req := &http.Request{
		Method: "GET",
		URL:    u,
		Header: http.Header{
			"User-Agent": {UserAgent},
			"X-Api-Key":  {a.apiKey},
		},
	}

	var data []byte

	return a.httpget(req, data)
}

func (a *newRelicAPI) httpget(req *http.Request, in []byte) (out []byte, err error) {
	resp, err := a.client.Do(req)
	if err != nil {
		return
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return
	}
	resp.Body.Close()
	out = append(in, body...)

	// Read the link header to see if we need to read more pages.
	link := resp.Header.Get("Link")
	vals := linkRegexp.FindStringSubmatch(link)

	if len(vals) == 2 {
		// Regexp matched, read get next page

		u := new(url.URL)

		u, err = url.Parse(vals[1])
		if err != nil {
			return
		}
		req.URL = u
		return a.httpget(req, out)
	}
	return
}

// Entry point
func main() {
	var server, apikey, listenAddress, metricPath string
	var period int
	var timeout time.Duration
	var err error

	flag.StringVar(&apikey, "api.key", "", "NewRelic API key")
	flag.StringVar(&server, "api.server", "https://api.newrelic.com", "NewRelic API URL")
	flag.IntVar(&period, "api.period", 60, "Period of data to extract in seconds")
	flag.DurationVar(&timeout, "api.timeout", 5*time.Second, "Period of time to wait for an API response in seconds")

	flag.StringVar(&listenAddress, "web.listen-address", ":9126", "Address to listen on for web interface and telemetry.")
	flag.StringVar(&metricPath, "web.telemetry-path", "/metrics", "Path under which to expose metrics.")

	flag.Parse()

	api := newNewRelicAPI(server, apikey, timeout)
	api.period = period
	exporter := newExporter()
	exporter.api = api

	prometheus.MustRegister(exporter)

	http.Handle(metricPath, prometheus.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>` +
			`<head><title>NewRelic exporter</title></head>` +
			`<body>` +
			`<h1>NewRelic exporter</h1>` +
			`<p><a href='` + metricPath + `'>Metrics</a></p>` +
			`</body>` +
			`</html>`))
	})

	log.Printf("Listening on %s.", listenAddress)
	err = http.ListenAndServe(listenAddress, nil)
	if err != nil {
		log.Fatal(err)
	}
	log.Print("HTTP server stopped.")
}
