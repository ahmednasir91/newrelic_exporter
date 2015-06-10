package main

import (
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"testing"
)

var testApiKey string = "205071e37e95bdaa327c62ccd3201da9289ccd17"
var testApiAppId int = 9045822
var testPostData string = "names[]=Datastore%2Fstatement%2FJDBC%2Fmessages%2Finsert&raw=true&summarize=true&period=0&from=0001-01-01T00:00:00Z&to=0001-01-01T00:00:00Z"

func TestAppListGet(t *testing.T) {

	ts, err := testServer()
	if err != nil {
		t.Fatal(err)
	}

	defer ts.Close()

	var api newRelicApi

	api.server = ts.URL
	api.apiKey = testApiKey

	var app AppList

	err = app.get(api)
	if err != nil {
		t.Fatal(err)
	}

	if len(app.Applications) != 1 {
		t.Fatal("Expected 1 application")
	}

	a := app.Applications[0]

	switch {

	case a.Id != testApiAppId:
		t.Fatal("Wrong ID")

	case a.Health != "green":
		t.Fatal("Wrong health status")

	case a.Name != "Test/Client/Name":
		t.Fatal("Wrong name")

	case a.AppSummary["throughput"] != 54.7:
		t.Fatal("Wrong throughput")

	case a.AppSummary["host_count"] != 3:
		t.Fatal("Wrong host count")

	case a.UsrSummary["response_time"] != 4.61:
		t.Fatal("Wrong response time")

	}

}

func TestMetricNamesGet(t *testing.T) {

	ts, err := testServer()
	if err != nil {
		t.Fatal(err)
	}

	defer ts.Close()

	var api newRelicApi

	api.server = ts.URL
	api.apiKey = testApiKey

	var names MetricNames

	err = names.get(api, testApiAppId)
	if err != nil {
		t.Fatal(err)
	}

	if len(names.Metrics) != 1 {
		t.Fatal("Expected one name set")
	}

	if len(names.Metrics[0].Values) != 10 {
		t.Fatal("Expected 10 metric names")
	}

	if names.Metrics[0].Name != "Datastore/statement/JDBC/messages/insert" {
		t.Fatal("Wrong application name")
	}

}

func TestMetricValuesGet(t *testing.T) {

	ts, err := testServer()
	if err != nil {
		t.Fatal(err)
	}

	defer ts.Close()

	var api newRelicApi

	api.server = ts.URL
	api.apiKey = testApiKey

	var data MetricData
	var names MetricNames

	err = names.get(api, testApiAppId)
	if err != nil {
		t.Fatal(err)
	}

	err = data.get(api, testApiAppId, names)
	if err != nil {
		t.Fatal(err)
	}

	if len(data.Metric_Data.Metrics) != 1 {
		t.Fatal("Expected 1 metric set")
	}

	if len(data.Metric_Data.Metrics[0].Timeslices) != 1 {
		t.Fatal("Expected 1 timeslice")
	}

	appData := data.Metric_Data.Metrics[0].Timeslices[0]

	if len(appData.Values) != 10 {
		t.Fatal("Expected 10 data points")
	}

	if appData.Values["call_count"].(float64) != 2 {
		t.Fatal("Wrong call_count value")
	}

	if appData.Values["calls_per_minute"].(float64) != 2.03 {
		t.Fatal("Wrong calls_per_minute value")
	}

}

func TestScrapeAPI(t *testing.T) {

	ts, err := testServer()
	if err != nil {
		t.Fatal(err)
	}

	defer ts.Close()

	exporter := NewExporter()

	var recieved []Metric

	exporter.api.server = ts.URL
	exporter.api.apiKey = testApiKey

	metrics := make(chan Metric)

	go exporter.scrape(metrics)

	for m := range metrics {
		recieved = append(recieved, m)
	}

	if len(recieved) != 21 {
		t.Fatal("Expected 21 metrics")
	}

}

func testServer() (ts *httptest.Server, err error) {
	TLSIGNORE = true

	ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		if r.Header.Get(APIHDR) != testApiKey {
			w.WriteHeader(403)
		}

		var body []byte

		switch r.URL.Path {

		case "/v2/applications.json":
			body, err = ioutil.ReadFile("_testing/application_list.json")
			if err != nil {
				return
			}

		case "/v2/applications/9045822/metrics.json":
			body, err = ioutil.ReadFile("_testing/metric_names.json")
			if err != nil {
				return
			}

		case "/v2/applications/9045822/metrics/data.json":
			body, err = ioutil.ReadFile("_testing/metric_data.json")
			if err != nil {
				return
			}

		default:
			w.WriteHeader(404)
			return

		}

		w.WriteHeader(200)
		w.Write(body)

	}))

	return
}
