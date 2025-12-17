package stats

import (
	"expvar"
	"html/template"
	"log"
	"net/http"
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

var (
	statsTemplate *template.Template
)

func init() {
	var err error
	statsTemplate, err = template.New("stats").Parse(statsPage)
	if err != nil {
		log.Fatalf("Failed to parse stats template: %v", err)
	}
}

func serveStats(w http.ResponseWriter, _ *http.Request, backendID, proxyURL string) {
	var responseCodes []responseCode
	if v := expvar.Get("response_codes"); v != nil {
		if responseCodesVar, ok := v.(*expvar.Map); ok {
			responseCodesVar.Do(func(kv expvar.KeyValue) {
				responseCodes = append(responseCodes, responseCode{Code: kv.Key, Count: kv.Value.String()})
			})
		}
	}

	// Get percentiles from expvar (these persist across sample periods)
	responseTimesVar := expvar.Get("response_times").(*expvar.Map)
	responseTimes := make(map[string]string)
	responseTimesVar.Do(func(kv expvar.KeyValue) {
		responseTimes[kv.Key] = kv.Value.String()
	})

	data := statsData{
		BackendID:     backendID,
		ProxyURL:      proxyURL,
		ResponseCodes: responseCodes,
		ResponseTimes: responseTimes,
	}

	if err := statsTemplate.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// Start a server on the given address that will respond to any request with a stats page.
func Start(address, backendID, proxyURL string) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		serveStats(w, r, backendID, proxyURL)
	})
	log.Printf("Stats server listening on %s", address)
	if err := http.ListenAndServe(address, nil); err != nil {
		log.Fatalf("Stats server failed: %v", err)
	}
}
