package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/kong"
	"github.com/cespare/xxhash/v2"
	"github.com/dgraph-io/ristretto"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
	"github.com/prometheus/client_golang/api"
	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	promconfig "github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/pyrra-dev/pyrra/openapi"
	openapiclient "github.com/pyrra-dev/pyrra/openapi/client"
	openapiserver "github.com/pyrra-dev/pyrra/openapi/server/go"
)

//go:embed ui/build
var ui embed.FS

var CLI struct {
	API struct {
		PrometheusURL             *url.URL `default:"http://localhost:9090" help:"The URL to the Prometheus to query."`
		PrometheusExternalURL     *url.URL `help:"The URL for the UI to redirect users to when opening Prometheus. If empty the same as prometheus.url"`
		ApiURL                    *url.URL `default:"http://localhost:9444" help:"The URL to the API service like a Kubernetes Operator."`
		RoutePrefix               string   `default:"" help:"The route prefix Pyrra uses. If run behind a proxy you can change it to something like /pyrra here."`
		UIRoutePrefix             string   `default:"" help:"The route prefix Pyrra's UI uses. This is helpful for when the prefix is stripped by a proxy but still runs on /pyrra. Defaults to --route-prefix"`
		PrometheusBearerTokenPath string   `default:"" help:"Bearer token path"`
	} `cmd:"" help:"Runs Pyrra's API and UI."`
	Filesystem struct {
		ConfigFiles      string `default:"/etc/pyrra/*.yaml" help:"The folder where Pyrra finds the config files to use."`
		PrometheusFolder string `default:"/etc/prometheus/pyrra/" help:"The folder where Pyrra writes the generates Prometheus rules and alerts."`
	} `cmd:"" help:"Runs Pyrra's filesystem operator and backend for the API."`
	Kubernetes struct {
		MetricsAddr string `default:":8080" help:"The address the metric endpoint binds to."`
	} `cmd:"" help:"Runs Pyrra's Kubernetes operator and backend for the API."`
}

func main() {
	ctx := kong.Parse(&CLI)
	switch ctx.Command() {
	case "api":
		cmdAPI(CLI.API.PrometheusURL, CLI.API.PrometheusExternalURL, CLI.API.ApiURL, CLI.API.RoutePrefix, CLI.API.UIRoutePrefix, CLI.API.PrometheusBearerTokenPath)
	case "filesystem":
		cmdFilesystem(CLI.Filesystem.ConfigFiles, CLI.Filesystem.PrometheusFolder)
	case "kubernetes":
		cmdKubernetes(CLI.Kubernetes.MetricsAddr)
	}
}

func cmdAPI(prometheusURL, prometheusExternal, apiURL *url.URL, routePrefix, uiRoutePrefix string, prometheusBearerTokenPath string) {
	build, err := fs.Sub(ui, "ui/build")
	if err != nil {
		log.Fatal(err)
	}

	if prometheusExternal == nil {
		prometheusExternal = prometheusURL
	}

	// RoutePrefix must always be at least '/'.
	routePrefix = "/" + strings.Trim(routePrefix, "/")
	if uiRoutePrefix == "" {
		uiRoutePrefix = routePrefix
	} else {
		uiRoutePrefix = "/" + strings.Trim(uiRoutePrefix, "/")
	}

	log.Println("Using Prometheus at", prometheusURL.String())
	log.Println("Using external Prometheus at", prometheusExternal.String())
	log.Println("Using API at", apiURL.String())
	log.Println("Using route prefix", routePrefix)

	reg := prometheus.NewRegistry()

	config := api.Config{Address: prometheusURL.String()}
	if len(prometheusBearerTokenPath) > 0 {
		config.RoundTripper = promconfig.NewAuthorizationCredentialsFileRoundTripper("Bearer", prometheusBearerTokenPath, api.DefaultRoundTripper)
	}

	client, err := api.NewClient(config)
	if err != nil {
		log.Fatal(err)
	}
	thanosClient := newThanosClient(client)

	cache, err := ristretto.NewCache(&ristretto.Config{
		NumCounters: 1e7,     // number of keys to track frequency of (10M).
		MaxCost:     1 << 30, // maximum cost of cache (1GB).
		BufferItems: 64,      // number of keys per Get buffer.
	})
	if err != nil {
		log.Fatal(err)
	}
	defer cache.Close()
	promAPI := &promCache{
		api:   prometheusv1.NewAPI(thanosClient),
		cache: cache,
	}

	apiConfig := openapiclient.NewConfiguration()
	apiConfig.Scheme = apiURL.Scheme
	apiConfig.Host = apiURL.Host
	apiClient := openapiclient.NewAPIClient(apiConfig)

	router := openapiserver.NewRouter(
		openapiserver.NewObjectivesApiController(&ObjectivesServer{
			promAPI:   promAPI,
			apiclient: apiClient,
		}),
	)
	router.Use(openapi.MiddlewareMetrics(reg))

	tmpl, err := template.ParseFS(build, "index.html")
	if err != nil {
		log.Fatal(err)
	}

	r := chi.NewRouter()
	r.Use(cors.Handler(cors.Options{})) // TODO: Disable by default

	r.Route(routePrefix, func(r chi.Router) {
		if routePrefix != "/" {
			r.Mount("/api/v1", http.StripPrefix(routePrefix, router))
		} else {
			r.Mount("/api/v1", router)
		}

		r.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
		r.Get("/objectives", func(w http.ResponseWriter, r *http.Request) {
			if err := tmpl.Execute(w, struct {
				PrometheusURL string
				PathPrefix    string
			}{
				PrometheusURL: prometheusExternal.String(),
				PathPrefix:    uiRoutePrefix,
			}); err != nil {
				log.Println(err)
				return
			}
		})
		r.Handle("/*", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Trim trailing slash to not care about matching e.g. /pyrra and /pyrra/
			if r.URL.Path == "/" || strings.TrimSuffix(r.URL.Path, "/") == routePrefix {
				if err := tmpl.Execute(w, struct {
					PrometheusURL string
					PathPrefix    string
				}{
					PrometheusURL: prometheusExternal.String(),
					PathPrefix:    uiRoutePrefix,
				}); err != nil {
					log.Println(err)
				}
				return
			}

			http.StripPrefix(
				routePrefix,
				http.FileServer(http.FS(build)),
			).ServeHTTP(w, r)
		}))
	})

	if routePrefix != "/" {
		// Redirect /pyrra to /pyrra/ for the UI to work properly.
		r.HandleFunc(strings.TrimSuffix(routePrefix, "/"), func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, routePrefix+"/", http.StatusPermanentRedirect)
			return
		})
	}

	if err := http.ListenAndServe(":9099", r); err != nil {
		log.Fatal(err)
	}
}

func newThanosClient(client api.Client) api.Client {
	return &thanosClient{client: client}
}

// thanosClient wraps the Prometheus Client to inject some headers to disable partial responses
// and enables querying for downsampled data.
type thanosClient struct {
	client api.Client
}

func (c *thanosClient) URL(ep string, args map[string]string) *url.URL {
	return c.client.URL(ep, args)
}

func (c *thanosClient) Do(ctx context.Context, r *http.Request) (*http.Response, []byte, error) {
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, nil, err
	}
	query, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, nil, err
	}

	// We don't want partial responses, especially not when calculating error budgets.
	query.Set("partial_response", "false")
	r.ContentLength += 23

	if strings.HasSuffix(r.URL.Path, "/api/v1/query_range") {
		start, err := strconv.ParseFloat(query.Get("start"), 64)
		if err != nil {
			return nil, nil, err
		}
		end, err := strconv.ParseFloat(query.Get("end"), 64)
		if err != nil {
			return nil, nil, err
		}

		if end-start >= 28*24*60*60 { // request 1h downsamples when range > 28d
			query.Set("max_source_resolution", "1h")
			r.ContentLength += 25
		} else if end-start >= 7*24*60*60 { // request 5m downsamples when range > 1w
			query.Set("max_source_resolution", "5m")
			r.ContentLength += 25
		}
	}

	encoded := query.Encode()
	r.Body = ioutil.NopCloser(strings.NewReader(encoded))
	return c.client.Do(ctx, r)
}

type prometheusAPI interface {
	// Query performs a query for the given time.
	Query(ctx context.Context, query string, ts time.Time) (model.Value, prometheusv1.Warnings, error)
	// QueryRange performs a query for the given range.
	QueryRange(ctx context.Context, query string, r prometheusv1.Range) (model.Value, prometheusv1.Warnings, error)
}

func RoundUp(t time.Time, d time.Duration) time.Time {
	n := t.Round(d)
	if n.Before(t) {
		return n.Add(d)
	}
	return n
}

type promCache struct {
	api   prometheusAPI
	cache *ristretto.Cache
}

func (p *promCache) Query(ctx context.Context, query string, ts time.Time) (model.Value, prometheusv1.Warnings, error) {
	xxh := xxhash.New()
	_, _ = xxh.WriteString(query)
	hash := xxh.Sum64()

	if value, exists := p.cache.Get(hash); exists {
		return value.(model.Value), nil, nil
	}

	value, warnings, err := p.api.Query(ctx, query, ts)
	if err != nil {
		return nil, warnings, err
	}

	if v, ok := value.(model.Vector); ok {
		if len(v) > 0 && len(warnings) == 0 {
			// TODO might need to pass cache duration via ctx?
			_ = p.cache.SetWithTTL(hash, value, 10, 5*time.Minute)
		}
	}

	return value, warnings, nil
}

func (p *promCache) QueryRange(ctx context.Context, query string, r prometheusv1.Range) (model.Value, prometheusv1.Warnings, error) {
	xxh := xxhash.New()
	_, _ = xxh.WriteString(query)
	_, _ = xxh.WriteString(r.Start.String())
	_, _ = xxh.WriteString(r.End.String())
	hash := xxh.Sum64()

	if value, exists := p.cache.Get(hash); exists {
		return value.(model.Value), nil, nil
	}

	value, warnings, err := p.api.QueryRange(ctx, query, r)
	if err != nil {
		return nil, warnings, err
	}

	if m, ok := value.(model.Matrix); ok {
		if len(m) > 0 && len(warnings) == 0 {
			// TODO might need to pass cache duration via ctx?
			_ = p.cache.SetWithTTL(hash, value, 100, 10*time.Minute)
		}
	}

	return value, warnings, nil
}

type ObjectivesServer struct {
	promAPI   *promCache
	apiclient *openapiclient.APIClient
}

func (o *ObjectivesServer) ListObjectives(ctx context.Context, query string) (openapiserver.ImplResponse, error) {
	if query != "" {
		// We'll parse the query matchers already to make sure it's valid before passing on to the backend.
		if _, err := parser.ParseMetricSelector(query); err != nil {
			return openapiserver.ImplResponse{Code: http.StatusBadRequest}, err
		}
	}

	objectives, _, err := o.apiclient.ObjectivesApi.ListObjectives(ctx).Expr(query).Execute()
	if err != nil {
		return openapiserver.ImplResponse{Code: http.StatusInternalServerError}, err
	}

	apiObjectives := make([]openapiserver.Objective, len(objectives))
	for i, objective := range objectives {
		apiObjectives[i] = openapi.ServerFromClient(objective)
	}

	return openapiserver.ImplResponse{
		Code: http.StatusOK,
		Body: apiObjectives,
	}, nil
}

func (o *ObjectivesServer) GetObjectiveStatus(ctx context.Context, expr string, grouping string) (openapiserver.ImplResponse, error) {
	clientObjectives, _, err := o.apiclient.ObjectivesApi.ListObjectives(ctx).Expr(expr).Execute()
	if err != nil {
		var apiErr openapiclient.GenericOpenAPIError
		if errors.As(err, &apiErr) {
			if strings.HasPrefix(apiErr.Error(), strconv.Itoa(http.StatusNotFound)) {
				return openapiserver.ImplResponse{Code: http.StatusNotFound}, apiErr
			}
		}
		return openapiserver.ImplResponse{Code: http.StatusInternalServerError}, err
	}
	if len(clientObjectives) != 1 {
		return openapiserver.ImplResponse{Code: http.StatusBadRequest}, fmt.Errorf("expr matches more than one SLO, it matches: %d", len(clientObjectives))
	}

	objective := openapi.InternalFromClient(clientObjectives[0])

	// Merge grouping into objective's query
	if grouping != "" {
		groupingMatchers, err := parser.ParseMetricSelector(grouping)
		if err != nil {
			return openapiserver.ImplResponse{}, err
		}
		if objective.Indicator.Ratio != nil {
			for _, m := range groupingMatchers {
				objective.Indicator.Ratio.Errors.LabelMatchers = append(objective.Indicator.Ratio.Errors.LabelMatchers, m)
				objective.Indicator.Ratio.Total.LabelMatchers = append(objective.Indicator.Ratio.Total.LabelMatchers, m)
			}
		}
		if objective.Indicator.Latency != nil {
			for _, m := range groupingMatchers {
				objective.Indicator.Latency.Success.LabelMatchers = append(objective.Indicator.Latency.Success.LabelMatchers, m)
				objective.Indicator.Latency.Total.LabelMatchers = append(objective.Indicator.Latency.Total.LabelMatchers, m)
			}
		}
	}

	ts := RoundUp(time.Now().UTC(), 5*time.Minute)

	queryTotal := objective.QueryTotal(objective.Window)
	log.Println(queryTotal)
	value, _, err := o.promAPI.Query(ctx, queryTotal, ts)
	if err != nil {
		return openapiserver.ImplResponse{Code: http.StatusInternalServerError}, err
	}

	statuses := map[model.Fingerprint]*openapiserver.ObjectiveStatus{}

	for _, v := range value.(model.Vector) {
		labels := make(map[string]string)
		for k, v := range v.Metric {
			labels[string(k)] = string(v)
		}

		statuses[v.Metric.Fingerprint()] = &openapiserver.ObjectiveStatus{
			Labels: labels,
			Availability: openapiserver.ObjectiveStatusAvailability{
				Percentage: 1,
				Total:      float64(v.Value),
			},
		}
	}

	queryErrors := objective.QueryErrors(objective.Window)
	log.Println(queryErrors)
	value, _, err = o.promAPI.Query(ctx, queryErrors, ts)
	if err != nil {
		return openapiserver.ImplResponse{Code: http.StatusInternalServerError}, err
	}
	for _, v := range value.(model.Vector) {
		s := statuses[v.Metric.Fingerprint()]
		s.Availability.Errors = float64(v.Value)
		s.Availability.Percentage = 1 - (s.Availability.Errors / s.Availability.Total)
	}

	statusSlice := make([]openapiserver.ObjectiveStatus, 0, len(statuses))

	for _, s := range statuses {
		s.Budget.Total = 1 - objective.Target
		s.Budget.Remaining = (s.Budget.Total - (s.Availability.Errors / s.Availability.Total)) / s.Budget.Total
		s.Budget.Max = s.Budget.Total * s.Availability.Total

		// If this objective has no requests, we'll skip showing it too
		if s.Availability.Total == 0 {
			continue
		}

		if math.IsNaN(s.Availability.Percentage) {
			s.Availability.Percentage = 1
		}
		if math.IsNaN(s.Budget.Remaining) {
			s.Budget.Remaining = 1
		}

		statusSlice = append(statusSlice, *s)
	}

	return openapiserver.ImplResponse{
		Code: http.StatusOK,
		Body: statusSlice,
	}, nil
}

func (o *ObjectivesServer) GetObjectiveErrorBudget(ctx context.Context, expr string, grouping string, startTimestamp int32, endTimestamp int32) (openapiserver.ImplResponse, error) {
	clientObjectives, _, err := o.apiclient.ObjectivesApi.ListObjectives(ctx).Expr(expr).Execute()
	if err != nil {
		return openapiserver.ImplResponse{Code: http.StatusInternalServerError}, err
	}
	if len(clientObjectives) != 1 {
		return openapiserver.ImplResponse{Code: http.StatusBadRequest}, fmt.Errorf("expr matches more than one SLO, it matches: %d", len(clientObjectives))
	}
	objective := openapi.InternalFromClient(clientObjectives[0])

	// Merge grouping into objective's query
	if grouping != "" {
		groupingMatchers, err := parser.ParseMetricSelector(grouping)
		if err != nil {
			return openapiserver.ImplResponse{}, err
		}
		if objective.Indicator.Ratio != nil {
			groupings := map[string]struct{}{}
			for _, g := range objective.Indicator.Ratio.Grouping {
				groupings[g] = struct{}{}
			}

			for _, m := range groupingMatchers {
				objective.Indicator.Ratio.Errors.LabelMatchers = append(objective.Indicator.Ratio.Errors.LabelMatchers, m)
				objective.Indicator.Ratio.Total.LabelMatchers = append(objective.Indicator.Ratio.Total.LabelMatchers, m)
				delete(groupings, m.Name)
			}

			objective.Indicator.Ratio.Grouping = []string{}
			for g := range groupings {
				objective.Indicator.Ratio.Grouping = append(objective.Indicator.Ratio.Grouping, g)
			}
		}
		if objective.Indicator.Latency != nil {
			groupings := map[string]struct{}{}
			for _, g := range objective.Indicator.Ratio.Grouping {
				groupings[g] = struct{}{}
			}

			for _, m := range groupingMatchers {
				objective.Indicator.Latency.Success.LabelMatchers = append(objective.Indicator.Latency.Success.LabelMatchers, m)
				objective.Indicator.Latency.Total.LabelMatchers = append(objective.Indicator.Latency.Total.LabelMatchers, m)
				delete(groupings, m.Name)
			}

			objective.Indicator.Ratio.Grouping = []string{}
			for g := range groupings {
				objective.Indicator.Ratio.Grouping = append(objective.Indicator.Ratio.Grouping, g)
			}
		}
	}

	now := time.Now()
	start := now.Add(-1 * time.Hour)
	end := now

	if startTimestamp != 0 && endTimestamp != 0 {
		start = time.Unix(int64(startTimestamp), 0)
		end = time.Unix(int64(endTimestamp), 0)
	}

	step := end.Sub(start) / 1000

	query := objective.QueryErrorBudget()
	log.Println(query)
	value, _, err := o.promAPI.QueryRange(ctx, query, prometheusv1.Range{
		Start: start,
		End:   end,
		Step:  step,
	})
	if err != nil {
		return openapiserver.ImplResponse{Code: http.StatusInternalServerError}, err
	}

	matrix, ok := value.(model.Matrix)
	if !ok {
		return openapiserver.ImplResponse{Code: http.StatusInternalServerError}, fmt.Errorf("no matrix returned")
	}

	if len(matrix) == 0 {
		return openapiserver.ImplResponse{Code: http.StatusNotFound, Body: struct{}{}}, nil
	}

	valueLength := 0
	for _, m := range matrix {
		if len(m.Values) > valueLength {
			valueLength = len(m.Values)
		}
	}

	values := matrixToValues(matrix)

	return openapiserver.ImplResponse{
		Code: http.StatusOK,
		Body: openapiserver.QueryRange{
			Query:  query,
			Labels: nil,
			Values: values,
		},
	}, nil
}

const (
	alertstateInactive = "inactive"
	alertstatePending  = "pending"
	alertstateFiring   = "firing"
)

func (o *ObjectivesServer) GetMultiBurnrateAlerts(ctx context.Context, expr string, grouping string) (openapiserver.ImplResponse, error) {
	clientObjectives, _, err := o.apiclient.ObjectivesApi.ListObjectives(ctx).Expr(expr).Execute()
	if err != nil {
		return openapiserver.ImplResponse{Code: http.StatusInternalServerError}, err
	}
	if len(clientObjectives) != 1 {
		return openapiserver.ImplResponse{Code: http.StatusBadRequest}, fmt.Errorf("expr matches not exactly one SLO")
	}

	objective := openapi.InternalFromClient(clientObjectives[0])

	// Merge grouping into objective's query
	if grouping != "" {
		groupingMatchers, err := parser.ParseMetricSelector(grouping)
		if err != nil {
			return openapiserver.ImplResponse{}, err
		}

		if objective.Indicator.Ratio != nil {
			objective.Indicator.Ratio.Grouping = nil // Don't group by here

			for _, m := range groupingMatchers {
				objective.Indicator.Ratio.Errors.LabelMatchers = append(objective.Indicator.Ratio.Errors.LabelMatchers, m)
				objective.Indicator.Ratio.Total.LabelMatchers = append(objective.Indicator.Ratio.Total.LabelMatchers, m)
			}
		}
		if objective.Indicator.Latency != nil {
			objective.Indicator.Latency.Grouping = nil // Don't group by here

			for _, m := range groupingMatchers {
				objective.Indicator.Latency.Success.LabelMatchers = append(objective.Indicator.Latency.Success.LabelMatchers, m)
				objective.Indicator.Latency.Total.LabelMatchers = append(objective.Indicator.Latency.Total.LabelMatchers, m)
			}
		}
	}

	baseAlerts, err := objective.Alerts()
	if err != nil {
		return openapiserver.ImplResponse{Code: http.StatusInternalServerError}, err
	}

	var alerts []openapiserver.MultiBurnrateAlert

	for _, ba := range baseAlerts {
		short := &openapiserver.Burnrate{
			Window:  ba.Short.Milliseconds(),
			Current: -1,
			Query:   ba.QueryShort,
		}
		long := &openapiserver.Burnrate{
			Window:  ba.Long.Milliseconds(),
			Current: -1,
			Query:   ba.QueryLong,
		}

		var wg sync.WaitGroup
		wg.Add(3)

		go func(b *openapiserver.Burnrate) {
			defer wg.Done()

			value, _, err := o.promAPI.Query(ctx, b.Query, time.Now())
			if err != nil {
				log.Println(err)
				return
			}
			vec, ok := value.(model.Vector)
			if !ok {
				log.Println("no vector")
				return
			}
			if vec.Len() != 1 {
				return
			}
			b.Current = float64(vec[0].Value)
		}(short)

		go func(b *openapiserver.Burnrate) {
			defer wg.Done()

			value, _, err := o.promAPI.Query(ctx, b.Query, time.Now())
			if err != nil {
				log.Println(err)
				return
			}
			vec, ok := value.(model.Vector)
			if !ok {
				log.Println("no vector")
				return
			}
			if vec.Len() != 1 {
				return
			}
			b.Current = float64(vec[0].Value)
		}(long)

		alertstate := alertstateInactive

		// TODO: It should be possible to reduce the amount of queries by querying ALERTS{slo="%s"}
		// and then matching the resulting short and long values.

		go func(name string, short, long int64) {
			defer wg.Done()

			s := model.Duration(time.Duration(short) * time.Millisecond)
			l := model.Duration(time.Duration(long) * time.Millisecond)

			query := fmt.Sprintf(`ALERTS{slo="%s",short="%s",long="%s"}`, name, s, l)
			value, _, err := o.promAPI.Query(ctx, query, time.Now())
			if err != nil {
				log.Println(err)
				return
			}
			vec, ok := value.(model.Vector)
			if !ok {
				log.Println("no vector")
				return
			}
			if vec.Len() != 1 {
				return
			}
			sample := vec[0]

			if sample.Value != 1 {
				log.Println("alert is not pending or firing")
				return
			}

			ls := model.LabelSet(sample.Metric)
			as := ls["alertstate"]
			if as == alertstatePending {
				alertstate = alertstatePending
			}
			if as == alertstateFiring {
				alertstate = alertstateFiring
			}
		}(objective.Labels.Get(labels.MetricName), short.Window, long.Window)

		wg.Wait()

		alerts = append(alerts, openapiserver.MultiBurnrateAlert{
			Severity: ba.Severity,
			For:      ba.For.Milliseconds(),
			Factor:   ba.Factor,
			Short:    *short,
			Long:     *long,
			State:    alertstate,
		})
	}

	return openapiserver.ImplResponse{
		Code: http.StatusOK,
		Body: alerts,
	}, nil
}

func (o *ObjectivesServer) GetREDRequests(ctx context.Context, expr string, grouping string, startTimestamp int32, endTimestamp int32) (openapiserver.ImplResponse, error) {
	clientObjectives, _, err := o.apiclient.ObjectivesApi.ListObjectives(ctx).Expr(expr).Execute()
	if err != nil {
		return openapiserver.ImplResponse{Code: http.StatusInternalServerError}, err
	}
	if len(clientObjectives) != 1 {
		return openapiserver.ImplResponse{Code: http.StatusBadRequest}, fmt.Errorf("expr matches not exactly one SLO")
	}
	objective := openapi.InternalFromClient(clientObjectives[0])

	// Merge grouping into objective's query
	if grouping != "" {
		groupingMatchers, err := parser.ParseMetricSelector(grouping)
		if err != nil {
			return openapiserver.ImplResponse{}, err
		}
		if objective.Indicator.Ratio != nil {
			for _, m := range groupingMatchers {
				objective.Indicator.Ratio.Errors.LabelMatchers = append(objective.Indicator.Ratio.Errors.LabelMatchers, m)
				objective.Indicator.Ratio.Total.LabelMatchers = append(objective.Indicator.Ratio.Total.LabelMatchers, m)
			}
		}
		if objective.Indicator.Latency != nil {
			for _, m := range groupingMatchers {
				objective.Indicator.Latency.Success.LabelMatchers = append(objective.Indicator.Latency.Success.LabelMatchers, m)
				objective.Indicator.Latency.Total.LabelMatchers = append(objective.Indicator.Latency.Total.LabelMatchers, m)
			}
		}
	}

	now := time.Now()
	start := now.Add(-1 * time.Hour)
	end := now

	if startTimestamp != 0 && endTimestamp != 0 {
		start = time.Unix(int64(startTimestamp), 0)
		end = time.Unix(int64(endTimestamp), 0)
	}
	step := end.Sub(start) / 1000

	diff := end.Sub(start)
	timeRange := 5 * time.Minute
	if diff >= 28*24*time.Hour {
		timeRange = 3 * time.Hour
	} else if diff >= 7*24*time.Hour {
		timeRange = time.Hour
	} else if diff >= 24*time.Hour {
		timeRange = 30 * time.Minute
	} else if diff >= 12*time.Hour {
		timeRange = 15 * time.Minute
	}
	query := objective.RequestRange(timeRange)
	log.Println(query)

	value, _, err := o.promAPI.QueryRange(ctx, query, prometheusv1.Range{
		Start: start,
		End:   end,
		Step:  step,
	})
	if err != nil {
		return openapiserver.ImplResponse{Code: http.StatusInternalServerError}, err
	}

	if value.Type() != model.ValMatrix {
		return openapiserver.ImplResponse{Code: http.StatusInternalServerError}, fmt.Errorf("returned data is not a matrix")
	}

	matrix, ok := value.(model.Matrix)
	if !ok {
		return openapiserver.ImplResponse{Code: http.StatusInternalServerError}, fmt.Errorf("no matrix returned")
	}

	if len(matrix) == 0 {
		return openapiserver.ImplResponse{Code: http.StatusNotFound, Body: struct{}{}}, nil
	}

	valueLength := 0
	for _, m := range matrix {
		if len(m.Values) > valueLength {
			valueLength = len(m.Values)
		}
	}

	labels := make([]string, len(matrix))
	for i, stream := range matrix {
		labels[i] = model.LabelSet(stream.Metric).String()
	}

	values := matrixToValues(matrix)

	return openapiserver.ImplResponse{
		Code: http.StatusOK,
		Body: openapiserver.QueryRange{
			Query:  query,
			Labels: labels,
			Values: values,
		},
	}, nil
}

func (o *ObjectivesServer) GetREDErrors(ctx context.Context, expr string, grouping string, startTimestamp int32, endTimestamp int32) (openapiserver.ImplResponse, error) {
	clientObjectives, _, err := o.apiclient.ObjectivesApi.ListObjectives(ctx).Expr(expr).Execute()
	if err != nil {
		return openapiserver.ImplResponse{Code: http.StatusInternalServerError}, err
	}
	if len(clientObjectives) != 1 {
		return openapiserver.ImplResponse{Code: http.StatusBadRequest}, fmt.Errorf("expr matches not exactly one SLO")
	}
	objective := openapi.InternalFromClient(clientObjectives[0])

	// Merge grouping into objective's query
	if grouping != "" {
		groupingMatchers, err := parser.ParseMetricSelector(grouping)
		if err != nil {
			return openapiserver.ImplResponse{}, err
		}
		if objective.Indicator.Ratio != nil {
			for _, m := range groupingMatchers {
				objective.Indicator.Ratio.Errors.LabelMatchers = append(objective.Indicator.Ratio.Errors.LabelMatchers, m)
				objective.Indicator.Ratio.Total.LabelMatchers = append(objective.Indicator.Ratio.Total.LabelMatchers, m)
			}
		}
		if objective.Indicator.Latency != nil {
			for _, m := range groupingMatchers {
				objective.Indicator.Latency.Success.LabelMatchers = append(objective.Indicator.Latency.Success.LabelMatchers, m)
				objective.Indicator.Latency.Total.LabelMatchers = append(objective.Indicator.Latency.Total.LabelMatchers, m)
			}
		}
	}

	now := time.Now()
	start := now.Add(-1 * time.Hour)
	end := now

	if startTimestamp != 0 && endTimestamp != 0 {
		start = time.Unix(int64(startTimestamp), 0)
		end = time.Unix(int64(endTimestamp), 0)
	}
	step := end.Sub(start) / 1000

	diff := end.Sub(start)
	timeRange := 5 * time.Minute
	if diff >= 28*24*time.Hour {
		timeRange = 3 * time.Hour
	} else if diff >= 7*24*time.Hour {
		timeRange = time.Hour
	} else if diff >= 24*time.Hour {
		timeRange = 30 * time.Minute
	} else if diff >= 12*time.Hour {
		timeRange = 15 * time.Minute
	}

	query := objective.ErrorsRange(timeRange)
	log.Println(query)

	value, _, err := o.promAPI.QueryRange(ctx, query, prometheusv1.Range{
		Start: start,
		End:   end,
		Step:  step,
	})
	if err != nil {
		return openapiserver.ImplResponse{Code: http.StatusInternalServerError}, err
	}

	if value.Type() != model.ValMatrix {
		return openapiserver.ImplResponse{Code: http.StatusInternalServerError}, fmt.Errorf("returned data is not a matrix")
	}

	matrix, ok := value.(model.Matrix)
	if !ok {
		return openapiserver.ImplResponse{Code: http.StatusInternalServerError}, fmt.Errorf("no matrix returned")
	}

	if len(matrix) == 0 {
		return openapiserver.ImplResponse{Code: http.StatusNotFound, Body: struct{}{}}, nil
	}

	valueLength := 0
	for _, m := range matrix {
		if len(m.Values) > valueLength {
			valueLength = len(m.Values)
		}
	}

	labels := make([]string, len(matrix))
	for i, stream := range matrix {
		labels[i] = model.LabelSet(stream.Metric).String()
	}

	values := matrixToValues(matrix)

	return openapiserver.ImplResponse{
		Code: http.StatusOK,
		Body: openapiserver.QueryRange{
			Query:  query,
			Labels: labels,
			Values: values,
		},
	}, nil
}

func matrixToValues(m model.Matrix) [][]float64 {
	series := len(m)
	if series == 0 {
		return nil
	}

	if series == 1 {
		vs := make([][]float64, len(m)+1) // +1 for timestamps
		for i, stream := range m {
			vs[0] = make([]float64, len(stream.Values))
			vs[i+1] = make([]float64, len(stream.Values))

			for j, pair := range stream.Values {
				vs[0][j] = float64(pair.Timestamp / 1000)
				if !math.IsNaN(float64(pair.Value)) {
					vs[i+1][j] = float64(pair.Value)
				}
			}
		}
		return vs
	}

	pairs := make(map[int64][]float64, len(m[0].Values))
	for i, stream := range m {
		for _, pair := range stream.Values {
			t := int64(pair.Timestamp / 1000)
			if _, ok := pairs[t]; !ok {
				pairs[t] = make([]float64, series)
			}
			if !math.IsNaN(float64(pair.Value)) {
				pairs[t][i] = float64(pair.Value)
			}
		}
	}

	vs := make(values, series+1)
	for i := 0; i < series+1; i++ {
		vs[i] = make([]float64, len(pairs))
	}
	var i int
	for t, fs := range pairs {
		vs[0][i] = float64(t)
		for j, f := range fs {
			vs[j+1][i] = f
		}
		i++
	}

	sort.Sort(vs)

	return vs
}

type values [][]float64

func (v values) Len() int {
	return len(v[0])
}

func (v values) Less(i, j int) bool {
	return v[0][i] < v[0][j]
}

// Swap iterates over all []float64 and consistently swaps them
func (v values) Swap(i, j int) {
	for n := range v {
		v[n][i], v[n][j] = v[n][j], v[n][i]
	}
}
