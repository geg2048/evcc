package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/core/site"
	"github.com/evcc-io/evcc/server/assets"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/encode"
	"github.com/evcc-io/evcc/util/jq"
	"github.com/evcc-io/evcc/util/logstash"
	"github.com/gorilla/mux"
	"github.com/itchyny/gojq"
	"golang.org/x/text/language"
	"gopkg.in/yaml.v3"
)

var ignoreState = []string{"releaseNotes"} // excessive size

// getPreferredLanguage returns the preferred language as two letter code
func getPreferredLanguage(header string) string {
	languages, _, err := language.ParseAcceptLanguage(header)
	if err != nil || len(languages) == 0 {
		return "en"
	}

	base, _ := languages[0].Base()
	return base.String()
}

func indexHandler(customCss bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")

		indexTemplate, err := fs.ReadFile(assets.Web, "index.html")
		if err != nil {
			log.FATAL.Print("httpd: failed to load embedded template:", err.Error())
			log.FATAL.Print("Make sure templates are included using the `release` build tag or use `make build`")
			w.WriteHeader(http.StatusNotFound)
			return
		}

		t, err := template.New("evcc").Delims("[[", "]]").Parse(string(indexTemplate))
		if err != nil {
			log.FATAL.Fatal("httpd: failed to create main page template:", err.Error())
		}

		defaultLang := getPreferredLanguage(r.Header.Get("Accept-Language"))

		if err := t.Execute(w, map[string]interface{}{
			"Version":     util.Version,
			"Commit":      util.Commit,
			"DefaultLang": defaultLang,
			"CustomCss":   customCss,
		}); err != nil {
			log.ERROR.Println("httpd: failed to render main page:", err.Error())
		}
	}
}

// jsonHandler is a middleware that decorates responses with JSON and CORS headers
func jsonHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		h.ServeHTTP(w, r)
	})
}

func jsonWrite(w http.ResponseWriter, content interface{}) {
	if err := json.NewEncoder(w).Encode(content); err != nil {
		log.ERROR.Printf("httpd: failed to encode JSON: %v", err)
	}
}

func jsonResult(w http.ResponseWriter, res interface{}) {
	jsonWrite(w, map[string]interface{}{"result": res})
}

func jsonError(w http.ResponseWriter, status int, err error) {
	w.WriteHeader(status)

	res := struct {
		Error string `json:"error"`
		Line  int    `json:"line,omitempty"`
	}{
		Error: err.Error(),
	}

	var (
		ype *yaml.ParserError
		yue yaml.UnmarshalError
	)
	switch {
	case errors.As(err, &ype):
		res.Line = ype.Line
	case errors.As(err, &yue):
		res.Line = yue.Line
	}

	jsonWrite(w, res)
}

func handler[T any](conv func(string) (T, error), set func(T) error, get func() T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)

		val, err := conv(vars["value"])
		if err == nil {
			err = set(val)
		}

		if err != nil {
			jsonError(w, http.StatusBadRequest, err)
			return
		}

		jsonResult(w, get())
	}
}

// ptrHandler updates pointer api
func ptrHandler[T any](conv func(string) (T, error), set func(*T) error, get func() *T) http.HandlerFunc {
	return handler(func(s string) (*T, error) {
		var val *T
		v, err := conv(s)
		if err == nil {
			val = &v
		} else if s == "" {
			err = nil
		}
		return val, err
	}, set, get)
}

// floatHandler updates float-param api
func floatHandler(set func(float64) error, get func() float64) http.HandlerFunc {
	return handler(parseFloat, set, get)
}

// floatPtrHandler updates float-pointer api
func floatPtrHandler(set func(*float64) error, get func() *float64) http.HandlerFunc {
	return ptrHandler(parseFloat, set, get)
}

// intHandler updates int-param api
func intHandler(set func(int) error, get func() int) http.HandlerFunc {
	return handler(strconv.Atoi, set, get)
}

// boolHandler updates bool-param api
func boolHandler(set func(bool) error, get func() bool) http.HandlerFunc {
	return handler(strconv.ParseBool, set, get)
}

// durationHandler updates duration-param api
func durationHandler(set func(time.Duration) error, get func() time.Duration) http.HandlerFunc {
	return handler(util.ParseDuration, set, get)
}

// getHandler returns api results
func getHandler[T any](get func() T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonResult(w, get())
	}
}

// updateSmartCostLimit sets the smart cost limit globally
func updateSmartCostLimit(site site.API, setLimit func(loadpoint.API, *float64)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		var val *float64

		if r.Method != http.MethodDelete {
			f, err := parseFloat(vars["value"])
			if err != nil {
				jsonError(w, http.StatusBadRequest, err)
				return
			}

			val = &f
		}

		for _, lp := range site.Loadpoints() {
			setLimit(lp, val)
		}

		jsonResult(w, val)
	}
}

// updateBatteryMode sets the external battery mode
func updateBatteryMode(site site.API) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		var val api.BatteryMode

		if r.Method != http.MethodDelete {
			s, err := api.BatteryModeString(vars["value"])
			if err != nil {
				jsonError(w, http.StatusBadRequest, err)
				return
			}

			val = s
		}

		site.SetBatteryModeExternal(val)

		jsonResult(w, site.GetBatteryModeExternal())
	}
}

// stateHandler returns the combined state
func stateHandler(cache *util.ParamCache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res := cache.State(encode.NewEncoder(encode.WithDuration()))
		for _, k := range ignoreState {
			delete(res, k)
		}

		if q := r.URL.Query().Get("jq"); q != "" {
			q = strings.TrimPrefix(q, ".result")

			query, err := gojq.Parse(q)
			if err != nil {
				jsonError(w, http.StatusBadRequest, err)
				return
			}

			b, err := json.Marshal(res)
			if err != nil {
				jsonError(w, http.StatusBadRequest, err)
				return
			}

			res, err := jq.Query(query, b)
			if err != nil {
				jsonError(w, http.StatusBadRequest, err)
				return
			}

			jsonWrite(w, res)
			return
		}

		jsonResult(w, res)
	}
}

// healthHandler returns current charge mode
func healthHandler(site site.API) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if site == nil || !site.Healthy() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "OK")
	}
}

// tariffHandler returns the configured tariff
func tariffHandler(site site.API) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		val := vars["tariff"]

		tariff, err := api.TariffUsageString(val)
		if err != nil {
			jsonError(w, http.StatusNotFound, err)
			return
		}

		t := site.GetTariff(tariff)
		if t == nil {
			jsonError(w, http.StatusNotFound, errors.New("tariff not available"))
			return
		}

		rates, err := t.Rates()
		if err != nil {
			jsonError(w, http.StatusNotFound, err)
			return
		}

		res := struct {
			Rates api.Rates `json:"rates"`
		}{
			Rates: rates,
		}

		jsonResult(w, res)
	}
}

// socketHandler attaches websocket handler to uri
func socketHandler(hub *SocketHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hub.ServeWebsocket(w, r)
	}
}

func logAreasHandler(w http.ResponseWriter, r *http.Request) {
	jsonResult(w, logstash.Areas())
}

func resetHandler(w http.ResponseWriter, r *http.Request) {
	util.ResetCached()
	jsonResult(w, "OK")
}

func logHandler(w http.ResponseWriter, r *http.Request) {
	a := r.URL.Query()["area"]
	l := logstash.LogLevelToThreshold(r.URL.Query().Get("level"))

	var count int
	if v := r.URL.Query().Get("count"); v != "" {
		count, _ = strconv.Atoi(v)
	}

	log := logstash.All(a, l, count)

	if r.URL.Query().Get("format") == "txt" {
		filename := "evcc-" + time.Now().Format("20060102-150405") + `-` + strings.ToLower(l.String()) + ".log"
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)

		for _, s := range log {
			if _, err := w.Write([]byte(s)); err != nil {
				return
			}
		}

		return
	}

	jsonResult(w, log)
}
