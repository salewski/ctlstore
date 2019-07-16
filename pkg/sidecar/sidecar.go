package sidecar

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/mux"
	"github.com/segmentio/ctlstore"
	"github.com/segmentio/errors-go"
	"github.com/segmentio/events"
	"github.com/segmentio/stats"
)

type (
	Sidecar struct {
		bindAddr string
		reader   Reader
		maxRows  int
		handler  http.Handler
	}
	Config struct {
		BindAddr string
		Reader   Reader
		MaxRows  int
	}
	Reader interface {
		GetRowByKey(ctx context.Context, out interface{}, familyName string, tableName string, key ...interface{}) (found bool, err error)
		GetRowsByKeyPrefix(ctx context.Context, familyName string, tableName string, key ...interface{}) (*ctlstore.Rows, error)
		GetLedgerLatency(ctx context.Context) (time.Duration, error)
	}
	ReadRequest struct {
		Key []Key
	}
	// Key represents a primary key segment.  The 'Value' field should be used unless the key segment is a
	// varbinary field.  This is so that the json unmarshaling will decode base64 for the Binary property.
	Key struct {
		Value  interface{}
		Binary []byte
	}
)

func (k Key) ToValue() interface{} {
	switch {
	case k.Binary != nil:
		return k.Binary
	default:
		return k.Value
	}
}

func keysToInterface(keys []Key) []interface{} {
	var res []interface{}
	for _, k := range keys {
		res = append(res, k.ToValue())
	}
	return res
}

func New(config Config) (*Sidecar, error) {
	sidecar := &Sidecar{
		bindAddr: config.BindAddr,
		reader:   config.Reader,
		maxRows:  config.MaxRows,
	}
	mux := mux.NewRouter()
	handleErr := func(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			err := fn(w, r)
			if err != nil {
				events.Log("err=%{error}s url=%{url}s", err, r.URL)
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		}
	}
	mux.HandleFunc("/get-row-by-key/{familyName}/{tableName}", handleErr(sidecar.getRowByKey)).Methods("POST")
	mux.HandleFunc("/get-rows-by-key-prefix/{familyName}/{tableName}", handleErr(sidecar.getRowsByKeyPrefix)).Methods("POST")
	mux.HandleFunc("/get-ledger-latency", handleErr(sidecar.getLedgerLatency)).Methods("GET")
	mux.HandleFunc("/healthcheck", handleErr(sidecar.healthcheck)).Methods("GET")
	mux.HandleFunc("/ping", handleErr(sidecar.ping)).Methods("GET")
	sidecar.handler = mux
	return sidecar, nil
}

func (s *Sidecar) Start(ctx context.Context) error {
	srv := &http.Server{
		Addr:         s.bindAddr,
		Handler:      s,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
		ErrorLog:     log.New(os.Stderr, "SRV ERR:", log.LstdFlags),
	}
	defer srv.Close()
	err := srv.ListenAndServe()
	return errors.Wrap(err, "listen and serve")
}

func (s *Sidecar) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// if we decide to move forward with sampling, we can add it to this func.
func (s *Sidecar) observeAPILatency(r *http.Request, op string) func() {
	start := time.Now()
	return func() {
		stats.Observe("api-latency", time.Now().Sub(start),
			stats.T("op", op),
			stats.T("user-agent", r.UserAgent()))
	}
}

func (s *Sidecar) getLedgerLatency(w http.ResponseWriter, r *http.Request) error {
	defer s.observeAPILatency(r, "get-ledger-latency")()
	duration, err := s.reader.GetLedgerLatency(r.Context())
	if err != nil {
		return errors.Wrap(err, "get ledger latency")
	}
	res := map[string]interface{}{
		"value": duration.Seconds(),
		"unit":  "seconds",
	}
	return json.NewEncoder(w).Encode(res)
}

func (s *Sidecar) healthcheck(w http.ResponseWriter, r *http.Request) error {
	defer s.observeAPILatency(r, "healthcheck")()

	_, err := s.reader.GetLedgerLatency(r.Context())
	return errors.Wrap(err, "healthcheck")
}

func (s *Sidecar) ping(w http.ResponseWriter, r *http.Request) error {
	defer s.observeAPILatency(r, "ping")()

	// for now, just hit the healthcheck. we can change this later.
	return s.healthcheck(w, r)
}

func (s *Sidecar) getRowsByKeyPrefix(w http.ResponseWriter, r *http.Request) error {
	defer s.observeAPILatency(r, "get-rows-by-key-prefix")()

	vars := mux.Vars(r)
	family := vars["familyName"]
	table := vars["tableName"]

	var rr ReadRequest
	err := json.NewDecoder(r.Body).Decode(&rr)
	if err != nil {
		return errors.Wrap(err, "decode body")
	}
	res := make([]interface{}, 0)
	rows, err := s.reader.GetRowsByKeyPrefix(r.Context(), family, table, keysToInterface(rr.Key)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		out := make(map[string]interface{})
		err = rows.Scan(out)
		if err != nil {
			return errors.Wrap(err, "scan")
		}
		res = append(res, out)
		if s.maxRows > 0 && len(res) > s.maxRows {
			return errors.Errorf("max row count (%d) exceeded", s.maxRows)
		}
	}
	err = rows.Err()
	if err != nil {
		return err
	}
	err = json.NewEncoder(w).Encode(res)
	return err
}

func (s *Sidecar) getRowByKey(w http.ResponseWriter, r *http.Request) error {
	defer s.observeAPILatency(r, "get-row-by-key")()

	vars := mux.Vars(r)
	family := vars["familyName"]
	table := vars["tableName"]

	var rr ReadRequest
	err := json.NewDecoder(r.Body).Decode(&rr)
	if err != nil {
		return errors.Wrap(err, "decode body")
	}

	out := make(map[string]interface{})
	found, err := s.reader.GetRowByKey(r.Context(), out, family, table, keysToInterface(rr.Key)...)
	if err != nil {
		return err
	}
	if !found {
		w.Header().Set("X-Ctlstore", "Not Found") // to differentiate between route based 404s
		w.WriteHeader(http.StatusNotFound)
		return nil
	}
	err = json.NewEncoder(w).Encode(out)
	return err
}