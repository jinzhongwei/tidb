// Package traceapp implements the Appdash web UI.
//
// The web UI can be effectively launched using the appdash command (see
// cmd/appdash) or via embedding this package within your app.
//
// Templates and other resources needed by this package to render the UI are
// built into the program using vfsgen, so you still get to have single
// binary deployment.
//
// For an example of embedding the Appdash web UI within your own application
// via the traceapp package, see the examples/cmd/webapp example.
package traceapp

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	htmpl "html/template"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/gorilla/mux"

	"github.com/tiancaiamao/appdash"
	static "sourcegraph.com/sourcegraph/appdash-data"
)

// App is an HTTP application handler that also exposes methods for
// constructing URL routes.
type App struct {
	*Router

	Store      appdash.Store
	Queryer    appdash.Queryer
	Aggregator appdash.Aggregator

	tmplLock sync.Mutex
	tmpls    map[string]*htmpl.Template

	Log     *log.Logger
	baseURL *url.URL
}

// New creates a new application handler. If r is nil, a new router is
// created.
//
// The given base URL is the absolute base URL under which traceapp is being
// served, e.g., "https://appdash.mysite.com" or "https://mysite.com/appdash".
// The base URL must contain a scheme and host, or else an error will be
// returned.
func New(r *Router, base *url.URL) (*App, error) {
	if r == nil {
		r = NewRouter(nil)
	}

	// Validate the base URL and use the root path if none was specified.
	cpy := *base
	base = &cpy
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("appdash: base URL must contain both scheme and host, found %q", base.String())
	}
	if base.Path == "" {
		base.Path = "/"
	}

	app := &App{
		Router:  r,
		Log:     log.New(os.Stderr, "appdash: ", log.LstdFlags),
		baseURL: base,
	}

	r.r.Get(ViewTrace).Handler(handlerFunc(app.viewTrace))
	r.r.Get(RootRoute).Handler(handlerFunc(app.serveRoot))
	r.r.Get(TraceRoute).Handler(handlerFunc(app.serveTrace))
	r.r.Get(TraceSpanRoute).Handler(handlerFunc(app.serveTrace))
	r.r.Get(TraceProfileRoute).Handler(handlerFunc(app.serveTrace))
	r.r.Get(TraceSpanProfileRoute).Handler(handlerFunc(app.serveTrace))
	r.r.Get(TraceUploadRoute).Handler(handlerFunc(app.serveTraceUpload))
	r.r.Get(TracesRoute).Handler(handlerFunc(app.serveTraces))
	r.r.Get(DashboardRoute).Handler(handlerFunc(app.serveDashboard))
	r.r.Get(DashboardDataRoute).Handler(handlerFunc(app.serveDashboardData))
	r.r.Get(AggregateRoute).Handler(handlerFunc(app.serveAggregate))

	// Static file serving.
	r.r.Get(StaticRoute).Handler(http.StripPrefix("/static/", http.FileServer(static.Data)))

	return app, nil
}

// ServeHTTP implements http.Handler.
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.Router.r.ServeHTTP(w, r)
}

func (a *App) serveRoot(w http.ResponseWriter, r *http.Request) error {
	return a.renderTemplate(w, r, "root.html", http.StatusOK, &struct {
		TemplateCommon
	}{})
}

func HandleTiDB(w http.ResponseWriter, r *http.Request) {
	data := `<html>
		<head >
		<meta http-equiv='Content-Type' content='text/html; charset=utf-8'/>
		<title >Form Page: sampleform</title>
		</head>
		<body>
		<h1>TiDB Trace Viewer</h1>
		  <form method="post" action="/web/trace/view" >
		  <input type="text" name="data" />
		  <input type="submit" name="submit" value="View Trace" />
		  </form>
		</body>
		</html>`
	buf := bytes.NewBufferString(data)
	io.Copy(w, buf)
}

func (a *App) viewTrace(w http.ResponseWriter, r *http.Request) error {
	defer r.Body.Close()

	var err error
	var data []byte
	// Read the uploaded JSON trace data.
	data1 := r.FormValue("data")
	if data1 == "" {
		data, err = ioutil.ReadAll(r.Body)
		if err != nil {
			return err
		}
	} else {
		data = []byte(data1)
	}

	// Unmarshal the trace.
	var traces []*appdash.Trace
	err = json.Unmarshal(data, &traces)
	if err != nil {
		log.Println(data1)
		return err
	}
	if len(traces) != 1 {
		return errors.New("invalid trace format")
	}

	store := appdash.NewMemoryStore()
	a.Store = store
	a.Queryer = store
	a.uploadTraces(traces...)
	return a.serveTraceID(w, r, traces[0].Span.ID.Trace)
}

func (a *App) serveTrace(w http.ResponseWriter, r *http.Request) error {
	v := mux.Vars(r)

	if permalink := r.URL.Query().Get("permalink"); permalink != "" {
		// If the user specified a permalink, then decode it directly into a
		// trace structure and place it into storage for viewing.
		gz, err := gzip.NewReader(base64.NewDecoder(base64.RawURLEncoding, strings.NewReader(permalink)))
		if err != nil {
			return err
		}
		var upload *appdash.Trace
		if err := json.NewDecoder(gz).Decode(&upload); err != nil {
			return err
		}
		if err := a.uploadTraces(upload); err != nil {
			return err
		}
	}

	// Look in the store for the trace.
	traceID, err := appdash.ParseID(v["Trace"])
	if err != nil {
		return err
	}
	return a.serveTraceID(w, r, traceID)
}

func (a *App) serveTraceID(w http.ResponseWriter, r *http.Request, traceID appdash.ID) error {
	trace, err := a.Store.Trace(traceID)
	if err != nil {
		return err
	}

	v := mux.Vars(r)
	// Get sub-span if the Span route var is present.
	if spanIDStr := v["Span"]; spanIDStr != "" {
		var spanID appdash.ID
		if spanID, err = appdash.ParseID(spanIDStr); err != nil {
			return err
		}
		trace = trace.FindSpan(spanID)
		if trace == nil {
			return errors.New("could not find the specified trace span")
		}
	}

	// We could use a separate handler for this, but as we need the above to
	// determine the correct trace (or therein sub-trace), we just handle any
	// JSON profile requests here.
	if path.Base(r.URL.Path) == "profile" {
		return a.profile(trace, w)
	}

	// Do not show d3 timeline chart when timeline item fields are invalid.
	// So we avoid JS code breaking due missing values.
	var showTimelineChart bool = true
	visData, err := a.d3timeline(trace)
	switch err {
	case errTimelineItemValidation:
		showTimelineChart = false
	case nil:
		break
	default:
		return err
	}

	// Determine the profile URL.
	var profile *url.URL
	if trace.ID.Parent == 0 {
		profile, err = a.Router.URLToTraceProfile(trace.Span.ID.Trace)
	} else {
		profile, err = a.Router.URLToTraceSpanProfile(trace.Span.ID.Trace, trace.Span.ID.Span)
	}
	if err != nil {
		return err
	}

	// The JSON trace is the human-readable trace form for exporting.
	jsonTrace, err := json.MarshalIndent([]*appdash.Trace{trace}, "", "  ")
	if err != nil {
		return err
	}

	// The permalink of the trace is literally the JSON encoded trace gzipped & base64 encoded.
	var buf bytes.Buffer
	gz := gzip.NewWriter(base64.NewEncoder(base64.RawURLEncoding, &buf))
	err = json.NewEncoder(gz).Encode(trace)
	if err != nil {
		return err
	}
	if err = gz.Close(); err != nil {
		return err
	}
	permalink, err := a.URLToTrace(trace.ID.Trace)
	if err != nil {
		return err
	}
	permalink.RawQuery = "permalink=" + buf.String()

	return a.renderTemplate(w, r, "trace.html", http.StatusOK, &struct {
		TemplateCommon
		Trace             *appdash.Trace
		ShowTimelineChart bool
		VisData           []timelineItem
		ProfileURL        string
		Permalink         string
		JSONTrace         string
	}{
		Trace:             trace,
		ShowTimelineChart: showTimelineChart,
		VisData:           visData,
		ProfileURL:        profile.String(),
		Permalink:         permalink.String(),
		JSONTrace:         string(jsonTrace),
	})
}

func (a *App) ServeTraces(w http.ResponseWriter, r *http.Request) {
	a.serveTraces(w, r)
}

func (a *App) serveTraces(w http.ResponseWriter, r *http.Request) error {
	// Parse the query for a comma-separated list of traces that we should only
	// show (all others are hidden).
	var showJust []appdash.ID
	if show := r.URL.Query().Get("show"); len(show) > 0 {
		for _, idStr := range strings.Split(show, ",") {
			id, err := appdash.ParseID(idStr)
			if err == nil {
				showJust = append(showJust, id)
			}
		}
	}

	traces, err := a.Queryer.Traces(appdash.TracesOpts{
		TraceIDs: showJust,
	})
	if err != nil {
		return err
	}

	return a.renderTemplate(w, r, "traces.html", http.StatusOK, &struct {
		TemplateCommon
		Traces  []*appdash.Trace
		Visible func(*appdash.Trace) bool
	}{
		Traces: traces,
		Visible: func(t *appdash.Trace) bool {
			return true
		},
	})
}

func (a *App) serveAggregate(w http.ResponseWriter, r *http.Request) error {
	// By default we display all traces.
	traces, err := a.Queryer.Traces(appdash.TracesOpts{})
	if err != nil {
		return err
	}

	q := r.URL.Query()

	// If they specified a comma-separated list of specific trace IDs that they
	// are interested in, then we only show those.
	selection := q.Get("selection")
	if len(selection) > 0 {
		var selected []*appdash.Trace
		for _, idStr := range strings.Split(selection, ",") {
			var id appdash.ID
			if id, err = appdash.ParseID(idStr); err != nil {
				return err
			}
			for _, t := range traces {
				if t.Span.ID.Trace == id {
					selected = append(selected, t)
				}
			}
		}
		traces = selected
	}

	// Perform the aggregation and render the data.
	aggregated, err := a.aggregate(traces, parseAggMode(q.Get("view-mode")))
	if err != nil {
		return err
	}
	return a.renderTemplate(w, r, "aggregate.html", http.StatusOK, &struct {
		TemplateCommon
		Aggregated []*aggItem
	}{
		Aggregated: aggregated,
	})
}

func (a *App) serveTraceUpload(w http.ResponseWriter, r *http.Request) error {
	// Read the uploaded JSON trace data.
	defer r.Body.Close()
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return err
	}

	// Unmarshal the trace.
	var traces []*appdash.Trace
	err = json.Unmarshal(data, &traces)
	if err != nil {
		return err
	}
	return a.uploadTraces(traces...)
}

// uploadTraces uploads literal traces into the storage system for later viewing.
func (a *App) uploadTraces(traces ...*appdash.Trace) error {
	// Collect the unmarshaled traces, ignoring any previously existing ones (i.e.
	// ones that would collide / be merged together).
	for _, trace := range traces {
		_, err := a.Store.Trace(trace.Span.ID.Trace)
		if err != appdash.ErrTraceNotFound {
			// The trace collides with an existing trace, ignore it.
			continue
		}

		// Collect the trace (store it for later viewing).
		if err = collectTrace(a.Store, trace); err != nil {
			return err
		}
	}
	return nil
}
