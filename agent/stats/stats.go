package stats

import (
	"expvar"
	"fmt"
	"html/template"
	"net/http"

	"github.com/google/inverting-proxy/agent/metrics"
)

const statsPage = `
<!DOCTYPE html>
<html>
<head>
	<title>Inverting Proxy Agent Stats</title>
	<style>
		body {
			font-family: sans-serif;
		}
		table {
			border-collapse: collapse;
			margin-top: 1em;
		}
		th, td {
			border: 1px solid #ccc;
			padding: 0.5em;
		}
		th {
			background-color: #eee;
		}
	</style>
</head>
<body>
	<h1>Inverting Proxy Agent Stats</h1>

	<p><b>Backend ID:</b> {{.BackendID}}</p>
	<p><b>Proxy URL:</b> {{.ProxyURL}}</p>

	<h2>Response Codes</h2>
	<table>
		<tr>
			<th>Code</th>
			<th>Count</th>
		</tr>
		{{range .ResponseCodes}}
		<tr>
			<td>{{.Code}}</td>
			<td>{{.Count}}</td>
		</tr>
		{{end}}
	</table>

	<h2>Response Times (ms)</h2>
	<table>
		<tr>
			<th>Percentile</th>
			<th>Time (ms)</th>
		</tr>
		<tr>
			<td>p50</td>
			<td>{{index .ResponseTimes "p50"}}</td>
		</tr>
		<tr>
			<td>p90</td>
			<td>{{index .ResponseTimes "p90"}}</td>
		</tr>
		<tr>
			<td>p99</td>
			<td>{{index .ResponseTimes "p99"}}</td>
		</tr>
	</table>
</body>
</html>
`

type responseCode struct {
	Code  string
	Count string
}

type statsData struct {
	BackendID     string
	ProxyURL      string
	ResponseCodes []responseCode
	ResponseTimes map[string]string
}

func formatFloat(f float64) string {
	return fmt.Sprintf("%.2f", f)
}

func serveStats(w http.ResponseWriter, _ *http.Request, backendID, proxyURL string) {
	responseCodesVar := expvar.Get("response_codes").(*expvar.Map)

	var responseCodes []responseCode
	responseCodesVar.Do(func(kv expvar.KeyValue) {
		responseCodes = append(responseCodes, responseCode{Code: kv.Key, Count: kv.Value.String()})
	})

	// Get current percentiles on-demand
	percentiles := metrics.GetCurrentPercentiles()
	responseTimes := make(map[string]string)
	responseTimes["p50"] = formatFloat(percentiles["p50"])
	responseTimes["p90"] = formatFloat(percentiles["p90"])
	responseTimes["p99"] = formatFloat(percentiles["p99"])

	data := statsData{
		BackendID:     backendID,
		ProxyURL:      proxyURL,
		ResponseCodes: responseCodes,
		ResponseTimes: responseTimes,
	}

	tmpl, err := template.New("stats").Parse(statsPage)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// Start a server on the given address that will respond to any request with a stats page.
func Start(address, backendID, proxyURL string) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		serveStats(w, r, backendID, proxyURL)
	})
	http.ListenAndServe(address, nil)
}
